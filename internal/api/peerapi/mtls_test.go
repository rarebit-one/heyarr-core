// Every response in this file is drained and closed by the helper that made
// the request, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by peerGet
package peerapi_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
)

// This file is M4-05's acceptance, and its subject is the refusals.
//
// Two rules shape every test here.
//
// First: a refusal must be a FAILED HANDSHAKE, not a 403. A 403 arrives over a
// completed TLS session, which means the listener accepted a key it had no
// reason to trust and then thought better of it at the HTTP layer — and every
// byte of a range response would already have been streamable to a route that
// forgot the check. So the refusals below are asserted with a raw dial and an
// explicit Handshake(), where "refused" and "connected" cannot be confused.
//
// Second: the happy path must assert the identity the SERVER DERIVED, not the
// status code. A server that authenticated nobody also answers 200.

// ---------------------------------------------------------------------------
// fixtures

// peerNode is one node's identity and the certificate material it presents.
type peerNode struct {
	peerID   string
	name     string
	pub      ed25519.PublicKey
	priv     ed25519.PrivateKey
	material *mtls.Material
}

func newPeerNode(t *testing.T, peerID, name string) *peerNode {
	t.Helper()
	return newPeerNodeAt(t, peerID, name, nil)
}

// newPeerNodeAt is newPeerNode with an injected clock, so a node whose
// certificate is expired from the other end's point of view is something a
// test can produce honestly rather than fake.
func newPeerNodeAt(t *testing.T, peerID, name string, now func() time.Time) *peerNode {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	material, err := mtls.NewMaterial(mtls.MaterialOptions{
		PrivateKey: priv, PeerID: peerID,
		Lifetime: time.Hour, RenewBefore: time.Minute,
		Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &peerNode{peerID: peerID, name: name, pub: pub, priv: priv, material: material}
}

func (p *peerNode) member() mtls.Peer {
	return mtls.Peer{PeerID: p.peerID, Name: p.name, PublicKey: p.pub}
}

// trustRoot is the membership table both ends consult.
//
// It answers from a map and counts every call. The count is what tells a
// working guard from a cached one: a cache returns the right answer for a peer
// nobody removed, and the only thing that distinguishes it is that it stops
// asking.
type trustRoot struct {
	mu      sync.Mutex
	byKey   map[string]mtls.Peer
	lookups atomic.Int64
}

func newTrustRoot(members ...mtls.Peer) *trustRoot {
	r := &trustRoot{byKey: map[string]mtls.Peer{}}
	for _, m := range members {
		r.byKey[string(m.PublicKey)] = m
	}
	return r
}

func (r *trustRoot) Lookup(_ context.Context, pub []byte) (mtls.Peer, error) {
	r.lookups.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byKey[string(pub)]
	if !ok {
		return mtls.Peer{}, mtls.ErrNotAMember
	}
	return p, nil
}

// remove is revocation: the record is deleted, because there is no revocation
// list in this design to add to (ADR-0012).
func (r *trustRoot) remove(pub ed25519.PublicKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byKey, string(pub))
}

// listener is a running peer surface.
type listener struct {
	srv  *peerapi.Server
	self *peerNode
	addr string
	logs *syncBuffer
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func serve(t *testing.T, self *peerNode, members mtls.Membership) *listener {
	t.Helper()
	logs := &syncBuffer{}
	srv, err := peerapi.New(peerapi.Options{
		Addr:       "127.0.0.1:0",
		Material:   self.material,
		Members:    members,
		SelfPeerID: self.peerID,
		Logger:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutting the peer surface down: %v", err)
		}
	})
	return &listener{srv: srv, self: self, addr: srv.Addr(), logs: logs}
}

func (l *listener) identityURL() string { return "https://" + l.addr + peerapi.Prefix + "/identity" }

// handshakeTo dials a peer listener and reports whether the connection is
// usable — with no HTTP client anywhere near it.
//
// It exists so that "refused" cannot be confused with "answered 403". A
// refusal in this fabric destroys the connection; a 403 arrives over a
// completed TLS session, which would mean the listener accepted a key it had
// no reason to trust and then thought better of it at the HTTP layer.
//
// # Why it does not stop at Handshake()
//
// TLS 1.3 sends the client's certificate in the client's LAST flight. The
// client is finished at that point and returns from Handshake successfully;
// the server's decision — and its bad_certificate alert — arrives afterwards,
// on the first read. A test that asserted only on Handshake would therefore
// pass on a listener that accepts every key, which is the exact shape of
// mistake this whole file exists to catch. So this writes a request byte-wise
// and reads the first byte of an answer: an error anywhere in that sequence is
// a refused connection, and a byte of application data is an accepted one.
func handshakeTo(t *testing.T, addr string, cfg *tls.Config) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		// A dial failure is NOT a refusal — it means the listener is not
		// there, and a test that accepted it would pass against a server that
		// never started. That happened three times in M3.
		t.Fatalf("could not reach the peer listener at %s: %v. Nothing below would prove anything.", addr, err)
	}
	defer func() { _ = conn.Close() }()
	tlsConn := tls.Client(conn, cfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return err
	}
	if err := tlsConn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := tlsConn.Write([]byte("GET " + peerapi.Prefix + "/identity HTTP/1.1\r\nHost: peer\r\n\r\n")); err != nil {
		return err
	}
	first := make([]byte, 1)
	if _, err := tlsConn.Read(first); err != nil {
		return err
	}
	return nil
}

