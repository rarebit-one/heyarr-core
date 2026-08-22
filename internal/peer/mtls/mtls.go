// Package mtls is the peer-to-peer transport: self-signed certificates over
// the Ed25519 peer identity, pinned to membership (§26, ADR-0012, M4-05).
//
// # There is no CA, and that is the decision
//
// ADR-0012: "Peers authenticate to each other with mTLS using self-signed
// certificates whose public keys are pinned by the controller-issued peer
// membership record. No CA, no PKI, no public certificate authority in the
// inter-peer path."
//
// So the certificate is not evidence of anything. It is a container for a
// public key and a proof that the far end holds the matching private half —
// which is what the TLS handshake establishes and the only thing it
// establishes here. Everything that decides whether to talk to that key is
// [Membership], consulted at the moment of the handshake and again on every
// request the connection carries.
//
// # Why this is not a thin wrapper over http.Client
//
// ADR-0012's consequence, in full: "Heyarr is responsible for its own
// transport security and must be safe over a hostile network. In particular it
// must not treat an existing site-to-site VPN as its security boundary —
// tunnels get reconfigured, and their crypto ages."
//
// A pinning check that has never refused anything is decoration. In every
// environment anybody develops in, both ends are honest and the tunnel is up,
// so the code that refuses is the code that never runs. That is why the
// refusals in this package are separate, named errors rather than one
// "handshake failed", why each has its own test, and why [AssertPinned] exists
// at all.
//
// # The mistake this package is shaped to make loud
//
// Go's TLS verification is easy to satisfy by accident. Because there is no CA
// to chain to, the correct shape is InsecureSkipVerify with a hand-rolled
// VerifyPeerCertificate — and that is one careless edit away from
// InsecureSkipVerify and nothing else, which passes every happy-path test in
// existence and authenticates nobody.
//
// The countermeasure is structural: [ServerConfig] and [ClientConfig] are the
// only constructors of a peer *tls.Config in this repository, both go through
// one unexported function, and that function refuses to return a configuration
// that skips chain verification without a pinning callback. A test asserts
// that InsecureSkipVerify appears nowhere else in the tree.
package mtls

import (
	"context"
	"crypto/ed25519"
	"errors"
)

// Peer is what pinning learns about the far end of a connection.
//
// It is this package's own type rather than membership.Member so that the
// transport does not import the control plane's storage — the CLI dials peers
// with a single pinned key read from the API and has no database at all (see
// [PinnedKey]), and a transport that could only be constructed from a *sql.DB
// would have made that impossible.
type Peer struct {
	// PeerID is the membership record's id — the identity the SERVER derives
	// from the certificate, and the thing an acceptance assertion compares
	// against the id enrolment returned.
	PeerID string
	// Name is the peer's name within the fabric, for logs and errors.
	Name string
	// PublicKey is the pinned Ed25519 public key. It is the public half by
	// construction: the private half is a 0600 file at the other site.
	PublicKey ed25519.PublicKey
}

// Membership is the trust root, as the transport consults it (ADR-0012).
//
// Implementations MUST answer from storage every time. ADR-0012 makes
// revocation the removal of a membership record and there is no CRL to fall
// back on, so the freshness of this lookup IS the revocation mechanism. A set
// loaded at process start, a map warmed on first handshake or a cache with a
// TTL each mean a removed peer keeps its access for a window nobody chose.
//
// internal/peer/membership.Store answers this shape; the controller adapts it.
type Membership interface {
	// Lookup reports the member holding this public key, or an error wrapping
	// [ErrNotAMember] if none does.
	Lookup(ctx context.Context, publicKey []byte) (Peer, error)
}

// The refusals. Each is a distinct error because each calls for a different
// action, and "the handshake failed" tells an operator nothing they can use.
//
// None of them carries key material. The PUBLIC key appears in some of them
// and that is intentional — it is what an operator pastes into `heyarr peers
// add` to fix the problem — but nothing here has ever been handed a private
// key to leak.
var (
	// ErrNoPeerCertificate is a connection that presented no certificate at
	// all. On the server this is the anonymous client; on the client it cannot
	// happen, because a TLS server always sends one.
	ErrNoPeerCertificate = errors.New("mtls: no certificate was presented, and the peer fabric authenticates by certificate only")
	// ErrMalformedCertificate is a certificate that does not parse.
	ErrMalformedCertificate = errors.New("mtls: the presented certificate is not a valid X.509 certificate")
	// ErrCertificateExpired is a certificate whose validity window has passed.
	// Certificates are regenerated in-process (see Material), so this is
	// normally a clock disagreement rather than a neglected renewal.
	ErrCertificateExpired = errors.New("mtls: the presented certificate has expired")
	// ErrCertificateNotYetValid is the same disagreement in the other
	// direction. It is separate because the remedy is not the same: one end's
	// clock is ahead, and renewing anything would not help.
	ErrCertificateNotYetValid = errors.New("mtls: the presented certificate is not valid yet")
	// ErrNotAPeerKey is a certificate carrying something other than an Ed25519
	// public key. It cannot be a member of this fabric — ADR-0012 pins one
	// algorithm — and it is reported separately from "not a member" so that an
	// operator who generated the wrong kind of key is not sent hunting through
	// `heyarr peers`.
	ErrNotAPeerKey = errors.New("mtls: the presented certificate does not carry an ed25519 public key")
	// ErrNotSelfSigned is a certificate whose signature was not made by the
	// key it carries. Pinning compares the key, so such a certificate could
	// never authenticate anyway — but refusing it by name says which of the
	// two things is wrong, rather than reporting a key that is not a member
	// when the real fault is a certificate assembled from two identities.
	ErrNotSelfSigned = errors.New("mtls: the presented certificate is not signed by the key it carries")
	// ErrNotAMember is a well-formed certificate carrying a key no membership
	// record pins. It is the unknown-key refusal and the revoked-peer refusal
	// both: after removal there is nothing left to distinguish them, which is
	// what "revocation is removing a membership record" means.
	ErrNotAMember = errors.New("mtls: the presented public key is not a member of this fabric")
	// ErrNoPinning is a peer TLS configuration built without a pinning
	// callback. It is not an operator-facing error: it is the guard that makes
	// the InsecureSkipVerify slip a construction failure rather than a silent
	// authentication bypass. See [AssertPinned].
	ErrNoPinning = errors.New("mtls: refusing to build a peer TLS configuration with no pinning callback")
)

// PinnedKey is a [Membership] of exactly one peer.
//
// It exists for the caller that has already been told, by the controller,
// which key it is about to talk to: `heyarr peers ping` reads a peer record
// over the API and then dials that peer, and the key in the record it just
// read is the key it must pin. Consulting a membership table it does not have
// would be strictly weaker.
//
// It is NOT a cache of a membership store and must never be used as one. It
// answers about one key, which was supplied by the caller for one dial, and it
// goes away with the process.
func PinnedKey(p Peer) Membership { return pinnedKey{peer: p} }

type pinnedKey struct{ peer Peer }

func (p pinnedKey) Lookup(_ context.Context, publicKey []byte) (Peer, error) {
	if len(publicKey) != ed25519.PublicKeySize ||
		!ed25519.PublicKey(publicKey).Equal(p.peer.PublicKey) {
		return Peer{}, ErrNotAMember
	}
	return p.peer, nil
}
