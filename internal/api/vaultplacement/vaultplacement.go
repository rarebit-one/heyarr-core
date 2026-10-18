// Package vaultplacement is the DEVICE-facing side of cross-site placement pins
// (ADR-0096): the /api/v1 routes a device calls to say a ciphertext vault blob
// should live on a peer OTHER than the one it uploaded to, and to take that back.
//
// It is the companion to internal/api/vaultblob. That route stores a vault blob
// and pins it to THIS node so garbage collection retains it locally; this route
// records a pin naming ANOTHER peer, which is what makes the blob a replication
// destination for that peer (the convergence union in
// internal/persistence/catalog unions the pins on top of the canonical-set diff).
//
// A pin is deliberately OPAQUE — a (blob, peer) pair and nothing else. It carries
// no space, no path and no asset link, so the control plane learns that a blob
// belongs on a peer and never which vault or file it is (Invariant 6). The device,
// which holds the drive CRDT the pin reflects, is the only thing that knows what
// the blob is for; it pins when its retention policy says the blob should live on
// a peer and unpins when that policy lets the blob go.
package vaultplacement

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/rarebit-one/void-which-binds-go/hashing"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/auth"
)

// Pinner records and removes opaque placement pins (ADR-0096).
// *catalog.Catalog satisfies it.
type Pinner interface {
	PinPlacement(ctx context.Context, blobHash, peerID string) error
	UnpinPlacement(ctx context.Context, blobHash, peerID string) error
}

// Status is where a blob is wanted and where it is known to be held
// (ADR-0114): what a client waits on before it treats a blob as safe at both
// sites.
type Status struct {
	// PinnedTo is every peer a placement names for the blob, in this node's
	// peer ids: device pins, plus this node's siblings when it is pinned here.
	PinnedTo []string
	// HeldAt is every Full Peer recorded as holding the blob's bytes.
	HeldAt []string
	// TakenFrom is the sibling sites this node took the placement from.
	TakenFrom []string
	// Sites is how many distinct sites hold the bytes (failure domains, not
	// peers).
	Sites int
}

// StatusReader reads a blob's placement status. Optional on Options: without
// one the status route answers 503.
type StatusReader interface {
	PlacementStatus(ctx context.Context, blobHash string) (Status, error)
}

// Options configure a Handler.
type Options struct {
	// Pinner records and removes the pins. Required — the routes are nothing
	// without it, so the handler is not constructible when it is missing rather
	// than mounting routes that 500 on the first call.
	Pinner Pinner
	// Logger records failures a client is not told about. Optional.
	Logger *slog.Logger
	// Grants lets a RESTRICTED caller record a pin only while it holds a write
	// grant on some space (ADR-0104). Optional; nil refuses every restricted
	// caller. Every other caller is unaffected.
	Grants httpapi.SpaceWriteGrants
	// Status reads a blob's placement status for GET
	// /vault/blobs/{hash}/placement (ADR-0114). Optional.
	Status StatusReader
}

// Handler serves the device-facing placement-pin routes.
type Handler struct {
	pinner Pinner
	log    *slog.Logger
	grants httpapi.SpaceWriteGrants
	status StatusReader
}

