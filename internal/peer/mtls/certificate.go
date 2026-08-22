package mtls

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"sync"
	"time"
)

// DefaultLifetime is how long a generated peer certificate is valid.
//
// It is short because nothing depends on it being long: the certificate is
// generated in memory at startup, regenerated in place before it expires, and
// never written to disk, so renewal costs nothing and needs no operator. It is
// not a security control — the key is the identity and the key does not rotate
// with the certificate — but a validity window that has to be honoured at all
// is what makes an expired certificate a case this fabric can refuse, and
// refusing it is one of M4-05's acceptance conditions.
const DefaultLifetime = 30 * 24 * time.Hour

// DefaultRenewBefore is how far ahead of expiry Material regenerates.
//
// A certificate is replaced while it is still valid rather than after it stops
// being, so a long-running node never presents an expired one to a peer whose
// clock is a little ahead of its own.
const DefaultRenewBefore = 24 * time.Hour

// certificateURIScheme names what the SAN in a peer certificate is.
//
// There is no DNS name and no IP address in these certificates on purpose.
// Names are not identity here — a peer is its public key (ADR-0012) — and
// putting a hostname in would invite exactly the wrong reflex in a future
// reader: verifying the name and believing that meant something. A URI SAN
// spelling out the peer id is for a human reading `openssl x509 -text`, and
// nothing in this package makes a decision on it.
const certificateURIScheme = "heyarr"

// SelfSigned mints a certificate for a peer identity.
//
// The certificate carries the Ed25519 public key and is signed by the matching
// private key. That signature proves nothing to a verifier — anyone can sign
// their own certificate, which is why membership is the trust root — and it is
// there because X.509 requires a signature and because a certificate whose
// signature does not match the key it carries is a certificate assembled from
// two identities, which is worth refusing by name (see [ErrNotSelfSigned]).
//
// notBefore and lifetime are parameters rather than derived from time.Now so
// that an expired certificate is a thing a test can produce honestly, rather
// than something a test has to fake by moving a verifier's clock. Every
// acceptance condition about expiry rests on this.
func SelfSigned(priv ed25519.PrivateKey, peerID string, notBefore time.Time, lifetime time.Duration) (tls.Certificate, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return tls.Certificate{}, fmt.Errorf("mtls: a peer certificate needs an ed25519 private key, got %d bytes", len(priv))
	}
	if peerID == "" {
		return tls.Certificate{}, errors.New("mtls: a peer certificate needs the peer id it belongs to")
	}
	if lifetime <= 0 {
		return tls.Certificate{}, fmt.Errorf("mtls: a peer certificate needs a positive lifetime, got %s", lifetime)
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return tls.Certificate{}, errors.New("mtls: the private key did not yield an ed25519 public key")
	}

	// A serial nobody coordinates. Serial numbers matter to a CA reconciling
	// issuance across time; there is no CA here, so this only has to be
	// unlikely to repeat within one fabric.
	serialMax := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialMax)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("mtls: generating a certificate serial: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: peerID},
		Issuer:       pkix.Name{CommonName: peerID},
		NotBefore:    notBefore.UTC(),
		NotAfter:     notBefore.Add(lifetime).UTC(),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		// Both, because a Heyarr peer is both ends of a peer connection: it
		// serves bytes to the other site and pulls bytes from it (ADR-0029).
		// One certificate for both directions means one identity, which is the
		// whole premise — a node with a separate client identity would be two
		// members of the fabric wearing one name.
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		URIs:                  []*url.URL{{Scheme: certificateURIScheme, Host: "peer", Path: "/" + peerID}},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, pub, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("mtls: creating the peer certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("mtls: re-reading the peer certificate: %w", err)
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  priv,
		Leaf:        leaf,
	}, nil
}

// MaterialOptions configure a [Material].
type MaterialOptions struct {
	// PrivateKey is this node's Ed25519 private key, from identity.Signer.
	PrivateKey ed25519.PrivateKey
	// PeerID is this node's membership id.
	PeerID string
	// Lifetime defaults to DefaultLifetime.
	Lifetime time.Duration
	// RenewBefore defaults to DefaultRenewBefore.
	RenewBefore time.Duration
	// Now is injected so renewal is testable without sleeping (ADR-0017).
	Now func() time.Time
}

// Material is this node's certificate, regenerated as it ages.
//
// It holds the private key and hands out a *tls.Certificate. Nothing is
// persisted: the certificate is derived from the identity every time it is
// needed, so there is no certificate file to back up, to leak, to forget to
// rotate, or to restore onto a second machine. ADR-0012 pins keys, not
// certificates, so a regenerated certificate is the same peer to everyone that
// enrolled it — which is exactly why regenerating it can be automatic.
type Material struct {
	mu          sync.Mutex
	priv        ed25519.PrivateKey
	peerID      string
	lifetime    time.Duration
	renewBefore time.Duration
	now         func() time.Time

	current *tls.Certificate
}

// NewMaterial prepares certificate material for a peer identity. It generates
// nothing until the first certificate is asked for.
func NewMaterial(opts MaterialOptions) (*Material, error) {
	if len(opts.PrivateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("mtls: certificate material needs an ed25519 private key, got %d bytes", len(opts.PrivateKey))
	}
	if opts.PeerID == "" {
		return nil, errors.New("mtls: certificate material needs this node's peer id")
	}
	lifetime := opts.Lifetime
	if lifetime <= 0 {
		lifetime = DefaultLifetime
	}
	renew := opts.RenewBefore
	if renew <= 0 {
		renew = DefaultRenewBefore
	}
	if renew >= lifetime {
		// A renewal window wider than the lifetime regenerates on every
		// handshake, which is not a renewal policy but a signature-per-request
		// denial of service against yourself.
		return nil, fmt.Errorf("mtls: renew_before %s is not shorter than the certificate lifetime %s", renew, lifetime)
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Material{
		priv:        opts.PrivateKey,
		peerID:      opts.PeerID,
		lifetime:    lifetime,
		renewBefore: renew,
		now:         now,
	}, nil
}

// PublicKey is the identity this material proves.
func (m *Material) PublicKey() ed25519.PublicKey {
	pub, _ := m.priv.Public().(ed25519.PublicKey)
	return pub
}

// PeerID is the peer this material belongs to.
func (m *Material) PeerID() string { return m.peerID }

// Certificate returns a currently valid certificate, minting a fresh one when
// the held certificate is inside its renewal window or past it.
//
// It is safe for concurrent use: TLS calls it once per handshake, from
// whichever goroutine is accepting or dialling.
func (m *Material) Certificate() (*tls.Certificate, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	if m.current != nil && m.current.Leaf != nil &&
		now.Before(m.current.Leaf.NotAfter.Add(-m.renewBefore)) &&
		!now.Before(m.current.Leaf.NotBefore) {
		return m.current, nil
	}
	// Backdated by the renewal window so that a peer whose clock is behind
	// this one does not reject a certificate minted a moment ago. Without it,
	// the very first handshake after a renewal is the one most likely to fail,
	// and it fails intermittently, which is the worst way for a clock skew to
	// present itself.
	cert, err := SelfSigned(m.priv, m.peerID, now.Add(-m.renewBefore), m.lifetime)
	if err != nil {
		return nil, err
	}
	m.current = &cert
	return m.current, nil
}
