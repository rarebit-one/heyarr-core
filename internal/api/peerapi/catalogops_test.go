package peerapi_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/catalogop"
	"github.com/rarebit-one/heyarr-core/internal/catalogtomb"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

var catalogOpsNow = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

// realCatalogTomb builds a catalogtomb.Store on a temp DB — the same store the
// controller wires as the CatalogOps source.
func realCatalogTomb(t *testing.T) *catalogtomb.Store {
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
	store, err := catalogtomb.New(catalogtomb.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// signDelete mints a signed delete op for (ct, wk) citing no prior op.
func signDelete(t *testing.T, signer ed25519.PrivateKey, ct, wk string) string {
	t.Helper()
	tok, err := catalogop.Sign(signer, catalogop.OpDelete, ct, wk, nil, catalogOpsNow)
	if err != nil {
		t.Fatalf("signing a delete op: %v", err)
	}
	return tok
}

func catalogOpsURL(l *listener) string {
	return "https://" + l.addr + peerapi.Prefix + "/catalog/ops"
}

// A member fetches this node's catalog ops over the pinned link.
func TestCatalogOpsRouteServesTheLogToAMember(t *testing.T) {
	ctx := context.Background()
	_, signer, _ := ed25519.GenerateKey(nil)
	store := realCatalogTomb(t)
	tok := signDelete(t, signer, "movie", "the-thing-1982")
	if err := store.RecordOps(ctx, []string{tok}); err != nil {
		t.Fatalf("seeding an op: %v", err)
	}

	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())
	l := serveCatalogOps(t, a, root, store)

	status, body, _, err := peerGet(t, dialler(t, b, root), catalogOpsURL(l))
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET catalog ops: status=%d err=%v\n%s", status, err, body)
	}
	var got struct {
		Ops []string `json:"ops"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("ops body is not decodable: %v\n%s", err, body)
	}
	if len(got.Ops) != 1 || got.Ops[0] != tok {
		t.Fatalf("served the wrong ops: %v", got.Ops)
	}
}

// A node not converging a catalog answers 503, not a broken 200.
func TestCatalogOpsRouteAnswers503WhenNotConverging(t *testing.T) {
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())
	l := serve(t, a, root) // no CatalogOps source wired

	status, _, _, err := peerGet(t, dialler(t, b, root), catalogOpsURL(l))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("a node not converging a catalog should answer 503, got %d", status)
	}
}

// The convergence property, end to end over the wire: B pushes a delete op it
// signed; A records it and A's store then tombstones that work — the removal
// crossed the site boundary. The response carries A's merged log back.
func TestPushingACatalogOpConvergesTheServersStore(t *testing.T) {
	ctx := context.Background()
	_, signerB, _ := ed25519.GenerateKey(nil)
	aStore := realCatalogTomb(t)
	tok := signDelete(t, signerB, "movie", "alien-1979")

	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())
	l := serveCatalogOps(t, a, root, aStore)

	if tombstoned, err := aStore.Tombstoned(ctx, "movie", "alien-1979"); err != nil || tombstoned {
		t.Fatalf("A should not tombstone the work before the push: tombstoned=%v err=%v", tombstoned, err)
	}

	status, body, err := peerPost(t, dialler(t, b, root), catalogOpsURL(l),
		map[string]any{"ops": []string{tok}})
	if err != nil || status != http.StatusOK {
		t.Fatalf("POST catalog ops: status=%d err=%v\n%s", status, err, body)
	}

	// A now tombstones the work the op deleted — convergence.
	if tombstoned, err := aStore.Tombstoned(ctx, "movie", "alien-1979"); err != nil || !tombstoned {
		t.Fatalf("A should tombstone the work after the push: tombstoned=%v err=%v", tombstoned, err)
	}
	// And the response returns the merged log (which now holds the pushed op).
	var got struct {
		Ops []string `json:"ops"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("push response is not decodable: %v\n%s", err, body)
	}
	if len(got.Ops) != 1 || got.Ops[0] != tok {
		t.Fatalf("the merged log should hold the pushed op, got %v", got.Ops)
	}
}

// A pushed op whose signature does not verify is the pusher's error: 400, and
// nothing is recorded.
func TestPushingAMalformedCatalogOpIsRejected(t *testing.T) {
	aStore := realCatalogTomb(t)
	a := newPeerNode(t, "peer-a-id", "peer-a")
	b := newPeerNode(t, "peer-b-id", "peer-b")
	root := newTrustRoot(a.member(), b.member())
	l := serveCatalogOps(t, a, root, aStore)

	status, _, err := peerPost(t, dialler(t, b, root), catalogOpsURL(l),
		map[string]any{"ops": []string{"not-a-real-token"}})
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("a malformed op should be refused with 400, got %d", status)
	}
}

// serveCatalogOps constructs a peer surface with a CatalogOps source, mirroring
// serveLeases.
func serveCatalogOps(t *testing.T, self *peerNode, members mtls.Membership, src peerapi.CatalogOpSource) *listener {
	t.Helper()
	logs := &syncBuffer{}
	srv, err := peerapi.New(peerapi.Options{
		Addr:       "127.0.0.1:0",
		Material:   self.material,
		Members:    members,
		SelfPeerID: self.peerID,
		CatalogOps: src,
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
		_ = srv.Shutdown(ctx)
	})
	return &listener{srv: srv, self: self, addr: srv.Addr(), logs: logs}
}

// peerPost sends a JSON body over a pinned peer client.
func peerPost(t *testing.T, c *http.Client, url string, body any) (status int, respBody string, err error) {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatal(readErr)
	}
	return resp.StatusCode, string(raw), nil
}
