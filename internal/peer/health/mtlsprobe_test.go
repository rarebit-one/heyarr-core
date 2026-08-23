// Every response here is closed by the prober itself, which bodyclose cannot
// see through.
//
//nolint:bodyclose // the prober closes what it opens; these tests never hold a response
package health_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/peer/health"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
)

// The probe, over the fabric it is probing (#184).
//
// # What these assert, and why it is not "no error was returned"
//
// M4 shipped five TLS refusal tests that passed against a listener accepting
// every key. TLS 1.3 puts the client's certificate in the client's LAST
// flight: the client is finished at that point and Handshake returns nil,
// and the server's verdict arrives afterwards. A test that asserted only that
// the client saw no error would therefore pass on a server that verified
// nothing, which is the entire failure mode this fabric exists to prevent.
//
// So the handshake assertion here is made from the SERVER: the request
// arrived, the connection reports HandshakeComplete, it carries exactly one
// peer certificate, and the key in it is the PROBING node's key. A server that
// authenticated nobody has no key to report.

// observation is what a peer listener saw. Every field is written under the
// mutex because the handler runs on the server's goroutine.
type observation struct {
	mu                sync.Mutex
	requests          int
	handshakeComplete bool
	resumed           bool
	version           uint16
	clientKeys        []ed25519.PublicKey
	paths             []string
}

func (o *observation) snapshot() observation {
	o.mu.Lock()
	defer o.mu.Unlock()
	return observation{
		requests: o.requests, handshakeComplete: o.handshakeComplete,
		resumed: o.resumed, version: o.version,
		clientKeys: append([]ed25519.PublicKey(nil), o.clientKeys...),
		paths:      append([]string(nil), o.paths...),
	}
}

// probeNode is one node's identity and the certificate it presents.
type probeNode struct {
	peerID   string
	name     string
	pub      ed25519.PublicKey
	material *mtls.Material
}

func newProbeNode(t *testing.T, peerID, name string) *probeNode {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	material, err := mtls.NewMaterial(mtls.MaterialOptions{
		PrivateKey: priv, PeerID: peerID,
		Lifetime: time.Hour, RenewBefore: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &probeNode{peerID: peerID, name: name, pub: pub, material: material}
}

// probeTrust is a membership of a fixed set of keys, as the transport consults
// it. A key that is not in it is not a member, which is the only refusal
// ADR-0012 has.
type probeTrust struct{ byKey map[string]mtls.Peer }

func newProbeTrust(nodes ...*probeNode) probeTrust {
	r := probeTrust{byKey: map[string]mtls.Peer{}}
	for _, n := range nodes {
		r.byKey[string(n.pub)] = mtls.Peer{PeerID: n.peerID, Name: n.name, PublicKey: n.pub}
	}
	return r
}

func (r probeTrust) Lookup(_ context.Context, pub []byte) (mtls.Peer, error) {
	p, ok := r.byKey[string(pub)]
	if !ok {
		return mtls.Peer{}, mtls.ErrNotAMember
	}
	return p, nil
}

// peerListener serves an mTLS listener shaped exactly like a peer surface —
// TLS 1.3, RequireAnyClientCert, pinned by membership — and records what each
// connection proved.
//
// It is built from mtls.ServerConfig rather than from peerapi.Server so that
// the handler can report the connection state back into the test. What is
// under test is the DIAL, and the thing worth asserting is what the far end
// verified.
func peerListener(t *testing.T, self *probeNode, members mtls.Membership, status int) (string, *observation) {
	t.Helper()
	cfg, err := mtls.ServerConfig(mtls.Options{Material: self.material, Members: members})
	if err != nil {
		t.Fatal(err)
	}
	obs := &observation{}
	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		TLSConfig:         cfg,
		// The refusal tests below deliberately fail handshakes, and the
		// server's chatter about them is not a test result.
		ErrorLog: log.New(io.Discard, "", 0),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			obs.mu.Lock()
			obs.requests++
			obs.paths = append(obs.paths, r.URL.Path)
			if r.TLS != nil {
				obs.handshakeComplete = r.TLS.HandshakeComplete
				obs.resumed = r.TLS.DidResume
				obs.version = r.TLS.Version
				for _, cert := range r.TLS.PeerCertificates {
					if key, ok := cert.PublicKey.(ed25519.PublicKey); ok {
						obs.clientKeys = append(obs.clientKeys, key)
					}
				}
			}
			obs.mu.Unlock()
			w.WriteHeader(status)
		}),
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.ServeTLS(l, "", "") }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return l.Addr().String(), obs
}

func proberFor(node *probeNode) health.MTLSProber {
	return health.MTLSProber{
		Material: node.material,
		Timeout:  10 * time.Second,
		Logger:   slog.New(slog.DiscardHandler),
	}
}

