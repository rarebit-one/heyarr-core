//nolint:bodyclose // responses are closed by do()'s t.Cleanup
package blobs_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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

// fakeSource is a byte-level blobs.PartialSource for the HTTP-level tests: it
// serves content over a set of landed byte ranges, with no piece machinery. The
// piece→byte translation is the controller adapter's job and is tested there.
type fakeSource struct {
	content  []byte
	landed   [][2]int64
	inflight bool
}

func (f fakeSource) ArrivingSize(context.Context, hashing.Hash) (int64, bool, error) {
	return int64(len(f.content)), f.inflight, nil
}

func (f fakeSource) SetPlayhead(context.Context, hashing.Hash, int64) error { return nil }

func (f fakeSource) Available(_ context.Context, _ hashing.Hash, off int64) (int64, bool, bool, error) {
	if !f.inflight {
		return 0, false, false, nil
	}
	for _, r := range f.landed {
		if off >= r[0] && off < r[1] {
			return r[1], true, true, nil
		}
	}
	return 0, false, true, nil
}

func (f fakeSource) ReadPartialAt(_ hashing.Hash, b []byte, off int64) (int, error) {
	end := off + int64(len(b))
	if end > int64(len(f.content)) {
		end = int64(len(f.content))
	}
	n := 0
	for pos := off; pos < end; pos++ {
		landed := false
		for _, r := range f.landed {
			if pos >= r[0] && pos < r[1] {
				landed = true
				break
			}
		}
		if landed {
			b[n] = f.content[pos]
		}
		n++
	}
	return n, nil
}

// newPartialHarness stands up the same real server the whole-blob tests use, but
// wires the client route's Partial capability so an in-flight blob is served
// progressively (ADR-0044). When partial is nil it is the peer-shaped handler —
// no partial serving — which is how the "still 404s" test proves the capability
// is opt-in, not ambient.
func newPartialHarness(t *testing.T, store cas.Store, partial blobs.PartialSource) *harness {
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
		Store:   store,
		Logger:  slog.New(slog.DiscardHandler),
		Partial: partial,
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

// emptyStore is a real CAS with nothing in it: store.Open reports not-found for
// any hash, so the route reaches the partial path rather than serving a whole
// blob.
func emptyStore(t *testing.T) cas.Store {
	t.Helper()
	fs, err := cas.OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func testBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i%251 + 1)
	}
	return b
}

func hashOf(t *testing.T, b []byte) hashing.Hash {
	t.Helper()
	h, _, err := hashing.HashReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// TestPartialBlobServesLandedRange proves the client route serves a range that
// has landed out of a blob still assembling — a 206 with the true bytes, the same
// strong ETag the finished blob will carry — without ever blocking.
func TestPartialBlobServesLandedRange(t *testing.T) {
	t.Parallel()
	content := testBytes(30000)
	blob := hashOf(t, content)
	src := fakeSource{content: content, landed: [][2]int64{{0, 10000}}, inflight: true}
	h := newPartialHarness(t, emptyStore(t), src)

	const from, to = 1000, 1000 + 8192 - 1
	resp := h.do(t, http.MethodGet, contentPath(blob),
		"Range", "bytes="+strconv.Itoa(from)+"-"+strconv.Itoa(to))
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if got := resp.Header.Get("ETag"); got != `"blake3-`+blob.Hex()+`"` {
		t.Fatalf("ETag = %q, want the whole-object digest", got)
	}
	if !bytes.Equal(body(t, resp), content[from:to+1]) {
		t.Fatal("served range does not match the content")
	}
}

// TestPartialBlobHeadReportsWholeLength proves a HEAD over an in-flight blob
// returns at once with the blob's TRUE logical length — not the high-water mark
// of what has landed — so a player sizes its request correctly (ADR-0043).
func TestPartialBlobHeadReportsWholeLength(t *testing.T) {
	t.Parallel()
	content := testBytes(30000)
	blob := hashOf(t, content)
	src := fakeSource{content: content, landed: [][2]int64{{0, 10000}}, inflight: true}
	h := newPartialHarness(t, emptyStore(t), src)

	resp := h.do(t, http.MethodHead, contentPath(blob))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.ContentLength != int64(len(content)) {
		t.Fatalf("Content-Length = %d, want the whole blob length %d", resp.ContentLength, len(content))
	}
	if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q, want bytes", got)
	}
}

// TestAbsentBlobIsNotFoundWithPartial proves that wiring the partial capability
// does not turn an absent blob into a 500: a source that reports nothing in
// flight (a zero digest, or a digest nothing has staged) is the same 404 the
// route gave before the capability existed.
func TestAbsentBlobIsNotFoundWithPartial(t *testing.T) {
	t.Parallel()
	src := fakeSource{content: nil, inflight: false} // nothing in flight for any hash
	h := newPartialHarness(t, emptyStore(t), src)

	zero := "blake3:" + strings.Repeat("0", hashing.HexLen)
	absent := hashOf(t, []byte("nothing has this"))
	for _, path := range []string{
		httpapi.APIPrefix + "/blobs/" + zero + "/content",
		contentPath(absent),
	} {
		resp := h.do(t, http.MethodGet, path)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404", path, resp.StatusCode)
		}
	}
}

// TestInFlightBlobIsNotFoundWithoutPartial proves the capability is opt-in: an
// in-flight blob served by a handler with no Partial wired (the peer-shaped
// handler) is a plain 404 — the guard that the peer content route never grows
// partial serving (ADR-0042).
func TestInFlightBlobIsNotFoundWithoutPartial(t *testing.T) {
	t.Parallel()
	content := testBytes(30000)
	blob := hashOf(t, content)
	h := newPartialHarness(t, emptyStore(t), nil) // no Partial capability

	resp := h.do(t, http.MethodGet, contentPath(blob))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an in-flight blob with no partial capability", resp.StatusCode)
	}
}
