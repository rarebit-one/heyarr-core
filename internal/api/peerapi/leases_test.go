package peerapi_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/grant"
	"github.com/rarebit-one/heyarr-core/internal/leases"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

var leaseNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

type leaseClock struct{ t time.Time }

func (c leaseClock) Now() time.Time { return c.t }

// stubLeaseSource is a fixed token list, for the route's own behaviour.
type stubLeaseSource struct {
	tokens []string
	err    error
}

func (s stubLeaseSource) ActiveLeaseTokens(context.Context) ([]string, error) {
	return s.tokens, s.err
}

// siblingSet is a static leases.SiblingKeys for the honouring side.
type siblingSet map[string]ed25519.PublicKey

func (s siblingSet) PeerKeys(context.Context) (map[string]ed25519.PublicKey, error) { return s, nil }

// realLeaseStore builds a leases.Store on a temp DB, signed by signer, pinning
// the given siblings — the same shape the controller wires.
func realLeaseStore(t *testing.T, signer ed25519.PrivateKey, siblings leases.SiblingKeys) *leases.Store {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	clock := leaseClock{t: leaseNow}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	store, err := leases.New(leases.Options{
		Writer: db.Writer(), Reader: db.Reader(), Events: log, Signer: signer, Siblings: siblings, Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// leasesURL is the peer-surface leases route for a listener.
func leasesURL(l *listener) string { return "https://" + l.addr + peerapi.Prefix + "/leases" }

func TestLeasesRouteServesActiveTokensToAMember(t *testing.T) {
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())
	l := serveLeases(t, a, root, stubLeaseSource{tokens: []string{"tok-1", "tok-2"}})

	status, body, _, err := peerGet(t, dialler(t, b, root), leasesURL(l))
	if err != nil {
		t.Fatalf("a member could not fetch leases: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("GET leases = %d\n%s", status, body)
	}
	var got struct {
		Leases []string `json:"leases"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("leases body is not decodable: %v\n%s", err, body)
	}
	if len(got.Leases) != 2 || got.Leases[0] != "tok-1" || got.Leases[1] != "tok-2" {
		t.Fatalf("served the wrong tokens: %v", got.Leases)
	}
}

func TestLeasesRouteAnswers503WithNoIssuer(t *testing.T) {
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())
	l := serve(t, a, root) // no Leases source wired

	status, _, _, err := peerGet(t, dialler(t, b, root), leasesURL(l))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("a node that issues no leases should answer 503, got %d", status)
	}
}

func TestANonMemberCannotFetchLeases(t *testing.T) {
	a := newPeerNode(t, "peer-a-id", "peer-a")
	stranger := newPeerNode(t, "stranger-id", "stranger")
	root := newTrustRoot(a.member()) // stranger is NOT enrolled
	l := serveLeases(t, a, root, stubLeaseSource{tokens: []string{"tok-1"}})

	if _, _, _, err := peerGet(t, dialler(t, stranger, root), leasesURL(l)); err == nil {
		t.Fatal("a non-member reached the leases route; the mTLS handshake should have refused it")
	}
}

// The end-to-end cross-site property: peer A serves a REAL lease over the peer
// surface; peer B fetches it, then honours it with A's server SHUT DOWN — B
// reaches nobody, and the signature against A's pinned key is the whole
// authority (ADR-0048). It still expires on B's own clock.
func TestBFetchesAndHonoursASiblingsLeaseWithTheIssuerDown(t *testing.T) {
	ctx := context.Background()

	// Peer A's lease issuer key, and a store that has issued one lease.
	_, issuerA, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	aStore := realLeaseStore(t, issuerA, nil)
	lease, err := aStore.Issue(ctx, "user-x", "asset-1", []grant.Capability{grant.CapabilityRead}, time.Hour)
	if err != nil {
		t.Fatalf("A issue: %v", err)
	}

	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())
	l := serveLeases(t, a, root, aStore)

	// B fetches A's active leases over the pinned link.
	status, body, _, err := peerGet(t, dialler(t, b, root), leasesURL(l))
	if err != nil || status != http.StatusOK {
		t.Fatalf("B could not fetch A's leases: status=%d err=%v", status, err)
	}
	var fetched struct {
		Leases []string `json:"leases"`
	}
	if err := json.Unmarshal([]byte(body), &fetched); err != nil {
		t.Fatal(err)
	}
	if len(fetched.Leases) != 1 || fetched.Leases[0] != lease.Token {
		t.Fatalf("B fetched the wrong tokens: %v", fetched.Leases)
	}

	// B pins A's issuer key as a sibling, and shuts A's server down.
	_, keyB, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	idA := identity.FormatPublicKey(issuerA.Public().(ed25519.PublicKey))
	bStore := realLeaseStore(t, keyB, siblingSet{idA: issuerA.Public().(ed25519.PublicKey)})

	shutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := l.srv.Shutdown(shutCtx); err != nil {
		t.Fatalf("shutting A down: %v", err)
	}

	// B honours the fetched lease with A unreachable — the signature is enough.
	req := grant.Request{Principal: "user-x", Resource: "asset-1", Capability: grant.CapabilityRead}
	if _, err := bStore.Honour(ctx, fetched.Leases[0], req, leaseNow); err != nil {
		t.Fatalf("B should honour A's cached lease with A down, got %v", err)
	}
	// And it still expires on B's own clock.
	if _, err := bStore.Honour(ctx, fetched.Leases[0], req, leaseNow.Add(2*time.Hour)); err == nil {
		t.Fatal("B should refuse the expired lease")
	}
}

// serveLeases constructs a peer surface with a Leases source. It mirrors
// serveWithSnapshots in mtls_test.go.
func serveLeases(t *testing.T, self *peerNode, members mtls.Membership, src peerapi.LeaseSource) *listener {
	t.Helper()
	logs := &syncBuffer{}
	srv, err := peerapi.New(peerapi.Options{
		Addr:       "127.0.0.1:0",
		Material:   self.material,
		Members:    members,
		SelfPeerID: self.peerID,
		Leases:     src,
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
		// Shutdown may already have been called by a test (the issuer-down
		// case); ignore the resulting error here.
		_ = srv.Shutdown(ctx)
	})
	return &listener{srv: srv, self: self, addr: srv.Addr(), logs: logs}
}
