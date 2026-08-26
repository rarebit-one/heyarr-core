package peerapi

import (
	"context"
	"net/http"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
)

// LeaseSource supplies the signed access-lease tokens this peer serves to a
// sibling to cache ahead of an outage (§54, ADR-0048). A sibling that fetches
// them can, once this peer is unreachable, still answer a read on whose
// authority — the degraded-read property (§53).
//
// nil is a legitimate state, not a wiring bug: a single-peer deployment issues
// to nobody, and the route then answers 503 honestly, exactly like every other
// optional capability on this surface.
type LeaseSource interface {
	// ActiveLeaseTokens returns the signed tokens worth caching: unrevoked, and
	// unexpired on the source's own clock.
	ActiveLeaseTokens(ctx context.Context) ([]string, error)
}

// leasesResponse is the peer-surface body. The tokens are opaque here on
// purpose: a fetching peer verifies each against the ISSUER's pinned key
// (ADR-0012), never against this peer that served them — a lease served by A is
// a statement signed by A, and it stays A's statement however it travelled, the
// same caution the fabric applies to a piece hash published by its server
// (ADR-0043).
type leasesResponse struct {
	Leases []string `json:"leases"`
}

// handleLeases serves this peer's active leases to an authenticated sibling, so
// the sibling can cache them before it needs them. It is a GET on the peer
// surface, behind requirePeerIdentity, so only a member reaches it — but the
// tokens carry their own authority regardless, which is why serving them to a
// member discloses nothing a member could not already be granted.
func (s *Server) handleLeases(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFrom(r.Context())
	if !ok {
		// requirePeerIdentity guarantees this; a handler that assumes it
		// silently is one that authorises everyone the day the middleware moves.
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if s.leases == nil {
		httpapi.Fail(w, r, problem.New(http.StatusServiceUnavailable, problem.TypeInternal,
			"Service Unavailable", "this node issues no access leases to cache; "+
				"leases come from a peer that authorises principals for its own holdings (§54, ADR-0048)"))
		return
	}

	tokens, err := s.leases.ActiveLeaseTokens(r.Context())
	if err != nil {
		s.log.Error("serving access leases failed",
			"request_id", httpapi.RequestIDFrom(r.Context()),
			"peer_id", principal.PeerID(), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if tokens == nil {
		// A nil slice would marshal as null; a caller decoding `leases` should
		// see [] for "none", not have to handle both.
		tokens = []string{}
	}
	s.log.Info("served access leases", "peer_id", principal.PeerID(), "count", len(tokens))
	s.writeJSON(w, r, leasesResponse{Leases: tokens})
}
