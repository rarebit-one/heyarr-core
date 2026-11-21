package peerapi

// state_trust.go is the peer side of snapshot trust (#681, #682, the ADR-0101
// amendment): a sibling pins its compaction base on this peer, and its audit
// asks which change ids this peer holds. Both are opaque causal metadata. The
// peer never decrypts a snapshot and cannot tell a genuine frontier from a
// relabelled one; it pins only what provably covers its own base.

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
)

// MaxHeldQuery bounds the ids one held query may carry. A canonical change id is
// 71 bytes and its JSON framing a few more, so this keeps a query well under
// maxStateChangeBody; a sibling auditing a longer log asks in batches.
const MaxHeldQuery = 4096

// stateHeldRequest is the audit's question: which of these change ids are held.
type stateHeldRequest struct {
	ChangeIDs []string `json:"change_ids"`
}

// StateBase names a pinned compaction base: its snapshot id and frontier.
type StateBase struct {
	SnapshotID string   `json:"snapshot_id"`
	Frontier   []string `json:"frontier"`
}

// stateHeldResponse answers it: the ids held as rows, in the order asked, and
// this peer's pinned base, whose causal history the asker counts as held too.
type stateHeldResponse struct {
	SpaceID string     `json:"space_id"`
	Held    []string   `json:"held"`
	Base    *StateBase `json:"base"`
}

// statePinBaseRequest names the snapshot a sibling pins as this peer's base.
type statePinBaseRequest struct {
	SnapshotID string `json:"snapshot_id"`
}

// statePinBaseResponse is the pin ack: the base, and whether this call moved it.
type statePinBaseResponse struct {
	SnapshotID string `json:"snapshot_id"`
	Pinned     bool   `json:"pinned"`
}

// handleStateHeld answers a sibling's audit: which of the offered change ids
// this peer holds, and the base it has pinned. The sibling records an
// acknowledgement from this answer, never from the heads this peer offers.
func (s *Server) handleStateHeld(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFrom(r.Context())
	if !ok {
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if s.state == nil {
		s.stateUnavailable(w, r)
		return
	}
	spaceID := chi.URLParam(r, "space")
	var req stateHeldRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxStateChangeBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpapi.Fail(w, r, problem.BadRequest("the held query must be a JSON list of change_ids: "+err.Error()))
		return
	}
	if len(req.ChangeIDs) > MaxHeldQuery {
		httpapi.Fail(w, r, problem.BadRequest("a held query may name at most 4096 change ids; ask in batches"))
		return
	}
	held, err := s.state.HeldChanges(r.Context(), spaceID, req.ChangeIDs)
	if err != nil {
		s.failState(w, r, principal, "confirming held changes", err)
		return
	}
	base, has, err := s.state.Base(r.Context(), spaceID)
	if err != nil {
		s.failState(w, r, principal, "reading the pinned base", err)
		return
	}
	resp := stateHeldResponse{SpaceID: spaceID, Held: held}
	if resp.Held == nil {
		resp.Held = []string{}
	}
	if has {
		frontier := base.Frontier
		if frontier == nil {
			frontier = []string{}
		}
		resp.Base = &StateBase{SnapshotID: base.SnapshotID, Frontier: frontier}
	}
	s.log.Debug("confirmed held changes", "peer_id", principal.PeerID(), "space", spaceID,
		"asked", len(req.ChangeIDs), "held", len(held))
	s.writeJSON(w, r, resp)
}

// handleStatePinBase pins a held snapshot as this peer's compaction base, the
// §45 compaction metadata a sibling replicates after the base snapshot itself.
// Idempotent; a pin that does not cover this peer's base is a 409 the sibling
// skips.
func (s *Server) handleStatePinBase(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFrom(r.Context())
	if !ok {
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if s.state == nil {
		s.stateUnavailable(w, r)
		return
	}
	spaceID := chi.URLParam(r, "space")
	var req statePinBaseRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxStateChangeBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.SnapshotID == "" {
		httpapi.Fail(w, r, problem.BadRequest("the base body must name a snapshot_id"))
		return
	}
	pinned, err := s.state.PinBase(r.Context(), spaceID, req.SnapshotID)
	if err != nil {
		s.failState(w, r, principal, "pinning a replicated base", err)
		return
	}
	if pinned {
		s.log.Info("pinned a replicated compaction base", "peer_id", principal.PeerID(), "space", spaceID, "snapshot", req.SnapshotID)
	}
	s.writeJSON(w, r, statePinBaseResponse{SnapshotID: req.SnapshotID, Pinned: pinned})
}
