package membership_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/resources"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// This file holds the single assertion issue #136 is really about.
//
// ADR-0012 says "Revocation is removing a membership record." That is only
// true if removing the record severs access, and it is only observably true if
// the access being severed was demonstrably working a moment earlier — on the
// same connection, not on a fresh one. A test that removed a peer and then
// asserted 403 would pass unchanged on a node where peer reads never worked at
// all, which is the most common way this kind of test is wrong.
//
// So the shape here is: read bytes successfully, prove the connection was
// reused, remove the peer, read again on that same connection, and require
// that the second read is refused AND that it happened on a connection the
// transport had already established.
//
// M4-05 replaces presentedKey below with the peer's verified mTLS certificate.
// Nothing else in this test changes when it does, which is the point of the
// seam.

// peerNode is a running Heyarr API with a CAS behind it and membership wired
// as the trust root.
type peerNode struct {
	t      *testing.T
	http   *httptest.Server
	store  *membership.Store
	cas    *cas.FS
	lookup *countingMembership
}

// countingMembership wraps the real store and counts how often the request
// path consulted it.
//
// The count is the sabotage detector's other half: a cached guard still
// returns the right answer for a peer that was never removed, and the thing
// that distinguishes it from a correct one is that it stops asking.
type countingMembership struct {
	inner  *membership.Store
	checks atomic.Int64
}

func (c *countingMembership) IsMember(ctx context.Context, pub []byte) (bool, error) {
	c.checks.Add(1)
	return c.inner.IsMember(ctx, pub)
}