// New builds the handler, failing at construction if the pinner is missing.
func New(opts Options) (*Handler, error) {
	if opts.Pinner == nil {
		return nil, errors.New("vaultplacement: a pinner is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Handler{pinner: opts.Pinner, log: log.With("component", "vaultplacement-api"), grants: opts.Grants, status: opts.Status}, nil
}

// Mount registers the routes on the authenticated /api/v1 router. Recording or
// removing a placement pin needs the `write` scope, exactly as storing a vault
// blob does (ADR-0096: a device with write scope) — a pin changes where content
// is kept, which is a write about the fabric even though it moves no bytes here.
func (h *Handler) Mount(r chi.Router) {
	// A restricted caller (ADR-0104) may add a pin while it holds a write grant,
	// so the content it pushes is placed like anyone's; it may never remove one,
	// because unpinning is how a blob becomes eligible for collection.
	r.With(httpapi.RequireScope(auth.ScopeWrite), httpapi.RequireRestrictedWriteGrant(h.grants)).Post("/vault/placements", h.pin)
	r.With(httpapi.RefuseRestricted, httpapi.RequireScope(auth.ScopeWrite)).Delete("/vault/placements", h.unpin)
	// Where a blob is wanted and held (ADR-0114). Read scope, and the same
	// restricted-caller gate as pinning: a writer that may place a blob may
	// ask whether its placement has landed.
	r.With(httpapi.RequireScope(auth.ScopeRead), httpapi.RequireRestrictedWriteGrant(h.grants)).
		Get("/vault/blobs/{hash}/placement", h.placementStatus)
}

// statusResult is the wire shape of the placement status. sites_durable is how
// many distinct sites hold the bytes, counting another peer only while its claim
// is fresh: a "move" lane deletes a device's own copy only once it is at least
// the number of sites it wants (ADR-0114).
type statusResult struct {
	BlobHash     string   `json:"blob_hash"`
	PinnedTo     []string `json:"pinned_to"`
	HeldAt       []string `json:"held_at"`
	TakenFrom    []string `json:"taken_from"`
	SitesDurable int      `json:"sites_durable"`
}

// placementStatus answers GET /vault/blobs/{hash}/placement.
func (h *Handler) placementStatus(w http.ResponseWriter, r *http.Request) {
	if h.status == nil {
		httpapi.Fail(w, r, problem.New(http.StatusServiceUnavailable, problem.TypeInternal,
			"Service Unavailable", "this node reports no placement status"))
		return
	}
	hash, err := hashing.Parse(chi.URLParam(r, "hash"))
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest("the path must name a blake3:<hex> digest: "+err.Error()))
		return
	}
	st, err := h.status.PlacementStatus(r.Context(), hash.String())
	if err != nil {
		h.log.Error("reading a placement status", "blob_hash", hash.String(), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	httpapi.WriteJSON(w, r, h.log, http.StatusOK, statusResult{
		BlobHash: hash.String(), PinnedTo: nonNil(st.PinnedTo), HeldAt: nonNil(st.HeldAt),
		TakenFrom: nonNil(st.TakenFrom), SitesDurable: st.Sites,
	})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// placementInput is the wire shape of both routes: the opaque (blob, peer) pair.
// The pin is identified by the pair itself — there is no server-minted id, because
// a pin carries no state a device would need to name it back by.
type placementInput struct {
	BlobHash string `json:"blob_hash"`
	PeerID   string `json:"peer_id"`
}

// placementResult acks a recorded pin. It echoes the pair rather than a new id:
// the pin IS the pair, and a device unpins by the same two fields it pinned with.
type placementResult struct {
	BlobHash string `json:"blob_hash"`
	PeerID   string `json:"peer_id"`
}

// pin records that a vault blob should live on a peer (ADR-0096). It validates the
// blob id is a blake3 digest — the same shape vaultblob enforces on upload — and
// that a peer is named, but it does NOT check the peer exists or is a Full Peer:
// a pin is opaque and may outlive churn in `peers`, and a pin naming a peer that
// is not a replication target is simply ignored by convergence (migration 00050).
// Idempotent per (blob, peer): re-pinning is a 200, not a conflict.
func (h *Handler) pin(w http.ResponseWriter, r *http.Request) {
	in, ok := h.decode(w, r)
	if !ok {
		return
	}
	if err := h.pinner.PinPlacement(r.Context(), in.BlobHash, in.PeerID); err != nil {
		h.log.Error("recording a placement pin",
			"blob_hash", in.BlobHash, "peer_id", in.PeerID, "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	httpapi.WriteJSON(w, r, h.log, http.StatusOK, placementResult(in))
}

// unpin removes a placement pin, so the peer is no longer a replication target for
// the blob and, where the pin was that blob's last reference, garbage collection
// may reclaim it (ADR-0096). Idempotent: removing a pin that is not there is a 204,
// not a 404 — the device's intent (this blob should NOT live on this peer) is
// satisfied either way.
func (h *Handler) unpin(w http.ResponseWriter, r *http.Request) {
	in, ok := h.decode(w, r)
	if !ok {
		return
	}
	if err := h.pinner.UnpinPlacement(r.Context(), in.BlobHash, in.PeerID); err != nil {
		h.log.Error("removing a placement pin",
			"blob_hash", in.BlobHash, "peer_id", in.PeerID, "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decode reads and validates the (blob, peer) pair both routes take, writing the
// failure and returning false when it is malformed.
func (h *Handler) decode(w http.ResponseWriter, r *http.Request) (placementInput, bool) {
	var in placementInput
	if err := httpapi.DecodeJSON(w, r, &in, maxRequestBody); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return placementInput{}, false
	}
	if _, err := hashing.Parse(in.BlobHash); err != nil {
		httpapi.Fail(w, r, problem.BadRequest("blob_hash must be a blake3:<hex> digest: "+err.Error()))
		return placementInput{}, false
	}
	if in.PeerID == "" {
		httpapi.Fail(w, r, problem.BadRequest("peer_id is required — a placement pin names the peer a blob should live on"))
		return placementInput{}, false
	}
	return in, true
}
