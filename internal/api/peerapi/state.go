package peerapi

// state.go is the encrypted personal-state sync protocol on the peer surface
// (§42, §44, ADR-0049) — a SEPARATE protocol from CAS sync that shares this
// listener and its mTLS authentication but nothing of its blob-optimised shape.
// It moves opaque `{space_id, change_id, parents, ciphertext}` changes: a peer
// offers its causal heads, pulls the changes it is missing beneath them, and
// pushes changes it holds. The peer NEVER decrypts or merges a change — the
// semantic merge is client-side (§42, Invariant 6).
//
// The structural guarantee that makes "the peer reads a change" unspellable is
// this file's imports: it depends on internal/personalstate/protocol (the opaque
// wire type and its pure DAG reconciliation) and NOT on the plaintext CRDT model
// (internal/personalstate/crdt) or the device-side decrypt path
// (internal/personalstate/client, .../statesync). A depguard rule and a boundary
// test both hold that line, so a "merge helper" that peeks does not compile.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
)

// maxStateChangeBody bounds a pushed change. A change is a small encrypted CRDT
// delta, not a blob (that is CAS sync's job), so a megabyte is generous — and an
// unbounded decode is a memory-exhaustion primitive even behind mTLS.
const maxStateChangeBody = 1 << 20

// ErrNoSuchSpace is a state operation naming a space this peer does not hold. It
// is declared here, not imported from the store, because this package does not
// import persistence (like every backend on this surface): the wiring adapter
// translates its store's not-found into this sentinel, and the handler maps it to
// a 404 without ever depending on how the peer stores anything.
var ErrNoSuchSpace = errors.New("peerapi: no such encrypted space")

// ErrInvalidState is a replicated space or wrapped key the store refused as
// malformed — an unknown space kind, an empty recipient, or empty wrapped bytes.
// Like ErrNoSuchSpace it is declared here so the handler maps a push the store
// rejects to a 400 without this package importing persistence: the wiring adapter
// translates the store's validation sentinels into this one.
var ErrInvalidState = errors.New("peerapi: invalid personal-state push")

// StateStore is the peer's opaque personal-state storage, as this surface needs
// it: the causal heads of a space, its changes (all, or those a caller is
// missing), and a sink that accepts a change after verifying its content-address.
// Every value it moves is ciphertext and opaque causal metadata; nothing here
// decrypts.
//
// nil is a legitimate state — a node holding no personal state still MOUNTS the
// routes and answers 503, exactly like every other optional capability here.
type StateStore interface {
	// HeadsFor returns a space's causal frontier. ErrNoSuchSpace if not held.
	HeadsFor(ctx context.Context, spaceID string) ([]string, error)
	// ChangesFor returns every change held for a space, oldest first.
	// ErrNoSuchSpace if not held.
	ChangesFor(ctx context.Context, spaceID string) ([]protocol.EncryptedChange, error)
	// PutChange stores a change after verifying its content-addressed id. It is
	// idempotent, and refuses a change whose id does not match its bytes.
	// ErrNoSuchSpace if the space is not held here.
	PutChange(ctx context.Context, ch protocol.EncryptedChange) error
	// PutSpace records an encrypted space a replicating sibling pushes — the
	// opaque id and its structural kind (§37, §45). Idempotent; a space already
	// held is a no-op. The kind is a known §39 category, refused otherwise.
	PutSpace(ctx context.Context, spaceID, kind string) error
	// PutWrappedKey stores a wrapped copy of a space's key a sibling pushes. The
	// bytes are opaque — the peer holds them and cannot open them. Idempotent per
	// (space, recipient). ErrNoSuchSpace if the space is not held here.
	PutWrappedKey(ctx context.Context, spaceID, recipient string, wrapped []byte) error
	// LatestSnapshotFor returns the newest snapshot held for a space, and whether
	// one exists (§44). ok is false — with a nil error — when the space is held but
	// has no snapshot yet. ErrNoSuchSpace if the space itself is not held.
	LatestSnapshotFor(ctx context.Context, spaceID string) (protocol.EncryptedSnapshot, bool, error)
	// PutSnapshot stores an encrypted snapshot after verifying its content-address
	// (§44). Idempotent on the id. ErrNoSuchSpace if the space is not held here.
	PutSnapshot(ctx context.Context, snap protocol.EncryptedSnapshot) error
}

// stateHeadsResponse is the offer side: the space's causal heads.
type stateHeadsResponse struct {
	SpaceID string   `json:"space_id"`
	Heads   []string `json:"heads"`
}

// stateChangesResponse is the pull side: the opaque changes, ciphertext and all.
type stateChangesResponse struct {
	SpaceID string                     `json:"space_id"`
	Changes []protocol.EncryptedChange `json:"changes"`
}

