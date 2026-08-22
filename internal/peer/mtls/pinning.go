package mtls

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// lookupTimeout bounds the membership query a handshake makes.
//
// It is generous because the query is one indexed read against a local SQLite
// file, and it exists at all because a handshake blocking forever on a wedged
// database is a peer that cannot be told it was refused.
const lookupTimeout = 5 * time.Second

// Options configure a pinned TLS configuration.
type Options struct {
	// Material is this node's certificate, and therefore its identity.
	Material *Material
	// Members is the trust root. Required: a peer configuration without one
	// would authenticate everybody.
	Members Membership
	// Now is injected so certificate validity is testable (ADR-0017).
	Now func() time.Time
	// Logger records refusals. Every refusal is logged at the point it is
	// made, with which check failed, because the far end only ever sees a TLS
	// alert — "handshake failure" on one side and a name on the other is how
	// an operator ends up believing the network is broken.
	Logger *slog.Logger
}

// ServerConfig is the TLS configuration a peer listener uses.
//
// ClientAuth is RequireAnyClientCert: "Any" because there is no CA to check a
// chain against (ADR-0012), and "Require" because an anonymous connection has
// no key to pin and must be refused by the TLS stack before any request
// exists. That is the whole of "a node presenting no client certificate is
// refused" — it is a failed handshake, not a 403 on a completed session.
func ServerConfig(opts Options) (*tls.Config, error) {
	p, err := newPinning(opts, "server")
	if err != nil {
		return nil, err
	}
	if p.material == nil {
		return nil, errors.New("mtls: a peer listener needs this node's certificate material")
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAnyClientCert,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return p.material.Certificate()
		},
	}
	return pinned(cfg, p.verify)
}

// ClientConfig is the TLS configuration a peer dialler uses.
//
// It pins the server exactly as the server pins it. Authentication in this
// fabric is mutual in the literal sense: neither end has any reason to believe
// the other beyond "the key you are holding is a key my membership record
// pins", and a client that verified nothing would happily stream a library to
// whatever answered on that port after a DNS change.
func ClientConfig(opts Options) (*tls.Config, error) {
	p, err := newPinning(opts, "client")
	if err != nil {
		return nil, err
	}
	if p.material == nil {
		return nil, errors.New("mtls: a peer dialler needs this node's certificate material")
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return p.material.Certificate()
		},
		// There is no CA and no name to check, so the standard verification
		// has nothing to do and is turned off. Everything that decides whether
		// to continue is the callback pinned() attaches below, and pinned()
		// refuses to return a configuration without one.
		InsecureSkipVerify: true, // #nosec G402 -- pinning replaces chain verification; see AssertPinned
	}
	return pinned(cfg, p.verify)
}

