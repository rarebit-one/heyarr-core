package mtls_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
)

// These tests are about the refusals.
//
// A pinning check that has never refused anything is decoration (ADR-0012),
// and the reason it stays decoration is that a refusal is only observable
// through a handshake as "handshake failure" — one opaque outcome that an
// unknown key, an expired certificate and a listener that never started all
// produce. So the checks are exercised here one at a time, by name, and the
// handshake tests next door assert separately that the refusal really does
// happen at the connection level.

var epoch = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

func keypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// members is a trust root that answers from a map, and counts.
//
// It queries a map rather than a database, and that is the ONE thing it is
// allowed to do differently from the real store: it must never memoise its own
// answer, because the property under test is that the transport asks again.
type members struct {
	byKey  map[string]mtls.Peer
	answer func(mtls.Peer) mtls.Peer // optional distortion, for the last guard
}

func (m members) Lookup(_ context.Context, pub []byte) (mtls.Peer, error) {
	p, ok := m.byKey[string(pub)]
	if !ok {
		return mtls.Peer{}, mtls.ErrNotAMember
	}
	if m.answer != nil {
		return m.answer(p), nil
	}
	return p, nil
}

func fabric(t *testing.T, peers ...mtls.Peer) members {
	t.Helper()
	m := members{byKey: map[string]mtls.Peer{}}
	for _, p := range peers {
		m.byKey[string(p.PublicKey)] = p
	}
	return m
}

