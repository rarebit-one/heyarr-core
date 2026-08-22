package resources

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
)

// Peer enrolment (§26, ADR-0012, M4-04).
//
// Enrolment is operator-mediated and explicit. There is no discovery endpoint,
// no join token and no "register me" a peer can call on its own behalf — an
// operator reads the other node's public key out of `heyarr peers` at that
// site and posts it here, and the operator at the other site does the same in
// the other direction. Two nodes, two commands.
//
// These are the only two routes that change who this deployment trusts, which
// is why they are admin rather than write: a `write` token that could enrol a
// peer could hand the whole library to a machine of its choosing, and one that
// could remove a peer could sever a site.

// peerFromMember renders a membership record as the wire shape. It goes
// through the same struct the list endpoint scans into, so an enrolment
// response and a subsequent list cannot disagree about the same peer.
func peerFromMember(m membership.Member) Peer {
	p := Peer{
		ID:         m.PeerID,
		Name:       m.Name,
		Site:       m.Site,
		Mode:       m.Mode,
		IsSelf:     m.IsSelf,
		CreatedAt:  m.CreatedAt,
		EnrolledAt: m.EnrolledAt,
		Health:     m.Health,
	}
	if !m.LastSeenAt.IsZero() {
		lastSeen := m.LastSeenAt
		p.LastSeenAt = &lastSeen
	}
	if m.Endpoint != "" {
		endpoint := m.Endpoint
		p.Endpoint = &endpoint
	}
	if len(m.PublicKey) > 0 {
		rendered := identity.FormatPublicKey(m.PublicKey)
		p.PublicKey = &rendered
	}
	return p
}

// createPeerRequest is the POST /peers body.
//
// PublicKey is required and there is no variant of this shape without it. A
// body carrying only a name and an endpoint would be a request to trust
// whatever answers at that address — trust on first use — and the way to keep
// that from being built is to make it unspellable rather than to validate it
// away later.
type createPeerRequest struct {
	Name string `json:"name"`
	Site string `json:"site"`
	Mode string `json:"mode"`
	// Endpoint is where to reach the peer. It is not identity: posting the
	// same public key with a different endpoint moves it, and the peer's id,
	// its enrolment time and everything referring to it are unaffected.
	Endpoint string `json:"endpoint"`
	// PublicKey is who the peer is, as "ed25519:<64 hex characters>" — the
	// exact string the other site's `heyarr peers` prints.
	PublicKey string `json:"public_key"`
}

func (a *API) createPeer(w http.ResponseWriter, r *http.Request) {
	var body createPeerRequest
	if err := decodeJSON(w, r, &body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if err := required("name", body.Name); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}
	if err := required("public_key", body.PublicKey); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(
			err.Error()+"; a peer is registered by its public key, not by its address (ADR-0012)"))
		return
	}
	pub, err := identity.ParsePublicKey(body.PublicKey)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	result, err := a.membership.Register(r.Context(), membership.Registration{
		Name:     body.Name,
		Site:     body.Site,
		Mode:     body.Mode,
		Endpoint: body.Endpoint,
		// Never true from the API. The self peer is created by this node's own
		// startup (ADR-0010) and there is no operation that promotes another
		// machine to being this one.
		IsSelf:    false,
		PublicKey: pub,
	})
	if err != nil {
		a.failMembership(w, r, err)
		return
	}

	// 201 only for an actual enrolment. A re-registration that moved an
	// endpoint created nothing, and a client that treated 201 as "a new peer
	// joined" would count the same peer twice.
	if result.Transition == membership.TransitionEnrolled {
		w.Header().Set("Location", httpapi.APIPrefix+"/peers/"+result.Member.PeerID)
		a.write(w, r, http.StatusCreated, peerFromMember(result.Member))
		return
	}
	a.write(w, r, http.StatusOK, peerFromMember(result.Member))
}

// getPeer resolves by id or by name.
//
// Both, because an operator holds whichever of the two they were shown, and a
// lookup that accepted only the id would send them to the list first — which
// is the step that gets skipped when the thing being looked up is about to be
// removed.
func (a *API) getPeer(w http.ResponseWriter, r *http.Request) {
	m, err := a.membership.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		a.failMembership(w, r, err)
		return
	}
	a.write(w, r, http.StatusOK, peerFromMember(m))
}

// deletePeer revokes membership (ADR-0012).
//
// It returns the record it removed rather than 204, so that an operator — and
// the acceptance script — can see exactly which key stopped being trusted. A
// bare 204 makes "did I revoke the right peer?" a question answered by reading
// the list you just changed.
func (a *API) deletePeer(w http.ResponseWriter, r *http.Request) {
	m, err := a.membership.Remove(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		a.failMembership(w, r, err)
		return
	}
	a.write(w, r, http.StatusOK, peerFromMember(m))
}

// failMembership maps each refusal to the status that tells a client what to do
// about it. They are enumerated rather than collapsed into "400 on anything
// from the membership package": a name collision is fixable by choosing another
// name, a key collision means the operator pasted the wrong site's key, and
// removing self is not fixable at all.
func (a *API) failMembership(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, membership.ErrMalformedKey):
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
	case errors.Is(err, membership.ErrUnknownPeer):
		httpapi.Fail(w, r, problem.NotFound(err.Error()))
	case errors.Is(err, membership.ErrKeyRegistered),
		errors.Is(err, membership.ErrNameTaken),
		errors.Is(err, membership.ErrSelfExists),
		errors.Is(err, membership.ErrSelfRemoval):
		httpapi.Fail(w, r, problem.Conflict(err.Error()))
	default:
		a.fail(w, r, "peer", err)
	}
}
