// Package vaultblob is the vault content INGEST path (ADR-0021, ADR-0096): a
// client uploads a pre-encrypted, content-addressed ciphertext blob, the peer
// verifies it hashes to the id the client declared, stores it, and records a
// placement pin so it is retained.
//
// It is deliberately its own path, separate from two others. It is not the
// plaintext scan-ingest pipeline (ADR-0014, reflink/hardlink/copy from a library
// root): a vault blob is bytes a client hands over, already encrypted. And it is
// not the read-only blobs GET contract (internal/api/blobs), which serves bytes
// and says nothing about them: this route WRITES, and it records the one thing
// that keeps a vault blob alive — a placement pin, because a vault blob has no
// `assets` row and the drive-CRDT reference that would keep it is encrypted state
// the control plane cannot see. Reads happen over the shared GET /blobs/{hash}
// route; only the write is vault-specific.
package vaultblob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rarebit-one/void-which-binds-go/hashing"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// defaultMaxBytes caps a single vault blob upload. Vault media is large by
// design (ADR-0021), so this is generous rather than tight; it exists so a
// runaway or hostile body cannot stream unbounded into the store. An upload
// truncated at the cap simply fails to hash to its declared id and is refused as
// a mismatch, so the cap needs no separate error path.
const defaultMaxBytes = int64(16) << 30 // 16 GiB

// ProvisionalTTL is how long an unconfirmed upload pin keeps its blob (ADR-0096
// amendment, #714): long enough for any writer to push its change and confirm,
// short enough that a write which never landed stops costing storage within a
// day.
const ProvisionalTTL = 24 * time.Hour

// maxConfirmBlobs bounds one confirm request. A write names two blobs (the
// manifest and the content); a sync engine batching many writes names more,
// and a thousand is generous for that.
const maxConfirmBlobs = 1000

// maxConfirmBody bounds the confirm request body: maxConfirmBlobs digests of
// 71 bytes each, quoted and comma-separated, with room to spare.
const maxConfirmBody = 128 << 10

// Store is the narrow slice of the CAS this handler needs: verify-against-expected
// ingest. *cas.FS (and the cas.Store interface) satisfies it.
type Store interface {
	PutExpecting(ctx context.Context, r io.Reader, expected hashing.Hash) (cas.Descriptor, error)
	// Stat confirms the bytes are still there when the upload is recorded,
	// because GC may reclaim a released blob of the same hash in between (#779).
	Stat(ctx context.Context, h hashing.Hash) (cas.Descriptor, error)
}

// Pinner records a stored vault blob in the catalogue — its blob row, this
// node's replica, and the placement pin that retains it (ADR-0096, #658) — and
// confirms upload pins once the change naming them is accepted (#714).
// *catalog.Catalog satisfies it.
//
// It is one call rather than a bare pin because a pin alone left the bytes
// unknown to the catalogue: convergence then queued a transfer of a blob this
// node already held, whose replica could never be recorded, and garbage
// collection saw untracked bytes (#658).
type Pinner interface {
	// present runs inside the recording transaction and confirms the bytes are
	// still stored; see catalog.Catalog.RecordVaultBlob.
	RecordVaultUpload(ctx context.Context, blobHash string, size int64, peerID string,
		pin catalog.UploadPin, present func(context.Context) error) error
	ConfirmUploadPins(ctx context.Context, peerID string, blobs []string, principal string,
		restricted bool) ([]catalog.ConfirmResult, error)
}

// Options configure a Handler.
type Options struct {
	// Store ingests the ciphertext, verifying it against the declared id. Required.
	Store Store
	// Pinner records the placement pin that keeps the blob. Required — an upload
	// with no pin would be reclaimed by GC, so the route is not mounted without it.
	Pinner Pinner
	// SelfPeer is this node's peer id, the pin's target on upload. Required.
	SelfPeer string
	// Logger records failures a client is not told about. Optional.
	Logger *slog.Logger
	// MaxBytes caps one upload body. Defaults to defaultMaxBytes when zero.
	MaxBytes int64
	// Grants lets a RESTRICTED caller upload only while it holds a write grant
	// on some space (ADR-0104). Optional; nil refuses every restricted caller.
	// Every other caller is unaffected.
	Grants httpapi.SpaceWriteGrants
	// Members lifts a read-floor Device credential to upload and confirm while
	// it is a writing member of some space (ADR-0128). Optional; nil keeps both
	// routes at the write scope, as before.
	Members httpapi.MemberWrites
	// ProvisionalPins makes an upload's pin provisional for ProvisionalTTL
	// until its writer confirms it (placement.provisional_upload_pins, #714).
	// Off — the default during rollout — every upload pin is permanent, as
	// before, for clients that predate confirm. Confirm is served either way.
	ProvisionalPins bool
	// Now is the clock a provisional pin's expiry is measured from. Nil uses
	// the wall clock (ADR-0017).
	Now func() time.Time
}