func TestASelfSignedCertificateCarriesTheIdentityAndNothingElse(t *testing.T) {
	pub, priv := keypair(t)
	cert, err := mtls.SelfSigned(priv, "peer-a", epoch, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	leaf := cert.Leaf
	if got, ok := leaf.PublicKey.(ed25519.PublicKey); !ok || !got.Equal(pub) {
		t.Fatalf("the certificate carries %T that is not this peer's public key", leaf.PublicKey)
	}
	if leaf.Subject.CommonName != "peer-a" {
		t.Errorf("common name = %q, want the peer id", leaf.Subject.CommonName)
	}
	if leaf.IsCA {
		t.Error("a peer certificate is a CA — there is no PKI here and nothing may be issued from it (ADR-0012)")
	}
	if len(leaf.DNSNames) != 0 || len(leaf.IPAddresses) != 0 {
		t.Errorf("the certificate carries names (%v / %v). A peer is its key, and a name in here "+
			"invites a future reader to verify the name and believe it means something",
			leaf.DNSNames, leaf.IPAddresses)
	}
	wantEKU := map[x509.ExtKeyUsage]bool{x509.ExtKeyUsageServerAuth: false, x509.ExtKeyUsageClientAuth: false}
	for _, eku := range leaf.ExtKeyUsage {
		if _, ok := wantEKU[eku]; ok {
			wantEKU[eku] = true
		}
	}
	for eku, seen := range wantEKU {
		if !seen {
			t.Errorf("the certificate is missing extended key usage %v — a Heyarr peer is both "+
				"ends of a peer connection and one identity must serve both (ADR-0029)", eku)
		}
	}
	if !leaf.NotAfter.Equal(epoch.Add(time.Hour)) {
		t.Errorf("not_after = %s, want %s", leaf.NotAfter, epoch.Add(time.Hour))
	}
}

func TestMaterialRenewsBeforeExpiryAndKeepsTheSameIdentity(t *testing.T) {
	pub, priv := keypair(t)
	now := epoch
	m, err := mtls.NewMaterial(mtls.MaterialOptions{
		PrivateKey: priv, PeerID: "peer-a",
		Lifetime: 10 * time.Hour, RenewBefore: time.Hour,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := m.Certificate()
	if err != nil {
		t.Fatal(err)
	}
	again, err := m.Certificate()
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Error("a second call minted a fresh certificate; signing one per handshake is a denial " +
			"of service against yourself")
	}

	// Inside the renewal window, still valid, and it renews anyway. That is
	// the point: a certificate is replaced while it is still good, so a peer
	// whose clock is a little ahead never sees an expired one.
	now = first.Leaf.NotAfter.Add(-30 * time.Minute)
	renewed, err := m.Certificate()
	if err != nil {
		t.Fatal(err)
	}
	if renewed == first {
		t.Fatal("the certificate was not renewed inside its renewal window")
	}
	if !renewed.Leaf.NotAfter.After(first.Leaf.NotAfter) {
		t.Errorf("the renewed certificate expires at %s, no later than the old one at %s",
			renewed.Leaf.NotAfter, first.Leaf.NotAfter)
	}
	got, ok := renewed.Leaf.PublicKey.(ed25519.PublicKey)
	if !ok || !got.Equal(pub) {
		t.Error("renewal changed this node's public key — every peer that enrolled it would now " +
			"refuse it (ADR-0012 pins keys, not certificates)")
	}
}

// TestEveryRefusalIsNamed is the heart of this file.
//
// One case per check, each asserting a DISTINCT error. A single "invalid
// certificates are rejected" test would pass with four of these checks deleted,
// and each one calls for a different action: enrol a key, fix a clock,
// regenerate a key of the right kind, stop assembling a certificate out of two
// identities.
func TestEveryRefusalIsNamed(t *testing.T) {
	memberPub, memberPriv := keypair(t)
	strangerPub, strangerPriv := keypair(t)
	_ = strangerPub

	member := mtls.Peer{PeerID: "peer-b-id", Name: "peer-b", PublicKey: memberPub}

	valid, err := mtls.SelfSigned(memberPriv, "peer-b-id", epoch.Add(-time.Hour), 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	expired, err := mtls.SelfSigned(memberPriv, "peer-b-id", epoch.Add(-10*time.Hour), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	future, err := mtls.SelfSigned(memberPriv, "peer-b-id", epoch.Add(10*time.Hour), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := mtls.SelfSigned(strangerPriv, "somebody-else", epoch.Add(-time.Hour), 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		raw     [][]byte
		members members
		want    error
		// why records what a reader of a failure should conclude, so a case
		// that starts passing for the wrong reason is still readable.
		why string
	}{
		{
			name:    "no certificate at all",
			raw:     nil,
			members: fabric(t, member),
			want:    mtls.ErrNoPeerCertificate,
			why:     "an anonymous connection has no key to pin",
		},
		{
			name:    "a certificate that does not parse",
			raw:     [][]byte{[]byte("this is not DER")},
			members: fabric(t, member),
			want:    mtls.ErrMalformedCertificate,
			why:     "garbage must be named as garbage, not reported as a key that is not a member",
		},
		{
			name:    "an expired certificate",
			raw:     expired.Certificate,
			members: fabric(t, member),
			want:    mtls.ErrCertificateExpired,
			why:     "the key is a member; the certificate is stale, and saying so is the difference between renewing and re-enrolling",
		},
		{
			name:    "a certificate that is not valid yet",
			raw:     future.Certificate,
			members: fabric(t, member),
			want:    mtls.ErrCertificateNotYetValid,
			why:     "the remedy is a clock, and renewing anything would not help",
		},
		{
			name:    "a certificate carrying the wrong kind of key",
			raw:     [][]byte{ecdsaCert(t)},
			members: fabric(t, member),
			want:    mtls.ErrNotAPeerKey,
			why:     "ADR-0012 pins one algorithm; an operator who generated the wrong kind of key should not be sent hunting through `heyarr peers`",
		},
		{
			name:    "a certificate signed by a key it does not carry",
			raw:     [][]byte{mismatchedCert(t, memberPub, strangerPriv)},
			members: fabric(t, member),
			want:    mtls.ErrNotSelfSigned,
			why:     "a certificate assembled from two identities is a different fault from an unknown key",
		},
		{
			name:    "a key no membership record pins",
			raw:     stranger.Certificate,
			members: fabric(t, member),
			want:    mtls.ErrNotAMember,
			why:     "this is the unknown-key refusal, and the whole reason the mechanism exists",
		},
		{
			name:    "no members at all",
			raw:     valid.Certificate,
			members: fabric(t),
			want:    mtls.ErrNotAMember,
			why:     "a fabric with an empty membership table admits nobody, rather than everybody",
		},
		{
			name: "a trust root that answered with a record pinning a different key",
			raw:  valid.Certificate,
			members: members{
				byKey: map[string]mtls.Peer{
					string(memberPub): member,
				},
				answer: func(p mtls.Peer) mtls.Peer {
					p.PublicKey = strangerPub
					return p
				},
			},
			want: mtls.ErrNotAMember,
			why: "the pin is a comparison against the presented key, not a lookup key — " +
				"a Lookup that answered by name would slip through here and nowhere else",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verify, err := mtls.Verifier(mtls.Options{
				Members: tc.members,
				Now:     func() time.Time { return epoch },
			})
			if err != nil {
				t.Fatal(err)
			}
			peer, err := verify(context.Background(), tc.raw)
			if err == nil {
				t.Fatalf("accepted %s as peer %q — %s", tc.name, peer.PeerID, tc.why)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("refused with %v, want %v — %s", err, tc.want, tc.why)
			}
		})
	}

	// The positive control. Without it every case above would still pass on a
	// verifier that refused everything, which is the other way this mechanism
	// fails and the harder one to notice.
	t.Run("the member's own certificate is accepted", func(t *testing.T) {
		verify, err := mtls.Verifier(mtls.Options{
			Members: fabric(t, member),
			Now:     func() time.Time { return epoch },
		})
		if err != nil {
			t.Fatal(err)
		}
		peer, err := verify(context.Background(), valid.Certificate)
		if err != nil {
			t.Fatalf("an enrolled member was refused: %v", err)
		}
		if peer.PeerID != member.PeerID {
			t.Errorf("resolved peer id %q, want %q", peer.PeerID, member.PeerID)
		}
	})
}

// TestARefusalNamesNoPrivateKeyMaterial: the refusals are read by operators and
// end up in logs and issues. The public key is in them deliberately — it is
// what gets pasted into `heyarr peers add` — and nothing else may be.
func TestARefusalNamesNoPrivateKeyMaterial(t *testing.T) {
	pub, priv := keypair(t)
	cert, err := mtls.SelfSigned(priv, "peer-b-id", epoch.Add(-time.Hour), 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	verify, err := mtls.Verifier(mtls.Options{
		Members: fabric(t),
		Now:     func() time.Time { return epoch },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = verify(context.Background(), cert.Certificate)
	if err == nil {
		t.Fatal("an unenrolled key was accepted")
	}
	msg := err.Error()
	// The positive control first: the PUBLIC key really is in the message, so
	// the absence below is a finding rather than an empty comparison.
	if !strings.Contains(msg, "ed25519:") {
		t.Fatalf("the refusal does not name the key an operator would have to enrol: %s", msg)
	}
	for _, forbidden := range []string{
		string(priv.Seed()),
		string(priv),
		"ed25519-seed:",
	} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("the refusal leaks private key material")
		}
	}
	_ = pub
}

// TestAssertPinnedRefusesTheEditThatAuthenticatesNobody is the invariant stated
// as an assertion rather than as a comment.
//
// InsecureSkipVerify plus a hand-rolled callback is the correct shape when
// there is no CA. InsecureSkipVerify with the callback deleted is an
// authentication bypass that passes every happy-path test ever written, because
// in every environment anyone develops in, both ends are honest.
func TestAssertPinnedRefusesTheEditThatAuthenticatesNobody(t *testing.T) {
	ok := func([][]byte, [][]*x509.Certificate) error { return nil }
	okConn := func(tls.ConnectionState) error { return nil }
	cases := []struct {
		name string
		cfg  *tls.Config
		want bool // want an error
	}{
		{"nil", nil, true},
		{
			"the whole failure mode: skip verification, verify nothing",
			&tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}, // #nosec G402 -- the configuration under test
			true,
		},
		{
			"a listener that requires a certificate and pins nothing",
			&tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAnyClientCert},
			true,
		},
		{
			"a chain-verifying ClientAuth, which has no CA to verify against here",
			&tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.VerifyClientCertIfGiven, VerifyPeerCertificate: ok, VerifyConnection: okConn},
			true,
		},
		{
			"an old TLS version",
			&tls.Config{MinVersion: tls.VersionTLS12, VerifyPeerCertificate: ok, VerifyConnection: okConn},
			true,
		},
		{
			// A resumed session runs no handshake, so VerifyPeerCertificate is
			// never called on it. Pinning that only happens on a full
			// handshake is pinning with a hole the width of a session ticket.
			"pinned on a full handshake and nowhere else",
			&tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, VerifyPeerCertificate: ok}, // #nosec G402 -- the configuration under test
			true,
		},
		{
			"the correct shape",
			&tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, VerifyPeerCertificate: ok, VerifyConnection: okConn}, // #nosec G402 -- pinning replaces chain verification
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := mtls.AssertPinned(tc.cfg)
			if tc.want && err == nil {
				t.Fatal("accepted a configuration that authenticates nobody")
			}
			if !tc.want && err != nil {
				t.Fatalf("refused the correct shape: %v", err)
			}
			if tc.want && !errors.Is(err, mtls.ErrNoPinning) {
				t.Errorf("refused with %v, want ErrNoPinning", err)
			}
		})
	}
}