// dialler builds a pinned client for one node against one trust root.
func dialler(t *testing.T, self *peerNode, members mtls.Membership) *http.Client {
	t.Helper()
	c, err := mtls.Client(mtls.Options{Material: self.material, Members: members})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if tr, ok := c.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	})
	return c
}

// clientConfigFor is the TLS half of dialler, for the raw-handshake tests.
func clientConfigFor(t *testing.T, self *peerNode, members mtls.Membership) *tls.Config {
	t.Helper()
	cfg, err := mtls.ClientConfig(mtls.Options{Material: self.material, Members: members})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// peerGet issues a request and returns the status, the body and whether the
// transport reused a connection it already had.
func peerGet(t *testing.T, c *http.Client, url string) (status int, body string, reused bool, err error) {
	t.Helper()
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
	})
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if reqErr != nil {
		t.Fatal(reqErr)
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", reused, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatal(readErr)
	}
	return resp.StatusCode, string(raw), reused, nil
}

func decodeIdentity(t *testing.T, body string) peerapi.IdentityResponse {
	t.Helper()
	var out peerapi.IdentityResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("the peer surface answered something that is not an identity: %v\n%s", err, body)
	}
	return out
}

// ---------------------------------------------------------------------------
// the happy path, asserted as an identity rather than as a status

func TestTwoEnrolledPeersCompleteAMutuallyAuthenticatedRequest(t *testing.T) {
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())
	l := serve(t, a, root)

	status, body, _, err := peerGet(t, dialler(t, b, root), l.identityURL())
	if err != nil {
		t.Fatalf("two enrolled peers could not talk: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("GET identity = %d\n%s", status, body)
	}

	got := decodeIdentity(t, body)
	// The assertion the issue asks for, and the reason this endpoint exists.
	// "It returned 200" would also be true of a listener that authenticated
	// nobody; this is the identity the SERVER worked out from the certificate.
	if got.PeerID != b.peerID {
		t.Errorf("the server derived peer id %q from the certificate, want %q — the registering "+
			"peer's id", got.PeerID, b.peerID)
	}
	if got.Name != b.name {
		t.Errorf("the server derived name %q, want %q", got.Name, b.name)
	}
	if got.PublicKey != identity.FormatPublicKey(b.pub) {
		t.Errorf("the server pinned %q, want %q", got.PublicKey, identity.FormatPublicKey(b.pub))
	}
	if got.ServedBy != a.peerID {
		t.Errorf("served_by = %q, want the listening node %q", got.ServedBy, a.peerID)
	}
	// And it is not an echo: nothing in the request said any of this.
	if strings.Contains(l.identityURL(), b.peerID) {
		t.Fatal("the request URL carries the peer id, so the response could be an echo of it")
	}
}

// TestTheDiallerPinsTheListenerToo: mutual means mutual.
//
// Without this, a client that verified nothing would pass every test above,
// and the day a DNS record moved it would stream a complete library to
// whatever answered on that port.
func TestTheDiallerPinsTheListenerToo(t *testing.T) {
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	serverRoot := newTrustRoot(a.member(), b.member())
	l := serve(t, a, serverRoot)

	// The control: with the listener enrolled at this end, the call works.
	if _, _, _, err := peerGet(t, dialler(t, b, serverRoot), l.identityURL()); err != nil {
		t.Fatalf("the control request failed, so the refusal below would prove nothing: %v", err)
	}

	// The dialler's own trust root knows b but not the node it is calling.
	clientRoot := newTrustRoot(b.member())
	err := handshakeTo(t, l.addr, clientConfigFor(t, b, clientRoot))
	if err == nil {
		t.Fatal("the dialler completed a handshake with a listener whose key it does not pin")
	}
	if !strings.Contains(err.Error(), "not a member") {
		t.Errorf("the dialler's refusal does not say why: %v", err)
	}
}

// ---------------------------------------------------------------------------
// the refusals, each at the connection level

