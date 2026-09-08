//nolint:bodyclose // responses are closed by do()'s t.Cleanup
package blobs_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// fakeEnsurer is a byte-agnostic blobs.TransferEnsurer for the HTTP-level tests:
// it reports whether the blob is desired and counts how often the route asked.
// The real gate + idempotent enqueue is the controller adapter's job and is
// tested there and end to end; here we prove the ROUTE consults the gate, starts
// nothing a HEAD asks for, and block-then-serves once the ensured transfer lands.
type fakeEnsurer struct {
	desired bool
	calls   atomic.Int32
	// onEnsure runs after the call is recorded, so a test can flip a store or a
	// source into "the transfer this GET started has begun producing bytes".
	onEnsure func()
}

func (f *fakeEnsurer) EnsureTransfer(context.Context, hashing.Hash) (bool, error) {
	f.calls.Add(1)
	if f.onEnsure != nil {
		f.onEnsure()
	}
	return f.desired, nil
}

// gatedStore is a real CAS whose Open reports not-found until a flag flips, so a
// test can model a transfer completing: the blob is really there, but the route
// cannot open it until the ensured transfer is declared done.
type gatedStore struct {
	cas.Store
	ready *atomic.Bool
}

func (g gatedStore) Open(ctx context.Context, h hashing.Hash) (cas.ReadSeekCloser, cas.Descriptor, error) {
	if !g.ready.Load() {
		return nil, cas.Descriptor{}, cas.ErrNotFound
	}
	return g.Store.Open(ctx, h)
}

// togglingSource is a byte-level PartialSource whose in-flight state a test flips
// on, so the route can be shown to pick up a partial that STARTS arriving only
// after the GET ensured its transfer — the ensure→block-then-serve handoff.
type togglingSource struct {
	content []byte
	landed  [][2]int64
	live    *atomic.Bool
}

func (s togglingSource) ArrivingSize(context.Context, hashing.Hash) (int64, bool, error) {
	return int64(len(s.content)), s.live.Load(), nil
}

func (s togglingSource) SetPlayhead(context.Context, hashing.Hash, int64) error { return nil }

func (s togglingSource) Available(_ context.Context, _ hashing.Hash, off int64) (int64, bool, bool, error) {
	if !s.live.Load() {
		return 0, false, false, nil
	}
	for _, r := range s.landed {
		if off >= r[0] && off < r[1] {
			return r[1], true, true, nil
		}
	}
	return 0, false, true, nil
}

func (s togglingSource) ReadPartialAt(_ hashing.Hash, b []byte, off int64) (int, error) {
	end := off + int64(len(b))
	if end > int64(len(s.content)) {
		end = int64(len(s.content))
	}
	n := 0
	for pos := off; pos < end; pos++ {
		for _, r := range s.landed {
			if pos >= r[0] && pos < r[1] {
				b[n] = s.content[pos]
				break
			}
		}
		n++
	}
	return n, nil
}

