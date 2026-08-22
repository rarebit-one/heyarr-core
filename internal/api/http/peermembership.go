package httpapi

import (
	"context"
	"crypto/ed25519"
	"net/http"

	"github.com/rarebit-one/heyarr-core/internal/api/problem"
)

// PeerMembership is the peer fabric's trust root, as the request path consults
// it (§26, ADR-0012, M4-04).
//
// It is an interface with one method rather than the membership store itself
// so that this package — the HTTP foundation everything mounts onto — does not
// import the peer plane to answer one question, and so that the question is
// the only thing the request path can ask. There is deliberately no "give me
// the whole membership list" method here: a handler that could fetch the list
// could hold onto it, and holding onto it is exactly the bug this guard exists
// to make impossible.
//
// internal/peer/membership.Store satisfies it.
type PeerMembership interface {
	// IsMember reports whether a public key is a member RIGHT NOW.
	//
	// Implementations must consult storage on every call. ADR-0012 makes
	// revocation the removal of a membership record, which means a stale
	// answer here is a peer that keeps reading bytes after an operator
	// revoked it — for a window nobody chose.
	IsMember(ctx context.Context, publicKey []byte) (bool, error)
}

// PeerLiveness records that a peer was heard from (§31, M4-10).
//
// It is a second, optional interface rather than a method on PeerMembership
// because the two answer questions that must not be confused. Membership is
// trust and is consulted on every request without exception — its freshness IS
// the revocation mechanism. Liveness is an observation, it may be throttled,
// and it may fail without consequence. Collapsing them into one interface is
// how somebody eventually adds a cache to "the peer interface" and caches the
// half that must never be cached.
//
// internal/peer/health.Tracker satisfies it.
type PeerLiveness interface {
	// Seen records that this public key's peer made a request. It must never
	// affect the outcome of that request.
	Seen(ctx context.Context, publicKey []byte) error
}

// PresentedPeerKey reports the peer public key a connection proved, and
// whether this is a peer connection at all.
//
// This is the seam M4-05 fills in. Today the only production extractor is
// TLSPresentedPeerKey; when mTLS lands, the certificate it verifies is the
// same certificate this reads, so the guard does not move.
//
// Returning false means "not a peer connection" — an ordinary client with a
// bearer token, a browser, the CLI — and the request proceeds on its bearer
// credential alone. Returning true means the caller has asserted a peer
// identity, and from that point membership is mandatory: there is no path
// where a presented key is looked at, found wanting, and let through anyway.
type PresentedPeerKey func(r *http.Request) ([]byte, bool)

// TLSPresentedPeerKey reads the Ed25519 public key out of the client
// certificate a peer presented.
//
// It reads PeerCertificates, which is what the TLS stack verified against the
// configured policy. This function does not decide whether the certificate is
// trusted — pinning it against a membership record is precisely what the guard
// below does with the key, and it is the only trust decision in the path
// (ADR-0012: no CA, no PKI).
func TLSPresentedPeerKey(r *http.Request) ([]byte, bool) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return nil, false
	}
	pub, ok := r.TLS.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok || len(pub) != ed25519.PublicKeySize {
		// A client certificate that is not an Ed25519 key cannot be a member
		// of this fabric, and reporting "not a peer connection" would let it
		// past the guard on its bearer token. Reporting a key that can never
		// match is the honest answer: it is a peer connection, and it is not a
		// member.
		return nil, true
	}
	return pub, true
}

// peerMembershipGuard refuses a request from a peer whose membership record is
// gone.
//
// # Why this is a per-request lookup and not a set
//
// ADR-0012: "Revocation is removing a membership record." There is no CRL, no
// short-lived token and no session teardown in this design, so the moment a
// removal takes effect is the moment the next lookup happens. Anything that
// remembers the answer — a map built at startup, a sync.Map warmed on first
// use, a five-second TTL — turns "revoked" into "revoked, eventually", on a
// connection that is already open and already streaming bytes.
//
// That is why the guard is a middleware over the whole authenticated API
// rather than a check inside the blob handler. A check that lives next to the
// bytes gets duplicated for the next route that serves bytes, and the copy is
// the one that caches.
func peerMembershipGuard(store PeerMembership, liveness PeerLiveness, presented PresentedPeerKey, log interface {
	Error(string, ...any)
},
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pub, isPeer := presented(r)
			if !isPeer {
				next.ServeHTTP(w, r)
				return
			}
			member, err := store.IsMember(r.Context(), pub)
			if err != nil {
				// Fail closed. An unavailable trust root is not permission:
				// the whole point of consulting it per request is that the
				// answer might have changed, and "I could not check" is not
				// "yes".
				log.Error("checking peer membership failed",
					"request_id", RequestIDFrom(r.Context()), "path", r.URL.Path, "error", err)
				Fail(w, r, problem.Internal())
				return
			}
			if !member {
				Fail(w, r, problem.Forbidden(
					"this peer is not a member of this fabric. Membership is the only trust root "+
						"in the inter-peer path (ADR-0012), and it is consulted on every request — "+
						"a removed peer loses access on the connection it is already using"))
				return
			}
			// The peer is a member and is talking to us, which is the best
			// evidence of liveness there is: a request it opened, as a side
			// effect of work it was doing anyway (§31, M4-10). Liveness is
			// OBSERVED, and this is the observation.
			//
			// It runs on a context detached from the request's, because the
			// fact is true whether or not the client stays to hear the answer
			// — a peer that disconnects mid-response was still up when it
			// asked. And a failure here is logged, never surfaced: recording
			// that somebody is alive must not be able to fail their request.
			if liveness != nil {
				if err := liveness.Seen(context.WithoutCancel(r.Context()), pub); err != nil {
					log.Error("recording that a peer was seen failed",
						"request_id", RequestIDFrom(r.Context()), "error", err)
				}
			}
			// Marked, not admitted. The guard still only ever subtracts: this
			// records that the caller reached the client API over a peer
			// credential, and the only thing anything downstream does with
			// that fact is refuse (ADR-0033 — a peer is not an admin). The
			// bearer token (ADR-0011) is as mandatory as it was.
			next.ServeHTTP(w, r.WithContext(markPeerConnection(r.Context())))
		})
	}
}

// markPeerConnection records that this request arrived over a connection that
// presented a peer client certificate.
func markPeerConnection(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyPeerConnection, true)
}

// PeerConnection reports whether a request reached the client API over a peer
// certificate (ADR-0012, ADR-0033).
//
// It exists so the admin surface can refuse such a request in ONE place. A
// peer certificate authenticates as that peer and authorises only the peer
// surface; it is not, and can never become, an admin credential. Asking this
// question route by route would work until the route somebody adds next month
// forgets to ask it, which is the whole reason the check lives in
// RequireScope rather than in six handlers.
func PeerConnection(ctx context.Context) bool {
	is, _ := ctx.Value(ctxKeyPeerConnection).(bool)
	return is
}