func TestAKeyThatIsNotAMemberIsRefusedDuringTheHandshake(t *testing.T) {
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	stranger := newPeerNode(t, "stranger-id", "stranger")
	root := newTrustRoot(a.member(), b.member())
	l := serve(t, a, root)

	// The control. Without it, everything below passes on a listener that
	// refuses everybody, which is the other way this mechanism fails.
	if err := handshakeTo(t, l.addr, clientConfigFor(t, b, root)); err != nil {
		t.Fatalf("an enrolled peer could not complete a handshake: %v", err)
	}

	// The stranger's own trust root pins the listener, so the ONLY thing that
	// can refuse this connection is the listener refusing the stranger.
	strangerRoot := newTrustRoot(a.member())
	err := handshakeTo(t, l.addr, clientConfigFor(t, stranger, strangerRoot))
	if err == nil {
		t.Fatal("a key that is not a member completed a handshake. A 403 later would be too late: " +
			"the session exists, and every route that forgets to check is reachable over it")
	}

	// And the listener said which check failed, on its own side, where the
	// answer is safe to state. The far end is told nothing on purpose.
	if logs := l.logs.String(); !strings.Contains(logs, "not a member") {
		t.Errorf("the listener refused without recording why:\n%s", logs)
	}
	if strings.Contains(err.Error(), identity.FormatPublicKey(a.pub)) {
		t.Error("the refusal told an unauthenticated caller about the membership table")
	}
}

func TestNoClientCertificateAtAllIsRefusedDuringTheHandshake(t *testing.T) {
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())
	l := serve(t, a, root)

	if err := handshakeTo(t, l.addr, clientConfigFor(t, b, root)); err != nil {
		t.Fatalf("an enrolled peer could not complete a handshake: %v", err)
	}

	// A client that verifies nothing and presents nothing. It skips
	// verification of the LISTENER deliberately: this test is about what the
	// listener does with an anonymous connection, and a client-side refusal
	// would mask it.
	anonymous := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, //nolint:gosec // the anonymous client under test; the listener is the subject
	}
	if err := handshakeTo(t, l.addr, anonymous); err == nil {
		t.Fatal("a connection presenting no client certificate was accepted. There is no key to " +
			"pin on such a connection, so nothing later could have refused it for the right reason")
	}
}

// TestASubstitutedKeyIsRefused is the case that is NOT the unknown-key case.
//
// The peer is a member. Its name is right, its id is right, its certificate
// says so — and the key inside it is not the key its membership record pins.
// That is what a pin is for, and it is the case a check written against the
// certificate's subject would sail straight through.
func TestASubstitutedKeyIsRefused(t *testing.T) {
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())
	l := serve(t, a, root)

	// The control: peer-b IS a member, so the refusal below is about the key
	// and not about peer-b being a stranger. This is the whole distinction.
	status, body, _, err := peerGet(t, dialler(t, b, root), l.identityURL())
	if err != nil || status != http.StatusOK {
		t.Fatalf("peer-b is not actually a member (%d %v), so the substitution below proves nothing", status, err)
	}
	if got := decodeIdentity(t, body); got.PeerID != b.peerID {
		t.Fatalf("the control resolved %q, want %q", got.PeerID, b.peerID)
	}

	// Now the same peer id, the same name, a different key.
	substituted := newPeerNode(t, b.peerID, b.name)
	if substituted.pub.Equal(b.pub) {
		t.Fatal("the substituted key is the registered key")
	}
	substitutedRoot := newTrustRoot(a.member())
	if err := handshakeTo(t, l.addr, clientConfigFor(t, substituted, substitutedRoot)); err == nil {
		t.Fatal("a certificate naming a member and carrying a different key was accepted. " +
			"The pin is on the key, not on the name (ADR-0012)")
	}
	if logs := l.logs.String(); !strings.Contains(logs, "not a member") {
		t.Errorf("the listener did not record the substituted key as a membership refusal:\n%s", logs)
	}
}

