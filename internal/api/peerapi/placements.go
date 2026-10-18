package peerapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/peer/inventory"
	"github.com/rarebit-one/heyarr-core/internal/peer/placementsync"
)

// PlacementSource answers a sibling's question about which blobs this node
// wants it to keep (ADR-0114).
//
// The askerPeerID argument is the acting peer, from the client certificate, and
// the request names no peer at all: peer ids are node-local, and the only
// identity this node can trust is the one the connection proved (ADR-0033).
// An implementation folds the asker's `held` claim, which is about the asker's
// own disk, and answers with what it offers that peer.
type PlacementSource interface {
	OfferPlacements(ctx context.Context, askerPeerID string, ask placementsync.Ask) (placementsync.Offer, error)
}

// maxPlacementAskBody bounds a question. It carries at most a held claim for a
// sixteenth of what the asker took from this node: the same bound, and the same
// reasoning, as an inventory report.
const maxPlacementAskBody = maxInventoryBody

// handlePlacements answers POST /peer/v1/placements.
//
// The ASKER is the destination: it calls the site that holds the bytes, which
// is the same direction its pull will travel, so between one-way-reachable
// sites the question and the fetch fail or succeed together (ADR-0038). This
// node never calls the asker to tell it anything.
//
// It is a POST only because the question carries a body. What it writes here is
// the asker's claim about its own disk, folded as an incremental inventory; it
// writes nothing about this node and nothing about any third peer.
func (s *Server) handlePlacements(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFrom(r.Context())
	if !ok {
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if s.placements == nil {
		httpapi.Fail(w, r, problem.New(http.StatusServiceUnavailable, problem.TypeInternal,
			"Service Unavailable", "this node offers no placements to siblings: only the personal "+
				"profile does (ADR-0107, ADR-0114)"))
		return
	}

	var ask placementsync.Ask
	dec := json.NewDecoder(io.LimitReader(r.Body, maxPlacementAskBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ask); err != nil {
		httpapi.Fail(w, r, problem.BadRequest("the body must be a placement question: "+err.Error()))
		return
	}
	if err := ask.Validate(); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	offer, err := s.placements.OfferPlacements(r.Context(), principal.PeerID(), ask)
	switch {
	case errors.Is(err, inventory.ErrUnknownPeer):
		httpapi.Fail(w, r, problem.Forbidden(
			"peer "+principal.PeerID()+" is a member of this fabric but has no catalog row, so "+
				"its replicas cannot be recorded"))
		return
	case errors.Is(err, inventory.ErrInvalidReport), errors.Is(err, placementsync.ErrInvalid):
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	case err != nil:
		s.log.Error("answering a placement question failed",
			"request_id", httpapi.RequestIDFrom(r.Context()),
			"peer_id", principal.PeerID(), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	s.writeJSON(w, r, offer)
}
