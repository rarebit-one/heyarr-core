package peerapi

// state_snapshots.go adds the snapshot leg of the personal-state sync protocol to
// the peer surface (§44): a sibling offers the latest snapshot it holds and
// accepts one pushed to it, so a Full Peer holds a bounded snapshot + tail rather
// than only the change log. Opaque like every other value here — the snapshot is
// ciphertext the peer never decrypts.

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
)

// stateSnapshotStored is the push ack: the id the peer verified and stored.
type stateSnapshotStored struct {
	SnapshotID string `json:"snapshot_id"`
}

// handleStateLatestSnapshot serves the newest snapshot a space has, or 404 when
// it has none yet (distinct from an unknown space — that path also 404s but for a
// different reason, so the log line names which).
func (s *Server) handleStateLatestSnapshot(w http.ResponseWriter, r *http.Request) {
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
	snap, has, err := s.state.LatestSnapshotFor(r.Context(), spaceID)
	if err != nil {
		s.failState(w, r, principal, "offering the latest snapshot", err)
		return
	}
	if !has {
		httpapi.Fail(w, r, problem.NotFound("this space has no snapshot"))
		return
	}
	s.log.Info("offered a snapshot", "peer_id", principal.PeerID(), "space", spaceID, "snapshot", snap.SnapshotID)
	s.writeJSON(w, r, snap)
}

// handleStateSnapshotPush accepts one opaque snapshot from a sibling, verifying
// its content-address before storage (Invariant 1), exactly as the change push
// does.
func (s *Server) handleStateSnapshotPush(w http.ResponseWriter, r *http.Request) {
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

	var snap protocol.EncryptedSnapshot
	dec := json.NewDecoder(io.LimitReader(r.Body, maxStateChangeBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&snap); err != nil {
		httpapi.Fail(w, r, problem.BadRequest("the snapshot body must be a JSON encrypted snapshot: "+err.Error()))
		return
	}
	if snap.SpaceID != spaceID {
		httpapi.Fail(w, r, problem.BadRequest("the snapshot's space_id must match the route's space id"))
		return
	}
	if err := snap.Validate(); err != nil {
		httpapi.Fail(w, r, problem.BadRequest("refusing the snapshot: "+err.Error()))
		return
	}
	if err := s.state.PutSnapshot(r.Context(), snap); err != nil {
		s.failState(w, r, principal, "accepting a snapshot", err)
		return
	}
	s.log.Info("accepted a snapshot", "peer_id", principal.PeerID(), "space", spaceID, "snapshot", snap.SnapshotID)
	s.writeJSON(w, r, stateSnapshotStored{SnapshotID: snap.SnapshotID})
}