// Handler serves the vault-ingest route.
type Handler struct {
	store    Store
	pinner   Pinner
	selfPeer string
	log      *slog.Logger
	maxBytes int64
	grants   httpapi.SpaceWriteGrants
	members  httpapi.MemberWrites

	provisional bool
	now         func() time.Time
}

// New builds the handler, failing at construction if a dependency is missing
// rather than mounting a route that 500s or, worse, stores an un-pinned blob.
func New(opts Options) (*Handler, error) {
	if opts.Store == nil {
		return nil, errors.New("vaultblob: a CAS store is required")
	}
	if opts.Pinner == nil {
		return nil, errors.New("vaultblob: a pinner is required — an unpinned vault blob is reclaimed by GC")
	}
	if opts.SelfPeer == "" {
		return nil, errors.New("vaultblob: this node's peer id is required as the pin target")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	maxBytes := opts.MaxBytes
	if maxBytes == 0 {
		maxBytes = defaultMaxBytes
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Handler{
		store:    opts.Store,
		pinner:   opts.Pinner,
		selfPeer: opts.SelfPeer,
		log:      log.With("component", "vaultblob-api"),
		maxBytes: maxBytes,
		grants:   opts.Grants,
		members:  opts.Members,

		provisional: opts.ProvisionalPins,
		now:         now,
	}, nil
}

// uploadResult acks a stored vault blob. It names only the id and size — bytes
// are opaque ciphertext, and the server knows nothing else about them.
type uploadResult struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

// Mount registers the ingest route on the authenticated /api/v1 router. Storing a
// vault blob needs the `write` scope (ADR-0096: a device with write scope,
// ADR-0065/0067), or a device that is a writing member of some space
// (ADR-0128) — a blob names no space, so "some space" is the check.
func (h *Handler) Mount(r chi.Router) {
	gate := r.With(httpapi.RequireWriteOrMember(h.members, ""), httpapi.RequireRestrictedWriteGrant(h.grants))
	gate.Method(http.MethodPut, "/vault/blobs/{hash}", http.HandlerFunc(h.upload))
	// Confirming makes upload pins permanent once the change naming them is
	// accepted (ADR-0096 amendment, #714). Same gate as uploading: a restricted
	// executor, or a member's device, that may upload may confirm — and only
	// what its own principal uploaded.
	gate.Post("/vault/blobs/confirm", h.confirm)
}

// upload receives a client-encrypted, content-addressed ciphertext blob at the id
// the client declares in the path, verifies the bytes hash to it, stores it, and
// pins it to this node so GC retains it. The body is size-capped; a truncated or
// mismatching body fails verification and is a 400. The plaintext never exists
// here — the client encrypted it (ADR-0021).
func (h *Handler) upload(w http.ResponseWriter, r *http.Request) {
	expected, err := hashing.Parse(httpapi.HashParam(r))
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest("the blob id must be a blake3:<hex> digest: "+err.Error()))
		return
	}

	body := io.LimitReader(r.Body, h.maxBytes)
	desc, err := h.store.PutExpecting(r.Context(), body, expected)
	if err != nil {
		var corrupt *cas.Corruption
		if errors.As(err, &corrupt) {
			httpapi.Fail(w, r, problem.BadRequest(
				"the uploaded bytes do not hash to "+expected.String()+
					" — they were corrupted in transit or truncated at the size limit"))
			return
		}
		h.log.Error("storing a vault blob", "hash", expected.String(), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}

	// Record AFTER the bytes land: a pin for bytes that failed to store would be
	// a pin outliving its blob (ADR-0096). This node is the pin target — it
	// holds the bytes; cross-site placement is a device-supplied pin for another
	// peer. The size is the store's, of bytes it has just verified.
	//
	// The bytes are confirmed present inside the recording transaction: GC may
	// have reclaimed a released blob of the same hash after the store write
	// (#779). If so nothing is recorded and the client is told to retry, rather
	// than acknowledged for bytes that are not here.
	present := func(ctx context.Context) error {
		d, err := h.store.Stat(ctx, expected)
		if err != nil {
			return err
		}
		if d.Size != desc.Size {
			return fmt.Errorf("stored size %d, uploaded %d", d.Size, desc.Size)
		}
		return nil
	}
	// The pin records who uploaded the blob, and — with provisional pins on —
	// is kept only until its writer confirms it (#714).
	var pin catalog.UploadPin
	if id, ok := httpapi.IdentityFrom(r.Context()); ok {
		pin.Uploader = id.Principal.ID
	}
	if h.provisional {
		pin.ProvisionalUntil = h.now().Add(ProvisionalTTL)
	}
	if err := h.pinner.RecordVaultUpload(r.Context(), expected.String(), desc.Size, h.selfPeer, pin, present); err != nil {
		if errors.Is(err, catalog.ErrBytesGone) {
			httpapi.Fail(w, r, problem.ServiceUnavailable(
				"the blob was reclaimed while this upload was being recorded; upload it again"))
			return
		}
		h.log.Error("pinning a vault blob", "hash", expected.String(), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}

	httpapi.WriteJSON(w, r, h.log, http.StatusCreated, uploadResult{Hash: expected.String(), Size: desc.Size})
}

// confirmInput is the confirm request: the blob ids an accepted change names.
type confirmInput struct {
	Blobs []string `json:"blobs"`
}

// confirmResult is one blob's outcome. Status is confirmed, already_permanent,
// not_held (this node holds no upload pin for it) or not_uploader (a restricted
// caller naming a blob another principal uploaded).
type confirmResult struct {
	Blob   string `json:"blob"`
	Status string `json:"status"`
}

type confirmResponse struct {
	Results []confirmResult `json:"results"`
}

// confirm makes this node's upload pins on the named blobs permanent (ADR-0096
// amendment, #714). The writer calls it after the drive change naming them was
// accepted. It answers per blob, so one blob this node does not hold does not
// fail the others, and it is idempotent: confirming twice is the same as once.
func (h *Handler) confirm(w http.ResponseWriter, r *http.Request) {
	var in confirmInput
	if err := httpapi.DecodeJSON(w, r, &in, maxConfirmBody); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if len(in.Blobs) == 0 {
		httpapi.Fail(w, r, problem.BadRequest("blobs is required — name the blob ids the accepted change references"))
		return
	}
	if len(in.Blobs) > maxConfirmBlobs {
		httpapi.Fail(w, r, problem.BadRequest(fmt.Sprintf("at most %d blobs per confirm", maxConfirmBlobs)))
		return
	}
	for i, b := range in.Blobs {
		parsed, err := hashing.Parse(b)
		if err != nil {
			httpapi.Fail(w, r, problem.BadRequest("each blob must be a blake3:<hex> digest: "+err.Error()))
			return
		}
		in.Blobs[i] = parsed.String()
	}

	id, _ := httpapi.IdentityFrom(r.Context())
	// A member's device confirms only its own user's uploads, as an executor
	// confirms only its own (ADR-0128): a pin someone else took stays theirs.
	ownOnly := id.Restricted || httpapi.MemberLifted(r.Context())
	results, err := h.pinner.ConfirmUploadPins(r.Context(), h.selfPeer, in.Blobs, id.Principal.ID, ownOnly)
	if err != nil {
		h.log.Error("confirming upload pins", "count", len(in.Blobs), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	out := confirmResponse{Results: make([]confirmResult, 0, len(results))}
	for _, res := range results {
		out.Results = append(out.Results, confirmResult{Blob: res.BlobHash, Status: res.Status})
	}
	httpapi.WriteJSON(w, r, h.log, http.StatusOK, out)
}