func newPeerNode(t *testing.T, presented httpapi.PresentedPeerKey) *peerNode {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(dir, "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	clock := &fixedClock{t: fixedTime}
	quiet := slog.New(slog.DiscardHandler)
	tokens, err := auth.NewStore(auth.StoreOptions{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := auth.NewVerifier(auth.VerifierOptions{Store: tokens})
	if err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(jobs.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock, Events: eventLog})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{
		DB: db, Events: eventLog, PeerName: "this-node", PeerSite: "site-a", Clock: clock, Logger: quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.SelfPeer(ctx); err != nil {
		t.Fatal(err)
	}
	store, err := membership.New(membership.Options{DB: db, Events: eventLog, Clock: clock, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	counting := &countingMembership{inner: store}

	api, err := resources.New(resources.Options{
		DB: db, Jobs: queue, Events: eventLog, Tokens: tokens, Catalog: cat,
		Membership: store, Logger: quiet, Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	blobStore, err := cas.OpenFS(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	blobHandler, err := blobs.New(blobs.Options{Store: blobStore, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = ""
	cfg.HTTP.Auth.Enabled = false

	srv, err := httpapi.New(httpapi.Options{
		Config: cfg, Logger: quiet, DB: db, Verifier: verifier, Events: eventLog,
		Build:              buildinfo.Info{Version: "test"},
		SchemaVersion:      20,
		KnownSchemaVersion: 20,
		CASRoot:            blobStore.Root(),
		Mount:              []httpapi.MountFunc{api.Mount, blobHandler.Mount},
		PeerMembership:     counting,
		PresentedPeerKey:   presented,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &peerNode{t: t, http: ts, store: store, cas: blobStore, lookup: counting}
}

// tracedGet issues a request and reports the status, the body and whether the
// transport reused an existing connection.
//
// The reuse flag is what makes this test say what it claims to say. Without
// it, "the removed peer can no longer read" is indistinguishable from "a fresh
// connection from a removed peer is refused" — and the second is a much weaker
// property, because a peer with an open connection is exactly the peer an
// operator is revoking in a hurry.
func tracedGet(t *testing.T, c *http.Client, url string) (status int, body string, reused bool) {
	t.Helper()
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw), reused
}

func TestRemovingAPeerSeversAConnectionThatWasReadingBytes(t *testing.T) {
	// The key the connection presents. In production this is the public key in
	// the peer's mTLS client certificate (M4-05); here it is handed to the
	// server through the same seam that will read it.
	peerPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	node := newPeerNode(t, func(*http.Request) ([]byte, bool) { return peerPub, true })

	// Bytes for the peer to read.
	const content = "the bytes a revoked peer must stop being able to read"
	desc, err := node.cas.Put(context.Background(), strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	blobURL := node.http.URL + "/api/v1/blobs/" + desc.Hash.String() + "/content"

	// One client, keep-alives on, so the second and third requests go over the
	// connection the first one opened.
	transport := &http.Transport{
		MaxIdleConnsPerHost: 1,
		IdleConnTimeout:     time.Minute,
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport}

	// Enrol the peer.
	enrolled, err := node.store.Register(context.Background(), membership.Registration{
		Name: "peer-b", Site: "site-b", Endpoint: "https://b.example:8385", PublicKey: peerPub,
	})
	if err != nil {
		t.Fatal(err)
	}

	// --- Reproduce the working case, before anything is removed. ---
	status, body, _ := tracedGet(t, client, blobURL)
	if status != http.StatusOK {
		t.Fatalf("before removal: GET blob = %d, want 200. The failure below would prove "+
			"nothing if reads never worked:\n%s", status, body)
	}
	if body != content {
		t.Fatalf("before removal: the peer read %q, want %q", body, content)
	}

	// A second read, to establish that the transport is actually reusing the
	// connection. If it is not, the revocation assertion below is about a new
	// connection and says less than it appears to.
	status, body, reused := tracedGet(t, client, blobURL)
	if status != http.StatusOK || body != content {
		t.Fatalf("the second read before removal failed: %d %q", status, body)
	}
	if !reused {
		t.Fatal("the client did not reuse its connection, so this test cannot show that " +
			"revocation severs an ALREADY OPEN connection")
	}

	checksBefore := node.lookup.checks.Load()
	if checksBefore < 2 {
		t.Fatalf("membership was consulted %d times for 2 requests — it is not being checked "+
			"per request", checksBefore)
	}

	// --- Revoke, through the API an operator would use. ---
	req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete,
		node.http.URL+"/api/v1/peers/"+enrolled.Member.PeerID, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// Drained before closing, so the connection goes back to the pool. An
	// undrained body means the transport discards the connection, and the read
	// below would then open a fresh one — which is precisely the weaker
	// property this test refuses to settle for.
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /peers = %d, want 200", resp.StatusCode)
	}

	// --- The same connection, immediately after. ---
	status, body, reused = tracedGet(t, client, blobURL)
	if status != http.StatusForbidden {
		t.Fatalf("after removal: GET blob = %d, want 403 — a peer whose membership record is "+
			"gone is still reading bytes (ADR-0012)\n%s", status, body)
	}
	if !reused {
		t.Error("the refused request went over a NEW connection, so this run does not show " +
			"that an already-open connection was severed")
	}
	if !strings.Contains(body, "not a member") {
		t.Errorf("the refusal does not say why: %s", body)
	}
	if node.lookup.checks.Load() <= checksBefore {
		t.Error("membership was not consulted for the request after removal — the answer was cached")
	}

	// And the bytes are genuinely gone from this peer's reach: not a stale
	// body, not a cached 200.
	if strings.Contains(body, content) {
		t.Error("the refused response carried the blob's bytes")
	}
}

// TestANonPeerConnectionIsUnaffected: the guard must not turn every ordinary
// client into a peer that has to be enrolled.
//
// Without this, a guard that refused everything would pass the test above.
func TestANonPeerConnectionIsUnaffected(t *testing.T) {
	node := newPeerNode(t, func(*http.Request) ([]byte, bool) { return nil, false })
	const content = "bytes an ordinary client reads with a bearer token"
	desc, err := node.cas.Put(context.Background(), strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	status, body, _ := tracedGet(t, http.DefaultClient,
		node.http.URL+"/api/v1/blobs/"+desc.Hash.String()+"/content")
	if status != http.StatusOK || body != content {
		t.Fatalf("a client presenting no peer identity got %d %q", status, body)
	}
	if got := node.lookup.checks.Load(); got != 0 {
		t.Errorf("membership was consulted %d times for a non-peer connection", got)
	}
}

// TestAnUnenrolledPeerIsRefusedFromTheStart is the other side of the
// revocation test: a key that was never registered never gets in. Together
// they pin both edges — enrolment lets a peer in, removal puts it back out —
// so neither can be satisfied by a guard that is stuck open or stuck shut.
func TestAnUnenrolledPeerIsRefusedFromTheStart(t *testing.T) {
	stranger, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	node := newPeerNode(t, func(*http.Request) ([]byte, bool) { return stranger, true })
	const content = "bytes a stranger must not read"
	desc, err := node.cas.Put(context.Background(), strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	blobURL := node.http.URL + "/api/v1/blobs/" + desc.Hash.String() + "/content"

	status, _, _ := tracedGet(t, http.DefaultClient, blobURL)
	if status != http.StatusForbidden {
		t.Fatalf("an unenrolled peer got %d, want 403", status)
	}

	// Enrol it, and the same request works. This is what makes the 403 above
	// evidence about membership rather than about a broken route.
	if _, err := node.store.Register(context.Background(), membership.Registration{
		Name: "stranger", PublicKey: stranger,
	}); err != nil {
		t.Fatal(err)
	}
	status, body, _ := tracedGet(t, http.DefaultClient, blobURL)
	if status != http.StatusOK || body != content {
		t.Fatalf("after enrolment: %d %q, want 200 and the bytes", status, body)
	}
}