// THE HANDSHAKE ASSERTION. Not "Probe returned nil" — that is what the M4
// tests asserted, and it passes against a server that pins nothing.
func TestTheProbeCompletesAMutuallyAuthenticatedHandshake(t *testing.T) {
	server := newProbeNode(t, "01990000-0000-7000-8000-0000000000s1", "site-b")
	client := newProbeNode(t, "01990000-0000-7000-8000-0000000000c1", "site-a")
	addr, obs := peerListener(t, server, newProbeTrust(server, client), http.StatusOK)

	err := proberFor(client).Probe(context.Background(), health.Peer{
		PeerID: server.peerID, Name: server.name,
		Endpoint: "https://" + addr, PublicKey: server.pub,
	})
	if err != nil {
		t.Fatalf("probing a peer surface: %v", err)
	}

	got := obs.snapshot()
	if got.requests != 1 {
		t.Fatalf("the listener served %d requests, want 1 — nothing reached it, so nothing "+
			"about the handshake is proven", got.requests)
	}
	if !got.handshakeComplete {
		t.Error("the connection does not report a completed handshake")
	}
	if got.resumed {
		t.Error("the connection was resumed from a ticket, so no certificate was exchanged on it " +
			"and this test would be asserting about an earlier connection")
	}
	if got.version != tls.VersionTLS13 {
		t.Errorf("negotiated TLS version = %#04x, want TLS 1.3 (%#04x) — ADR-0012 pins it",
			got.version, tls.VersionTLS13)
	}
	// The one that cannot pass on a listener accepting every key: the SERVER
	// holds the probing node's certificate, which only happens if the client
	// presented one and it was verified.
	if len(got.clientKeys) != 1 {
		t.Fatalf("the listener saw %d ed25519 client certificates, want exactly 1 — a probe that "+
			"presented none would have completed a one-way TLS session", len(got.clientKeys))
	}
	if !got.clientKeys[0].Equal(client.pub) {
		t.Error("the key the listener verified is not the probing node's key")
	}
	if got.paths[0] != "/peer/v1/identity" {
		t.Errorf("the probe asked for %q, want the peer surface's identity route", got.paths[0])
	}
}

// The regression, stated as a test rather than as a sentence in an issue: the
// plain-HTTPS prober cannot speak to a peer surface at all. This is why a
// remote peer's health could never move.
func TestThePlainProberCannotCompleteAPeerSurfaceHandshake(t *testing.T) {
	server := newProbeNode(t, "01990000-0000-7000-8000-0000000000s2", "site-b")
	client := newProbeNode(t, "01990000-0000-7000-8000-0000000000c2", "site-a")
	addr, obs := peerListener(t, server, newProbeTrust(server, client), http.StatusOK)

	peer := health.Peer{
		PeerID: server.peerID, Name: server.name,
		Endpoint: "https://" + addr, PublicKey: server.pub,
	}
	if err := (health.HTTPProber{Timeout: 5 * time.Second}).Probe(context.Background(), peer); err == nil {
		t.Fatal("the plain prober reported a peer surface as answering; it presents no client " +
			"certificate and an mTLS listener will not complete a handshake with it")
	}
	if got := obs.snapshot(); got.requests != 0 {
		t.Errorf("the listener served %d plain-HTTPS requests, want 0", got.requests)
	}

	// And the pinned one, against the same listener, in the same test, so the
	// contrast is not an artefact of two different fixtures.
	if err := proberFor(client).Probe(context.Background(), peer); err != nil {
		t.Fatalf("the pinned prober could not reach the same listener: %v", err)
	}
	if got := obs.snapshot(); got.requests != 1 {
		t.Errorf("the listener served %d pinned requests, want 1", got.requests)
	}
}

// Any answer is reachability, including a refusal. A peer that has revoked us
// is a peer that is up, and reporting it as an outage would be an
// authentication problem wearing an outage's clothes.
func TestTheProbeTreatsEveryStatusAsAnAnswer(t *testing.T) {
	for _, status := range []int{
		http.StatusOK, http.StatusForbidden, http.StatusNotFound,
		http.StatusInternalServerError, http.StatusServiceUnavailable,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := newProbeNode(t, "01990000-0000-7000-8000-0000000000s3", "site-b")
			client := newProbeNode(t, "01990000-0000-7000-8000-0000000000c3", "site-a")
			addr, obs := peerListener(t, server, newProbeTrust(server, client), status)

			if err := proberFor(client).Probe(context.Background(), health.Peer{
				PeerID: server.peerID, Name: server.name,
				Endpoint: "https://" + addr, PublicKey: server.pub,
			}); err != nil {
				t.Fatalf("a peer answering %d was reported as unreachable: %v", status, err)
			}
			if got := obs.snapshot(); got.requests != 1 {
				t.Errorf("requests = %d, want 1", got.requests)
			}
		})
	}
}