// stateChangeStored is the push ack: the id the peer verified and stored.
type stateChangeStored struct {
	ChangeID string `json:"change_id"`
}

// handleStateHeads offers a space's causal heads to a sibling, so it can compute
// what it is missing and pull only that (the incremental, resumable sync a phone
// needs — §46, #330). Read-only, opaque: a head is an opaque change id.
func (s *Server) handleStateHeads(w http.ResponseWriter, r *http.Request) {
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
	heads, err := s.state.HeadsFor(r.Context(), spaceID)
	if err != nil {
		s.failState(w, r, principal, "offering state heads", err)
		return
	}
	if heads == nil {
		heads = []string{}
	}
	s.log.Info("offered state heads", "peer_id", principal.PeerID(), "space", spaceID, "count", len(heads))
	s.writeJSON(w, r, stateHeadsResponse{SpaceID: spaceID, Heads: heads})
}

// handleStateChanges serves the changes a caller is missing. If the caller offers
// its known heads as repeated `have` query parameters, only the changes beneath
// this peer's frontier that those heads do not cover are returned
// (protocol.Missing — pure DAG reachability, no decryption); with no `have`, the
// whole set is served. The bias is toward sending too much, never too little: a
// duplicate change is idempotent under merge, a missing one stalls convergence.
func (s *Server) handleStateChanges(w http.ResponseWriter, r *http.Request) {
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
	changes, err := s.state.ChangesFor(r.Context(), spaceID)
	if err != nil {
		s.failState(w, r, principal, "serving state changes", err)
		return
	}
	if have := r.URL.Query()["have"]; len(have) > 0 {
		changes = protocol.Missing(changes, have)
	}
	if changes == nil {
		changes = []protocol.EncryptedChange{}
	}
	s.log.Info("served state changes", "peer_id", principal.PeerID(), "space", spaceID, "count", len(changes))
	s.writeJSON(w, r, stateChangesResponse{SpaceID: spaceID, Changes: changes})
}

// handleStateChangePush accepts one opaque change from a sibling. It verifies the
// content-address itself (a peer never trusts a claimed id, Invariant 1) before
// handing it to the store, which verifies it again — belt and suspenders on the
// one property that lets two peers agree on a change id without reading it.
func (s *Server) handleStateChangePush(w http.ResponseWriter, r *http.Request) {
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

	var ch protocol.EncryptedChange
	dec := json.NewDecoder(io.LimitReader(r.Body, maxStateChangeBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ch); err != nil {
		httpapi.Fail(w, r, problem.BadRequest("the change body must be a JSON encrypted change: "+err.Error()))
		return
	}
	if ch.SpaceID != spaceID {
		httpapi.Fail(w, r, problem.BadRequest("the change's space_id must match the route's space id"))
		return
	}
	// Verify the content-address before storage: a forged id, a re-pointed space
	// or tampered ciphertext is refused here as a 400, not passed down as a 500.
	if err := ch.Validate(); err != nil {
		httpapi.Fail(w, r, problem.BadRequest("refusing the change: "+err.Error()))
		return
	}
	if err := s.state.PutChange(r.Context(), ch); err != nil {
		s.failState(w, r, principal, "accepting a state change", err)
		return
	}
	s.log.Info("accepted a state change", "peer_id", principal.PeerID(), "space", spaceID, "change", ch.ChangeID)
	s.writeJSON(w, r, stateChangeStored{ChangeID: ch.ChangeID})
}

// stateUnavailable is the 503 a node with no personal-state store answers, so the
// route stays mounted for the OpenAPI parity walk yet refuses honestly.
func (s *Server) stateUnavailable(w http.ResponseWriter, r *http.Request) {
	httpapi.Fail(w, r, problem.New(http.StatusServiceUnavailable, problem.TypeInternal,
		"Service Unavailable", "this node keeps no encrypted personal state to sync (§42, §44, ADR-0049)"))
}

// failState maps a state error: an unknown space is a 404 (the space is not
// replicated here yet), a content-address mismatch a 400, anything else a 500 the
// caller cannot act on and the log carries.
func (s *Server) failState(w http.ResponseWriter, r *http.Request, principal Principal, doing string, err error) {
	switch {
	case errors.Is(err, ErrNoSuchSpace):
		httpapi.Fail(w, r, problem.NotFound("this peer does not hold that encrypted space"))
	case errors.Is(err, protocol.ErrIDMismatch), errors.Is(err, protocol.ErrIncomplete), errors.Is(err, ErrInvalidState),
		errors.Is(err, protocol.ErrSnapshotIDMismatch), errors.Is(err, protocol.ErrSnapshotIncomplete):
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
	default:
		s.log.Error(doing+" failed",
			"request_id", httpapi.RequestIDFrom(r.Context()),
			"peer_id", principal.PeerID(), "error", err)
		httpapi.Fail(w, r, problem.Internal())
	}
}
