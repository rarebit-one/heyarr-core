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
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// defaultMaxBytes caps a single vault blob upload. Vault media is large by
// design (ADR-0021), so this is generous rather than tight; it exists so a
// runaway or hostile body cannot stream unbounded into the store. An upload
// truncated at the cap simply fails to hash to its declared id and is refused as
// a mismatch, so the cap needs no separate error path.
const defaultMaxBytes = int64(16) << 30 // 16 GiB

// Store is the narrow slice of the CAS this handler needs: verify-against-expected
// ingest. *cas.FS (and the cas.Store interface) satisfies it.
type Store interface {
	PutExpecting(ctx context.Context, r io.Reader, expected hashing.Hash) (cas.Descriptor, error)
}

// Pinner records a placement pin that retains a blob on a peer (ADR-0096).
// *catalog.Catalog satisfies it.
type Pinner interface {
	PinPlacement(ctx context.Context, blobHash, peerID string) error
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
}

// Handler serves the vault-ingest route.
type Handler struct {
	store    Store
	pinner   Pinner
	selfPeer string
	log      *slog.Logger
	maxBytes int64
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
	return &Handler{
		store:    opts.Store,
		pinner:   opts.Pinner,
		selfPeer: opts.SelfPeer,
		log:      log.With("component", "vaultblob-api"),
		maxBytes: maxBytes,
	}, nil
}

// uploadResult acks a stored vault blob. It names only the id and size — bytes
// are opaque ciphertext, and the server knows nothing else about them.
type uploadResult struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

// Mount registers the ingest route on the authenticated /api/v1 router. Storing a
// vault blob needs the `write` scope (ADR-0096: a device with write scope, ADR-0065/0067).
func (h *Handler) Mount(r chi.Router) {
	r.With(httpapi.RequireScope(auth.ScopeWrite)).
		Method(http.MethodPut, "/vault/blobs/{hash}", http.HandlerFunc(h.upload))
}

// upload receives a client-encrypted, content-addressed ciphertext blob at the id
// the client declares in the path, verifies the bytes hash to it, stores it, and
// pins it to this node so GC retains it. The body is size-capped; a truncated or
// mismatching body fails verification and is a 400. The plaintext never exists
// here — the client encrypted it (ADR-0021).
func (h *Handler) upload(w http.ResponseWriter, r *http.Request) {
	expected, err := hashing.Parse(chi.URLParam(r, "hash"))
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

	// Pin AFTER the bytes land: a pin for bytes that failed to store would be a
	// pin outliving its blob (ADR-0096). This node is the pin target — it holds
	// the bytes; cross-site placement is a device-supplied pin for another peer.
	if err := h.pinner.PinPlacement(r.Context(), expected.String(), h.selfPeer); err != nil {
		h.log.Error("pinning a vault blob", "hash", expected.String(), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}

	h.write(w, r, http.StatusCreated, uploadResult{Hash: expected.String(), Size: desc.Size})
}

func (h *Handler) write(w http.ResponseWriter, r *http.Request, status int, body any) {
	buf, err := json.Marshal(body)
	if err != nil {
		h.log.Error("encoding a response failed",
			"request_id", httpapi.RequestIDFrom(r.Context()), "path", r.URL.Path, "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}