func TestAnExpiredCertificateIsRefusedAndARegeneratedOneIsAccepted(t *testing.T) {
	a := newPeerNode(t, "peer-a-id", "peer-a")

	// A node whose clock — and therefore whose certificate — is two hours
	// behind. Its material has a one-hour lifetime, so what it presents is
	// genuinely expired from the listener's point of view rather than merely
	// declared so.
	stale := time.Now().UTC().Add(-2 * time.Hour)
	now := stale
	b := newPeerNodeAt(t, "peer-b-id", "peer-b", func() time.Time { return now })
	root := newTrustRoot(a.member(), b.member())
	l := serve(t, a, root)

	if err := handshakeTo(t, l.addr, clientConfigFor(t, b, root)); err == nil {
		t.Fatal("an expired certificate was accepted")
	}
	if logs := l.logs.String(); !strings.Contains(logs, "has expired") {
		t.Errorf("the listener refused an expired certificate without saying so:\n%s", logs)
	}

	// The clock catches up; the material regenerates in place, from the same
	// private key. It is the same peer to everyone that enrolled it, because
	// ADR-0012 pins keys and not certificates — which is exactly why renewal
	// can be automatic and needs no operator.
	now = time.Now().UTC()
	status, body, _, err := peerGet(t, dialler(t, b, root), l.identityURL())
	if err != nil {
		t.Fatalf("a regenerated certificate was refused: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("after regeneration: %d\n%s", status, body)
	}
	if got := decodeIdentity(t, body); got.PeerID != b.peerID {
		t.Errorf("after regeneration the server derived %q, want %q", got.PeerID, b.peerID)
	}
}

// ---------------------------------------------------------------------------
// the two credentials do not cross

// TestABearerTokenIsNotAPeerCredential asserts both halves of "not
// sufficient": a token gets a caller nowhere here, and its absence costs an
// enrolled peer nothing.
func TestABearerTokenIsNotAPeerCredential(t *testing.T) {
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())
	l := serve(t, a, root)

	// Not sufficient: a perfectly good bearer token, no certificate. It never
	// reaches a handler — the listener will not complete a handshake with a
	// caller that presents no key, so the header is never parsed at all.
	tokenOnly := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS13,
			InsecureSkipVerify: true, //nolint:gosec // the token-only client under test
		}},
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, l.identityURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer heyarr_anadminstoken_withadminscope")
	resp, err := tokenOnly.Do(req) //nolint:bodyclose // resp is nil on the expected path
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("a bearer token authenticated against the peer surface: %d", resp.StatusCode)
	}

	// Not necessary: the enrolled peer sends no Authorization header at all.
	status, body, _, err := peerGet(t, dialler(t, b, root), l.identityURL())
	if err != nil || status != http.StatusOK {
		t.Fatalf("an enrolled peer with no bearer token was refused: %d %v\n%s", status, err, body)
	}
}

// ---------------------------------------------------------------------------
// membership is consulted per connection AND per request

func TestMembershipIsConsultedPerConnectionAndPerRequest(t *testing.T) {
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())
	l := serve(t, a, root)
	c := dialler(t, b, root)

	status, body, _, err := peerGet(t, c, l.identityURL())
	if err != nil || status != http.StatusOK {
		t.Fatalf("the first request failed: %d %v\n%s", status, err, body)
	}
	afterFirst := root.lookups.Load()
	if afterFirst < 2 {
		t.Fatalf("membership was consulted %d time(s) for one handshake and one request; it must be "+
			"asked for both, because a connection outlives a revocation", afterFirst)
	}

	// A second request on the connection the first one opened.
	status, body, reused, err := peerGet(t, c, l.identityURL())
	if err != nil || status != http.StatusOK {
		t.Fatalf("the second request failed: %d %v\n%s", status, err, body)
	}
	if !reused {
		t.Fatal("the transport did not reuse its connection, so the revocation below would be " +
			"about a NEW connection and would say far less than it appears to")
	}
	afterSecond := root.lookups.Load()
	if afterSecond <= afterFirst {
		t.Fatal("membership was not consulted for a request on an existing connection — the answer " +
			"was cached, and a removed peer would keep reading for as long as it held the socket")
	}

	// Revoke, which in this design means delete: there is no revocation list
	// to add to (ADR-0012).
	root.remove(b.pub)

	status, body, reused, err = peerGet(t, c, l.identityURL())
	if err != nil {
		// A transport-level failure here is acceptable only if it is not the
		// connection simply having gone away, which would prove nothing.
		t.Fatalf("after revocation the request failed at the transport (%v) rather than being "+
			"refused on the open connection", err)
	}
	if status != http.StatusForbidden {
		t.Fatalf("after revocation: %d, want 403 — a peer whose record is gone is still being "+
			"served on the connection it already had\n%s", status, body)
	}
	if !reused {
		t.Error("the refused request went over a NEW connection, so this run does not show that an " +
			"already-open connection was severed")
	}
	if !strings.Contains(body, "not a member") {
		t.Errorf("the refusal does not say why: %s", body)
	}

	// And a fresh connection is refused at the handshake, which is the same
	// fact from the other side.
	err = handshakeTo(t, l.addr, clientConfigFor(t, b, root))
	if err == nil {
		t.Fatal("a revoked peer completed a new handshake")
	}
	if logs := l.logs.String(); !strings.Contains(logs, "not a member") {
		t.Errorf("the listener did not record the revoked peer's refusal:\n%s", logs)
	}
}