// TestTheConstructorsRefuseAFabricWithNoTrustRoot: a peer configuration built
// without membership would authenticate every key that connects, and it would
// do so silently.
func TestTheConstructorsRefuseAFabricWithNoTrustRoot(t *testing.T) {
	_, priv := keypair(t)
	material, err := mtls.NewMaterial(mtls.MaterialOptions{PrivateKey: priv, PeerID: "peer-a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mtls.ServerConfig(mtls.Options{Material: material}); err == nil {
		t.Error("ServerConfig built a listener with no trust root")
	}
	if _, err := mtls.ClientConfig(mtls.Options{Material: material}); err == nil {
		t.Error("ClientConfig built a dialler with no trust root")
	}
	if _, err := mtls.ServerConfig(mtls.Options{Members: fabric(t)}); err == nil {
		t.Error("ServerConfig built a listener with no identity to present")
	}
}

// TestTheConstructedConfigurationsPin closes the loop: whatever the
// constructors return must satisfy the invariant AssertPinned states.
func TestTheConstructedConfigurationsPin(t *testing.T) {
	_, priv := keypair(t)
	material, err := mtls.NewMaterial(mtls.MaterialOptions{PrivateKey: priv, PeerID: "peer-a"})
	if err != nil {
		t.Fatal(err)
	}
	server, err := mtls.ServerConfig(mtls.Options{Material: material, Members: fabric(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := mtls.AssertPinned(server); err != nil {
		t.Errorf("ServerConfig returned a configuration that does not pin: %v", err)
	}
	if server.ClientAuth != tls.RequireAnyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAnyClientCert — an anonymous connection has no key "+
			"to pin and must be refused before a request exists", server.ClientAuth)
	}
	client, err := mtls.ClientConfig(mtls.Options{Material: material, Members: fabric(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := mtls.AssertPinned(client); err != nil {
		t.Errorf("ClientConfig returned a configuration that does not pin: %v", err)
	}
	if !client.InsecureSkipVerify {
		t.Error("the client config verifies a chain; there is no CA in this fabric to build one against")
	}
	for name, cfg := range map[string]*tls.Config{"server": server, "client": client} {
		if cfg.VerifyConnection == nil {
			t.Errorf("the %s config pins only on a full handshake; a resumed session would skip the "+
				"check entirely and a revoked peer would keep its access for a ticket lifetime", name)
		}
	}
}

// TestInsecureSkipVerifyLivesInExactlyOnePlace is the structural half of the
// countermeasure.
//
// AssertPinned makes the mistake a construction error wherever a peer config is
// built through this package. This makes it impossible to build one somewhere
// else without the diff being obvious: a second occurrence of the field
// anywhere in the tree fails this test by name.
func TestInsecureSkipVerifyLivesInExactlyOnePlace(t *testing.T) {
	const allowed = "internal/peer/mtls/pinning.go"
	root := filepath.Join("..", "..", "..")
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "node_modules", ".worktrees":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Parsed rather than grepped, and printed back WITHOUT comments:
		// this package's documentation names the field it exists to guard,
		// and a scan that counted prose would force the explanation out of
		// the one place a reader needs it.
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		var code strings.Builder
		if err := printer.Fprint(&code, fset, parsed); err != nil {
			return err
		}
		if !strings.Contains(code.String(), "InsecureSkipVerify") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		found = append(found, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0] != allowed {
		t.Errorf("InsecureSkipVerify appears in %v; it may appear only in %s, which is the one "+
			"place that also attaches a pinning callback and refuses to return without one "+
			"(ADR-0012). If this is a legitimate new peer transport, route it through "+
			"mtls.ClientConfig instead.", found, allowed)
	}
}

func TestPinnedKeyAnswersAboutOneKeyAndNoOther(t *testing.T) {
	pub, _ := keypair(t)
	other, _ := keypair(t)
	m := mtls.PinnedKey(mtls.Peer{PeerID: "peer-b-id", Name: "peer-b", PublicKey: pub})
	got, err := m.Lookup(context.Background(), pub)
	if err != nil {
		t.Fatalf("the pinned key was not recognised: %v", err)
	}
	if got.PeerID != "peer-b-id" {
		t.Errorf("peer id = %q", got.PeerID)
	}
	if _, err := m.Lookup(context.Background(), other); !errors.Is(err, mtls.ErrNotAMember) {
		t.Errorf("a different key was answered with %v, want ErrNotAMember", err)
	}
}

// ecdsaCert is a self-signed certificate carrying something that is not an
// Ed25519 key.
func ecdsaCert(t *testing.T) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "not-a-peer"},
		NotBefore:    epoch.Add(-time.Hour),
		NotAfter:     epoch.Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// mismatchedCert carries one identity's public key and another's signature.
func mismatchedCert(t *testing.T, carried ed25519.PublicKey, signer ed25519.PrivateKey) []byte {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "peer-b-id"},
		NotBefore:    epoch.Add(-time.Hour),
		NotAfter:     epoch.Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, carried, signer)
	if err != nil {
		t.Fatal(err)
	}
	return der
}