// Silence is the only unreachability, and a refused handshake is silence: the
// far end tells the caller nothing, by design.
func TestAPeerThatDoesNotPinUsIsSilence(t *testing.T) {
	server := newProbeNode(t, "01990000-0000-7000-8000-0000000000s4", "site-b")
	client := newProbeNode(t, "01990000-0000-7000-8000-0000000000c4", "site-a")
	// The server's trust root does not carry the client's key: revocation is
	// deletion, and this is what it looks like from the other side.
	addr, obs := peerListener(t, server, newProbeTrust(server), http.StatusOK)

	if err := proberFor(client).Probe(context.Background(), health.Peer{
		PeerID: server.peerID, Name: server.name,
		Endpoint: "https://" + addr, PublicKey: server.pub,
	}); err == nil {
		t.Fatal("a peer that refused the handshake was reported as answering")
	}
	if got := obs.snapshot(); got.requests != 0 {
		t.Errorf("the refused connection still reached the handler %d times", got.requests)
	}
}

// A peer with no pinned key is refused BEFORE a socket exists. Dialling an
// address and believing whatever answered is trust on first use, and this
// probe's answer decides whether a peer is offered as a read source.
func TestTheProbeRefusesToDialAPeerWithNoPinnedKey(t *testing.T) {
	server := newProbeNode(t, "01990000-0000-7000-8000-0000000000s5", "site-b")
	client := newProbeNode(t, "01990000-0000-7000-8000-0000000000c5", "site-a")
	addr, obs := peerListener(t, server, newProbeTrust(server, client), http.StatusOK)

	err := proberFor(client).Probe(context.Background(), health.Peer{
		PeerID: server.peerID, Name: server.name, Endpoint: "https://" + addr,
	})
	if !errors.Is(err, health.ErrNoPinnedKey) {
		t.Fatalf("error = %v, want ErrNoPinnedKey", err)
	}
	if got := obs.snapshot(); got.requests != 0 {
		t.Errorf("a peer with no pinned key was dialled anyway (%d requests)", got.requests)
	}
}

// An endpoint that is not a peer surface still works. A single-host deployment
// derives unix:// for itself and a development topology uses http://; neither
// is mutually authenticated TLS, and a prober that could only speak the fabric
// would report both as permanently unreachable.
func TestANonFabricEndpointIsProbedAsItWasBefore(t *testing.T) {
	var served int
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(plain.Close)
	client := newProbeNode(t, "01990000-0000-7000-8000-0000000000c6", "site-a")

	if err := proberFor(client).Probe(context.Background(), health.Peer{
		PeerID: "01990000-0000-7000-8000-0000000000s6", Name: "dev", Endpoint: plain.URL,
	}); err != nil {
		t.Fatalf("probing an http:// endpoint: %v", err)
	}
	if served != 1 {
		t.Errorf("the plain endpoint served %d requests, want 1", served)
	}
}

// A peer with no endpoint is a configuration gap, not an outage.
func TestTheProbeReportsAPeerWithNoEndpoint(t *testing.T) {
	client := newProbeNode(t, "01990000-0000-7000-8000-0000000000c7", "site-a")
	err := proberFor(client).Probe(context.Background(), health.Peer{
		PeerID: "01990000-0000-7000-8000-0000000000s7", Name: "keyed-only",
	})
	if !errors.Is(err, health.ErrNoEndpoint) {
		t.Fatalf("error = %v, want ErrNoEndpoint", err)
	}
}

// THE TRANSITION, through the sweep, against a real listener and a real
// database: a remote peer nothing has ever heard from is probed over the
// fabric and becomes reachable.
//
// It asserts `unknown` FIRST. Without that, the test would pass on a build
// where the column was reachable before anything happened, and it would be
// measuring nothing.
func TestAnIdleRemotePeerBecomesReachableThroughTheFabricProbe(t *testing.T) {
	server := newProbeNode(t, "01990000-0000-7000-8000-0000000000s8", "site-b")
	client := newProbeNode(t, "01990000-0000-7000-8000-0000000000c8", "site-a")
	addr, obs := peerListener(t, server, newProbeTrust(server, client), http.StatusOK)

	f, tracker := newFixture(t, proberFor(client))
	res, err := f.peers.Register(context.Background(), membership.Registration{
		Name: "site-b", Site: "site-b", Endpoint: "https://" + addr, PublicKey: server.pub,
	})
	if err != nil {
		t.Fatal(err)
	}
	peerID := res.Member.PeerID

	if got := stateOf(t, tracker, peerID); got != health.StateUnknown {
		t.Fatalf("health before the sweep = %q, want %q — the transition is what is under test, "+
			"and a peer that started reachable would prove nothing", got, health.StateUnknown)
	}

	sum := sweep(t, tracker)
	if sum.Probed != 1 {
		t.Errorf("the sweep probed %d peers, want 1", sum.Probed)
	}
	if got := stateOf(t, tracker, peerID); got != health.StateReachable {
		t.Fatalf("health after the sweep = %q, want %q", got, health.StateReachable)
	}
	if got := obs.snapshot(); got.requests != 1 || len(got.clientKeys) != 1 {
		t.Errorf("the peer listener saw %d requests and %d client keys, want 1 and 1",
			got.requests, len(got.clientKeys))
	}
	if got := f.transitions(peerID); len(got) != 1 || got[0] != [2]string{"unknown", "reachable"} {
		t.Errorf("transitions = %v, want exactly one unknown -> reachable", got)
	}
}
