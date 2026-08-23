// Every HTTP response in this package's tests is closed by the t.Cleanup that
// the harness registers, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/mcp"
	"github.com/rarebit-one/heyarr-core/internal/api/resources"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

const stamp = "2026-08-01T00:00:00Z"

type fixedClock struct{ t time.Time }

func (c *fixedClock) Now() time.Time { return c.t }

type harness struct {
	t      *testing.T
	db     *sqlite.DB
	http   *httptest.Server
	store  *auth.Store
	server *mcp.Server
}

// newHarness builds the MCP server over a real database and a real resource
// API, mounted on the real router.
//
// Nothing is mocked. These tests are about the interaction between JSON-RPC
// dispatch, the scope middleware and the shared write intents, and a mock would
// assert that the test's idea of that interaction is self-consistent.
func newHarness(t *testing.T, authEnabled bool) *harness {
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

	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.HTTP.Auth.Enabled = authEnabled
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = ""

	clock := &fixedClock{t: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)}
	store, err := auth.NewStore(auth.StoreOptions{
		Writer: db.Writer(), Reader: db.Reader(), Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := auth.NewVerifier(auth.VerifierOptions{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{
		Writer: db.Writer(), Reader: db.Reader(), Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(jobs.Options{
		Writer: db.Writer(), Reader: db.Reader(), Clock: clock, Events: eventLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{
		DB: db, Events: eventLog, PeerName: "test", PeerSite: "test-site", Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}

	api, err := resources.New(resources.Options{
		DB: db, Jobs: queue, Events: eventLog, Tokens: store, Catalog: cat,
		Logger: slog.New(slog.DiscardHandler), Now: clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := mcp.New(mcp.Options{
		DB: db, Resources: api, Jobs: queue,
		Logger: slog.New(slog.DiscardHandler), Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}

	srv, err := httpapi.New(httpapi.Options{
		Config: cfg, Logger: slog.New(slog.DiscardHandler), DB: db,
		Verifier: verifier, Events: eventLog,
		Build:              buildinfo.Info{Version: "test", Commit: "abc", Date: stamp},
		SchemaVersion:      4,
		KnownSchemaVersion: 4,
		Mount:              []httpapi.MountFunc{api.Mount, server.Mount},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	h := &harness{t: t, db: db, http: ts, store: store, server: server}
	h.seed()
	return h
}

func (h *harness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.db.Writer().Exec(query, args...); err != nil {
		h.t.Fatalf("seeding (%s): %v", query, err)
	}
}

const (
	peerID    = "01990000-0000-7000-8000-0000000000p1"
	workID    = "01990000-0000-7000-8000-0000000000w1"
	profileID = "01990000-0000-7000-8000-0000000000q1"
	blobHash  = "blake3:1111111111111111111111111111111111111111111111111111111111111111"
)

func (h *harness) seed() {
	h.t.Helper()
	h.exec(`INSERT INTO peers (id, name, site, mode, is_self, created_at)
		VALUES (?, 'peer-a', 'site-a', 'full', 1, ?)`, peerID, stamp)
	h.exec(`INSERT INTO quality_profiles
		(id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES (?, 'living-room', 'A television.',
			'[{"attribute":"resolution","op":"gte","value":1080}]',
			'[{"attribute":"video_codec","op":"eq","value":"hevc","weight":20}]',
			'[{"attribute":"resolution","op":"gte","value":2160}]', 1, ?, ?)`,
		profileID, stamp, stamp)
	h.exec(`INSERT INTO works
		(id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES (?, 'movie', 'movie:arrival:2016', 'Arrival', 'arrival', 2016, '{}', ?, ?)`,
		workID, stamp, stamp)
	h.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at)
		VALUES (?, 8589934592, 'video/x-matroska', ?)`, blobHash, stamp)
	h.exec(`INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, verified_at, updated_at)
		VALUES (?, ?, 'present', 8589934592, ?, ?)`, blobHash, peerID, stamp, stamp)
}

func (h *harness) mint(name string, scopes ...auth.Scope) string {
	h.t.Helper()
	created, err := h.store.Create(context.Background(), name, scopes, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return created.Secret
}

// rpc sends one JSON-RPC request and returns the decoded response.
func (h *harness) rpc(token string, body string) rpcResponse {
	h.t.Helper()
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, h.http.URL+"/api/v1/mcp", bytes.NewReader([]byte(body)))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = resp.Body.Close() })

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	out := rpcResponse{Status: resp.StatusCode, Raw: raw}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &out.Body); err != nil {
			h.t.Fatalf("the response is not JSON (%d): %s", resp.StatusCode, raw)
		}
	}
	return out
}

// call is the common case: tools/call with arguments.
func (h *harness) call(token, tool, args string) rpcResponse {
	h.t.Helper()
	if args == "" {
		args = "{}"
	}
	return h.rpc(token, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{
		"name":"`+tool+`","arguments":`+args+`}}`)
}

type rpcResponse struct {
	Status int
	Raw    []byte
	Body   struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		} `json:"error"`
	}
}

// structured decodes the tool result's structuredContent into v.
func (r rpcResponse) structured(t *testing.T, v any) {
	t.Helper()
	if r.Body.Error != nil {
		t.Fatalf("expected a result, got error %d: %s", r.Body.Error.Code, r.Body.Error.Message)
	}
	var envelope struct {
		Structured json.RawMessage `json:"structuredContent"`
	}
	if err := json.Unmarshal(r.Body.Result, &envelope); err != nil {
		t.Fatalf("result is not an MCP envelope: %s", r.Body.Result)
	}
	if err := json.Unmarshal(envelope.Structured, v); err != nil {
		t.Fatalf("structuredContent does not decode: %s", envelope.Structured)
	}
}

// wantOne creates a want and returns its id.
func (h *harness) wantOne(token string) string {
	h.t.Helper()
	resp := h.call(token, "want_content",
		`{"work_id":"`+workID+`","quality_profile":"living-room"}`)
	var item struct {
		ID string `json:"id"`
	}
	resp.structured(h.t, &item)
	if item.ID == "" {
		h.t.Fatalf("no want was created: %s", resp.Raw)
	}
	return item.ID
}
