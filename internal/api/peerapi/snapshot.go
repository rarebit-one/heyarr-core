package peerapi

import (
	"context"
	"net/http"
	"strconv"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	peercatalog "github.com/rarebit-one/heyarr-core/internal/peer/catalog"
)

// SnapshotSource builds a catalog snapshot for one peer (§52, M4-13).
//
// It is an interface here rather than the concrete catalog for the reason the
// rest of this package gives about its trust root: the peer surface must be
// constructible with nothing behind it. The OpenAPI parity test walks this
// router with no database at all, and a node that is a peer rather than a
// controller serves this surface while having no catalogue to snapshot. Both
// need the route to exist and to refuse honestly.
type SnapshotSource interface {
	// BuildSnapshot returns the next snapshot for peerID. holding is the
	// version the peer says it already has, and full forces the drift-
	// correcting rebuild.
	BuildSnapshot(ctx context.Context, peerID string, holding int64, full bool) (*peercatalog.Snapshot, error)
}

// handleCatalogSnapshot answers GET /peer/v1/catalog/snapshot.
//
// # The peer in the response is the certificate's, always
//
// There is no peer id in the path and none in the query, and that is the same
// rule ADR-0033 states for attachment: the acting peer comes from the
// certificate. A snapshot is a complete description of the library's
// organisation, so a route that let a caller name a different peer would let
// any member read a snapshot issued to any other — and then quietly advance
// that other peer's version, so the real peer's next incremental refresh would
// skip everything in between.
//
// # ?holding= is a claim, not a credential
//
// It says what the peer believes it has, and the controller decides what to do
// about it. A peer claiming a version the controller never issued gets a full
// rebuild rather than an argument, because the conservative answer to "we
// disagree about what you have" is to send everything.
func (s *Server) handleCatalogSnapshot(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFrom(r.Context())
	if !ok {
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if s.snapshots == nil {
		// Not a 500. This node genuinely has no catalogue to snapshot — it is
		// a peer, not a controller (ADR-0029) — and saying so is more useful
		// than an internal error, which would send an operator looking for a
		// bug in the controller they are not talking to.
		httpapi.Fail(w, r, problem.New(http.StatusServiceUnavailable, problem.TypeInternal,
			"No Catalog", "this node serves the peer fabric but holds no catalogue to snapshot; "+
				"a catalog snapshot comes from the controller a Full Peer is attached to (ADR-0029)"))
		return
	}

	var holding int64
	if raw := r.URL.Query().Get("holding"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			httpapi.Fail(w, r, problem.BadRequest(
				"holding must be a non-negative integer: it is the snapshot version this peer "+
					"already has, and zero means it has none"))
			return
		}
		holding = v
	}
	full := r.URL.Query().Get("full") == "true"

	snap, err := s.snapshots.BuildSnapshot(r.Context(), principal.PeerID(), holding, full)
	if err != nil {
		s.log.Error("building a catalog snapshot failed",
			"request_id", httpapi.RequestIDFrom(r.Context()),
			"peer_id", principal.PeerID(), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if snap == nil {
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if err := snap.Meta.Validate(); err != nil {
		// A malformed snapshot is refused here rather than shipped, because
		// the peer would refuse it anyway and the useful place for that
		// failure to be recorded is the end that built it.
		s.log.Error("refused to serve a malformed catalog snapshot",
			"request_id", httpapi.RequestIDFrom(r.Context()),
			"peer_id", principal.PeerID(), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}

	s.log.Info("served a catalog snapshot",
		"peer_id", principal.PeerID(), "version", snap.Meta.Version,
		"kind", snap.Meta.Kind, "rows", snap.Rows())
	s.writeJSON(w, r, snap)
}