// newEnsureHarness stands up the same real server the partial tests use, wiring
// the client route's Ensure capability with a fast poll and a caller-set ensure
// timeout so block-then-serve and its bound are exercised deterministically.
func newEnsureHarness(
	t *testing.T, store cas.Store, partial blobs.PartialSource,
	ensurer blobs.TransferEnsurer, ensureTimeout time.Duration,
) *harness {
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
	cfg.Peer = config.Peer{Name: "test-peer"}
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = ""
	cfg.HTTP.Auth.Enabled = false

	authStore, err := auth.NewStore(auth.StoreOptions{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := auth.NewVerifier(auth.VerifierOptions{Store: authStore})
	if err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}

	handler, err := blobs.New(blobs.Options{
		Store:         store,
		Logger:        slog.New(slog.DiscardHandler),
		Partial:       partial,
		Ensure:        ensurer,
		PollInterval:  5 * time.Millisecond,
		EnsureTimeout: ensureTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}

	srv, err := httpapi.New(httpapi.Options{
		Config:             cfg,
		Logger:             slog.New(slog.DiscardHandler),
		DB:                 db,
		Verifier:           verifier,
		Events:             eventLog,
		Build:              buildinfo.Info{Version: "test"},
		SchemaVersion:      1,
		KnownSchemaVersion: 1,
		CASRoot:            dir,
		Mount:              []httpapi.MountFunc{handler.Mount},
	})
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &harness{client: ts.Client(), url: ts.URL}
}

// TestEnsureOnGetServesADesiredBlobOnceItLands proves the headline of #371: a
// GET for a blob the node desires but does not hold, with no transfer running,
// ends up SERVED — the route asked the gate, the gate said yes, and the route
// block-then-served the moment the ensured transfer produced the bytes.
func TestEnsureOnGetServesADesiredBlobOnceItLands(t *testing.T) {
	t.Parallel()
	base, content, blob := seededBlob(t, 40000)
	ready := &atomic.Bool{}
	store := gatedStore{Store: base, ready: ready}
	ens := &fakeEnsurer{desired: true, onEnsure: func() { ready.Store(true) }}

	h := newEnsureHarness(t, store, nil, ens, 2*time.Second)
	resp := h.do(t, http.MethodGet, contentPath(blob))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a desired blob whose ensured transfer landed", resp.StatusCode)
	}
	if got := body(t, resp); !bytesEqual(got, content) {
		t.Fatal("served bytes do not match the finished blob")
	}
	if n := ens.calls.Load(); n != 1 {
		t.Fatalf("the gate was consulted %d times, want exactly 1", n)
	}
}

// TestEnsureOnGetServesAPartialThatStartsAfterTheGet proves the block-then-serve
// handoff: the transfer this GET started begins arriving as a partial, and the
// route serves a landed range off it over ordinary HTTP (§33, ADR-0044) without
// the caller ever knowing a transfer had to be started first.
func TestEnsureOnGetServesAPartialThatStartsAfterTheGet(t *testing.T) {
	t.Parallel()
	content := testBytes(30000)
	blob := hashOf(t, content)
	live := &atomic.Bool{} // not in flight until the GET ensures it
	src := togglingSource{content: content, landed: [][2]int64{{0, 30000}}, live: live}
	ens := &fakeEnsurer{desired: true, onEnsure: func() { live.Store(true) }}

	h := newEnsureHarness(t, emptyStore(t), src, ens, 2*time.Second)
	const from, to = 1000, 1000 + 8192 - 1
	resp := h.do(t, http.MethodGet, contentPath(blob),
		"Range", "bytes="+strconv.Itoa(from)+"-"+strconv.Itoa(to))
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206 off the partial the ensured transfer produced", resp.StatusCode)
	}
	if got := body(t, resp); !bytesEqual(got, content[from:to+1]) {
		t.Fatal("served range does not match the content")
	}
}

// TestEnsureOnGetGateHoldsForANonDesiredBlob is the load-bearing negative: a GET
// for a blob NOBODY desires still 404s. The gate refused, so nothing was served
// and — the point of the gate — a GET cannot make the node fetch it.
func TestEnsureOnGetGateHoldsForANonDesiredBlob(t *testing.T) {
	t.Parallel()
	absent := hashOf(t, []byte("nobody desires these bytes"))
	ens := &fakeEnsurer{desired: false}

	h := newEnsureHarness(t, emptyStore(t), nil, ens, 2*time.Second)
	resp := h.do(t, http.MethodGet, contentPath(absent))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a non-desired blob — the gate must not fetch on request", resp.StatusCode)
	}
	if n := ens.calls.Load(); n != 1 {
		t.Fatalf("the gate was consulted %d times, want exactly 1", n)
	}
}

// TestEnsureOnGetTimesOutWhenNothingLands proves the bound: a desired blob whose
// source never delivers answers "still fetching" (503, with a Retry-After)
// rather than hanging forever (#371).
func TestEnsureOnGetTimesOutWhenNothingLands(t *testing.T) {
	t.Parallel()
	absent := hashOf(t, []byte("desired, but no source will ever deliver"))
	ens := &fakeEnsurer{desired: true} // no onEnsure: nothing ever becomes servable

	h := newEnsureHarness(t, emptyStore(t), nil, ens, 80*time.Millisecond)
	resp := h.do(t, http.MethodGet, contentPath(absent))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when the ensured transfer never produces bytes", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("a still-fetching 503 must carry Retry-After so a player knows to ask again")
	}
}

// TestEnsureOnGetDoesNotFetchForAHead proves a HEAD — a question about what is
// held — never starts a transfer. It falls through to the plain 404, and the
// gate is not even consulted.
func TestEnsureOnGetDoesNotFetchForAHead(t *testing.T) {
	t.Parallel()
	absent := hashOf(t, []byte("a head must not start a fetch"))
	ens := &fakeEnsurer{desired: true}

	h := newEnsureHarness(t, emptyStore(t), nil, ens, 2*time.Second)
	resp := h.do(t, http.MethodHead, contentPath(absent))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a HEAD of an absent blob", resp.StatusCode)
	}
	if n := ens.calls.Load(); n != 0 {
		t.Fatalf("a HEAD consulted the gate %d times, want 0 — a HEAD must not start a fetch", n)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