// pinned is the ONLY place a peer TLS configuration is finished, and the only
// place in this repository that may set InsecureSkipVerify.
//
// The failure mode it exists for is not a wrong comparison. It is an edit that
// deletes the callback and leaves the skip — which authenticates nobody, and
// passes every test in which both ends are honest, which is every test anyone
// writes by default. Routing both constructors through one function that
// refuses a nil callback turns that edit from a silent bypass into a
// construction error at startup.
func pinned(cfg *tls.Config, verify func([][]byte, [][]*x509.Certificate) error) (*tls.Config, error) {
	if verify == nil {
		return nil, ErrNoPinning
	}
	cfg.VerifyPeerCertificate = verify
	if err := AssertPinned(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// AssertPinned refuses a TLS configuration that has turned verification off
// without putting anything in its place.
//
// It is exported so the assertion can be made at the seams as well as here —
// and so that a test can state the invariant as an assertion about a
// configuration rather than as an inspection of this file.
func AssertPinned(cfg *tls.Config) error {
	if cfg == nil {
		return fmt.Errorf("%w: the configuration is nil", ErrNoPinning)
	}
	if cfg.VerifyPeerCertificate == nil {
		return fmt.Errorf("%w: InsecureSkipVerify=%t, ClientAuth=%v — with no callback this "+
			"configuration authenticates every key that connects (ADR-0012)",
			ErrNoPinning, cfg.InsecureSkipVerify, cfg.ClientAuth)
	}
	if cfg.MinVersion < tls.VersionTLS13 {
		return fmt.Errorf("%w: the peer path requires TLS 1.3; ADR-0012 says this must be safe "+
			"over a hostile network and not lean on a tunnel whose crypto ages", ErrNoPinning)
	}
	if cfg.ClientAuth != tls.NoClientCert && cfg.ClientAuth != tls.RequireAnyClientCert {
		// The other modes verify a chain against ClientCAs, which is empty
		// here and always will be. VerifyClientCertIfGiven in particular reads
		// as stricter and is the one that lets an anonymous client through.
		return fmt.Errorf("%w: ClientAuth is %v; the peer path has no CA to build a chain "+
			"against, so a peer listener is RequireAnyClientCert and pins the key itself",
			ErrNoPinning, cfg.ClientAuth)
	}
	return nil
}

// Verifier is the pinning decision as a callable, without a handshake around
// it.
//
// It exists because a refusal is only ever observable through a handshake as
// "handshake failure": the far end is told nothing, on purpose. A test driven
// only through handshakes therefore cannot tell an unknown key from an expired
// certificate from a listener that never came up — and M3 shipped three tests
// that asserted the wrong failure for exactly that reason. This makes each
// check addressable by name; the handshake tests then assert, separately, that
// the refusal really does happen at the connection level.
func Verifier(opts Options) (func(ctx context.Context, rawCerts [][]byte) (Peer, error), error) {
	p, err := newPinning(opts, "verifier")
	if err != nil {
		return nil, err
	}
	return p.check, nil
}

// pinning holds what a verification needs. It is a struct rather than a
// closure over four variables so that the checks below read as a list of
// checks.
type pinning struct {
	material *Material
	members  Membership
	now      func() time.Time
	log      *slog.Logger
	role     string
}

func newPinning(opts Options, role string) (pinning, error) {
	if opts.Members == nil {
		return pinning{}, errors.New("mtls: a peer TLS configuration needs a membership trust root — " +
			"without one it would authenticate every key that connects (ADR-0012)")
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return pinning{material: opts.Material, members: opts.Members, now: now, log: log.With("component", "mtls"), role: role}, nil
}

// verify is the callback both configurations hang on.
func (p pinning) verify(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	peer, err := p.check(context.Background(), rawCerts)
	if err != nil {
		// Logged here, at the point of refusal, because this is the only place
		// that knows WHICH check failed. The far end gets a TLS alert with no
		// detail in it, by design — telling an unauthenticated caller whether
		// its key is unknown or its certificate is stale is telling it about
		// the membership table.
		p.log.Warn("refused a peer connection",
			"role", p.role, "reason", err.Error())
		return err
	}
	p.log.Debug("pinned a peer connection",
		"role", p.role, "peer_id", peer.PeerID, "peer_name", peer.Name)
	return nil
}

// check is the pinning decision. [Verifier] exposes it so it can be exercised
// directly rather than only through a handshake.
//
// That matters more than it looks. A refusal is only observable through a
// handshake as "handshake failed", so a test driven only through a handshake
// cannot tell an unknown key from an expired certificate from a listener that
// never came up — and M3 shipped three tests that asserted the wrong failure
// for exactly that reason. Here the checks are addressable one at a time, and
// the handshake tests then assert that the refusal really is at the connection
// level.
//
// The order is deliberate: shape, then validity, then membership. Membership
// is last because it is the only check that costs a query, and because a
// malformed certificate should be named as malformed rather than reported as a
// key that is not a member.
func (p pinning) check(ctx context.Context, rawCerts [][]byte) (Peer, error) {
	if len(rawCerts) == 0 {
		return Peer{}, fmt.Errorf("%w (%s side)", ErrNoPeerCertificate, p.role)
	}
	leaf, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return Peer{}, fmt.Errorf("%w: %w", ErrMalformedCertificate, err)
	}

	now := p.now().UTC()
	switch {
	case now.Before(leaf.NotBefore):
		return Peer{}, fmt.Errorf("%w: it is valid from %s and this node's clock reads %s",
			ErrCertificateNotYetValid, leaf.NotBefore.UTC().Format(time.RFC3339), now.Format(time.RFC3339))
	case now.After(leaf.NotAfter):
		return Peer{}, fmt.Errorf("%w: it expired at %s and this node's clock reads %s",
			ErrCertificateExpired, leaf.NotAfter.UTC().Format(time.RFC3339), now.Format(time.RFC3339))
	}

	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		return Peer{}, fmt.Errorf("%w: it carries a %T, and this deployment pins %s (ADR-0012)",
			ErrNotAPeerKey, leaf.PublicKey, identity.Algorithm)
	}
	if len(pub) != ed25519.PublicKeySize {
		return Peer{}, fmt.Errorf("%w: the key is %d bytes and an ed25519 public key is %d",
			ErrNotAPeerKey, len(pub), ed25519.PublicKeySize)
	}
	if err := leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature); err != nil {
		// Deliberately not CheckSignatureFrom: that refuses a non-CA issuer,
		// and every certificate in this fabric is a non-CA issuer of itself.
		return Peer{}, fmt.Errorf("%w: %w", ErrNotSelfSigned, err)
	}

	// Membership, last and every time.
	//
	// Not once per process, not once per connection pool, not behind a TTL.
	// ADR-0012 makes revocation the removal of a record and leaves no CRL to
	// fall back on, so the freshness of this lookup IS the revocation
	// mechanism (M4-04). A snapshot taken at startup would pass every test in
	// which nobody is ever removed.
	lookupCtx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	peer, err := p.members.Lookup(lookupCtx, pub)
	switch {
	case errors.Is(err, ErrNotAMember):
		return Peer{}, fmt.Errorf("%w: %s. Membership is the only trust root in the inter-peer path "+
			"(ADR-0012) — enrol this key with `heyarr peers add`, or it was revoked",
			ErrNotAMember, identity.FormatPublicKey(pub))
	case err != nil:
		// Fail closed. "I could not ask the trust root" is not "yes".
		return Peer{}, fmt.Errorf("mtls: consulting membership for the presented key: %w", err)
	}
	if !ed25519.PublicKey(peer.PublicKey).Equal(pub) {
		// The record the trust root returned must be the record for the key
		// that was actually presented. A Lookup that answered by name, by
		// endpoint, or by anything else would make the pin a lookup key rather
		// than a comparison, and this is the one line that would notice.
		return Peer{}, fmt.Errorf("%w: membership answered with peer %s, whose pinned key is not the "+
			"presented one", ErrNotAMember, peer.PeerID)
	}
	return peer, nil
}
