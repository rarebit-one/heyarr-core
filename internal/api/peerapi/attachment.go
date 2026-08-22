package peerapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
)

// ControlPlaneAttached is what a Full Peer's control plane is: the
// controller's (ADR-0029). It is a closed value rather than prose so an
// acceptance assertion can compare it exactly — a peer that read "attached"
// out of a sentence would keep passing when the sentence changed.
const ControlPlaneAttached = "controller-attached"

// maxAttachBody bounds the declaration a peer may send.
//
// The body carries one short identifier. A peer is authenticated, not trusted
// with this process's memory, and an authenticated caller streaming a gigabyte
// into a JSON decoder is the cheapest denial of service in any fabric.
const maxAttachBody = 4 << 10

// ErrNotTheActingPeer is a peer acting on another peer's resources.
//
// It is a distinct error rather than a generic refusal because it is the one
// refusal on this surface that is not about the transport at all: the caller
// authenticated perfectly, and then named somebody else.
var ErrNotTheActingPeer = errors.New("peerapi: the acting peer is taken from the certificate, and this request names another peer")

// Principal is the acting peer, derived from the client certificate.
//
// It is deliberately a different type from auth.Identity (ADR-0011), and it
// carries no scope, because there is no scope it could carry that would mean
// anything: a peer certificate authorises the peer surface, acting as that
// peer, and nothing else (ADR-0033). Anything wanting a *client* authority
// wants a bearer token on the other listener.
//
// The value it adds over passing an mtls.Peer around is [Principal.Authorises]
// — one place where "may this caller act on that peer?" is answered, so that
// M4-07's inventory report and everything after it cannot each answer it
// slightly differently.
type Principal struct {
	peer mtls.Peer
}

// PeerID is the acting peer's membership id, as this node derived it from the
// presented certificate. It never comes from a request body, a header or a
// path (ADR-0033).
func (p Principal) PeerID() string { return p.peer.PeerID }

// Name is the acting peer's name in this node's membership records.
func (p Principal) Name() string { return p.peer.Name }

// PublicKey is the pinned key this caller proved it holds.
func (p Principal) PublicKey() string { return identity.FormatPublicKey(p.peer.PublicKey) }

// Kind names what this principal is, for a log line that would otherwise read
// the same for a peer and for an operator's admin token.
func (p Principal) Kind() string { return "peer" }

// Authorises reports whether this peer may act on the named peer's resources.
//
// It is an equality, and it is meant to stay one. A peer's authority over
// another peer is not a smaller version of an operator's: there is none. A
// future "peer B may read peer A's inventory" is a controller decision
// (ADR-0029) expressed as something the controller sends, not as a widening
// here.
func (p Principal) Authorises(peerID string) error {
	if peerID == "" {
		return ErrNotTheActingPeer
	}
	if peerID != p.peer.PeerID {
		return ErrNotTheActingPeer
	}
	return nil
}

// PrincipalFrom returns the peer a request authenticated as.
//
// The second result is false on any request that did not pass through the peer
// surface's identity middleware — which is the only thing that ever puts a
// value here, and which derives it from the certificate.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := PeerFrom(ctx)
	if !ok {
		return Principal{}, false
	}
	return Principal{peer: p}, true
}

// AttachRequest is what a Full Peer sends when it attaches to a controller.
//
// PeerID is a DECLARATION, not a credential. It is what this peer believes it
// is, and the controller compares it against the identity the certificate
// proved — a mismatch is refused rather than obeyed, and a match changes
// nothing about who the acting peer is. That is the whole shape of ADR-0033's
// third rule, and it is worth a field rather than an absence: a peer restored
// from another site's configuration, or pointed at the wrong data directory,
// finds out on its first request instead of after its first inventory report
// has landed under somebody else's id.
type AttachRequest struct {
	// PeerID is the membership id this peer believes it holds. Required.
	PeerID string `json:"peer_id"`
}

// Attachment is what the controller answers: who the caller is, as the
// controller records it, and which controller answered.
type Attachment struct {
	// PeerID is the acting peer, derived from the certificate.
	PeerID string `json:"peer_id"`
	// Name is the acting peer's name in the controller's membership records.
	Name string `json:"name"`
	// PublicKey is the pinned key, as ed25519:<64 hex>.
	PublicKey string `json:"public_key"`
	// Controller is the answering node's own peer id, so a peer can tell which
	// controller it is attached to.
	Controller string `json:"controller"`
	// ControlPlane is always ControlPlaneAttached: a Full Peer runs none of
	// its own (ADR-0029). It is stated in the response so that a peer which
	// ever receives something else fails loudly rather than assuming.
	ControlPlane string `json:"control_plane"`
	// Principal is the kind of credential that was accepted — "peer", never
	// "admin". A peer reading its own attachment and finding an admin
	// principal would have found a privilege escalation (ADR-0033).
	Principal string `json:"principal"`
}

// handleAttachment answers GET /peer/v1/attachment.
//
// Everything in the response is derived from the certificate. There is no
// request body and no path parameter, which makes this the endpoint a peer
// uses to learn what the controller thinks it is without asserting anything.
func (s *Server) handleAttachment(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFrom(r.Context())
	if !ok {
		// The middleware is the only path here, so this is a wiring failure
		// rather than a request failure.
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	s.writeJSON(w, r, s.attachment(principal))
}

// handleAttach answers POST /peer/v1/attach.
//
// The peer declares which peer it believes it is; the controller answers with
// the peer the CERTIFICATE says it is. The declaration is compared and then
// discarded — it is never the acting identity, and the impersonation test
// exists to keep it that way.
func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFrom(r.Context())
	if !ok {
		httpapi.Fail(w, r, problem.Internal())
		return
	}

	var body AttachRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxAttachBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(
			"the attach body must be a JSON object with a peer_id: "+err.Error()))
		return
	}
	if body.PeerID == "" {
		// Not defaulted to the certificate's peer id. A body that may be
		// omitted is a body that stops being compared, and the comparison is
		// the only reason it exists.
		httpapi.Fail(w, r, problem.BadRequest(
			"peer_id is required: it is the peer this node believes it is, and the controller "+
				"compares it against the identity the certificate proved (ADR-0033)"))
		return
	}

	// The one line this endpoint is about. The acting peer is
	// principal.PeerID() — taken from the certificate — and the body is only
	// ever an argument to Authorises. Reading the acting identity out of the
	// body would authenticate every peer perfectly and then let any of them
	// act as any other.
	if err := principal.Authorises(body.PeerID); err != nil {
		s.log.Warn("refused a peer acting as another peer",
			"request_id", httpapi.RequestIDFrom(r.Context()),
			"acting_peer_id", principal.PeerID(),
			"declared_peer_id", body.PeerID)
		httpapi.Fail(w, r, problem.Forbidden(
			"this connection authenticated as peer "+principal.PeerID()+", and the request declares "+
				"peer "+body.PeerID+". The acting peer is taken from the certificate and never from "+
				"the request body (ADR-0033)"))
		return
	}

	s.log.Info("a peer attached",
		"peer_id", principal.PeerID(), "peer_name", principal.Name(), "controller", s.self)
	s.writeJSON(w, r, s.attachment(principal))
}

// attachment renders one principal's attachment. Both routes go through it so
// the declared-and-checked path and the derived-only path cannot answer with
// two different pictures of the same peer.
func (s *Server) attachment(p Principal) Attachment {
	return Attachment{
		PeerID:       p.PeerID(),
		Name:         p.Name(),
		PublicKey:    p.PublicKey(),
		Controller:   s.self,
		ControlPlane: ControlPlaneAttached,
		Principal:    p.Kind(),
	}
}
