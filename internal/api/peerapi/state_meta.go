package peerapi

// state_meta.go adds the space and wrapped-key push routes to the personal-state
// sync surface (§37, §45, ADR-0049) — the metadata a replicating sibling sends so
// a Full Peer holds a space's identity and its wrapped keys alongside the changes
// it already syncs (state.go). Everything here is opaque: a space id and its
// structural kind, and wrapped-key bytes the peer cannot open. Nothing decrypts.

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
)

// statePutSpaceRequest records a replicated space: its structural kind (§39). The
// id is the route's {space} — opaque, a UUIDv7 the peer never interprets.
type statePutSpaceRequest struct {
	Kind string `json:"kind"`
}

// statePutWrappedKeyRequest is one recipient's sealed copy of a space key. Wrapped
// is opaque bytes (base64 on the wire); the peer stores it and cannot open it.
type statePutWrappedKeyRequest struct {
	Recipient string `json:"recipient"`
	Wrapped   []byte `json:"wrapped"`
}

// handleStatePutSpace records an encrypted space a sibling replicates to this
// peer. Idempotent: a space already held is a no-op success.
func (s *Server) handleStatePutSpace(w http.ResponseWriter, r *http.Request) {
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
	var req statePutSpaceRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxStateChangeBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpapi.Fail(w, r, problem.BadRequest("the space body must name a kind: "+err.Error()))
		return
	}
	if err := s.state.PutSpace(r.Context(), spaceID, req.Kind); err != nil {
		s.failState(w, r, principal, "accepting a replicated space", err)
		return
	}
	s.log.Info("accepted a replicated space", "peer_id", principal.PeerID(), "space", spaceID, "kind", req.Kind)
	w.WriteHeader(http.StatusNoContent)
}

// handleStatePutWrappedKey stores a wrapped copy of a space's key a sibling
// replicates. Idempotent per (space, recipient) — a re-push replaces the copy.
func (s *Server) handleStatePutWrappedKey(w http.ResponseWriter, r *http.Request) {
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
	var req statePutWrappedKeyRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxStateChangeBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpapi.Fail(w, r, problem.BadRequest("the wrapped-key body is malformed: "+err.Error()))
		return
	}
	if err := s.state.PutWrappedKey(r.Context(), spaceID, req.Recipient, req.Wrapped); err != nil {
		s.failState(w, r, principal, "accepting a replicated wrapped key", err)
		return
	}
	s.log.Info("accepted a replicated wrapped key", "peer_id", principal.PeerID(), "space", spaceID, "recipient", req.Recipient)
	w.WriteHeader(http.StatusNoContent)
}
