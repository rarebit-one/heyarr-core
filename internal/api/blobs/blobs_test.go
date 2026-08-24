// Response bodies in this file are closed by the t.Cleanup that do() registers,
// which bodyclose cannot see through — hence the file-wide exemption rather
// than a comment on each of several dozen call sites.
//
//nolint:bodyclose // responses are closed by do()'s t.Cleanup
package blobs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
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

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// harness is the real server — the real router, the real middleware chain, the
// real auth floor — in front of whichever store a test supplies. It is driven
// through httptest rather than through httpapi.Server.Start, so there is no
// port to become ready and nothing to poll or sleep on.
type harness struct {
	client *http.Client
	url    string
}

type harnessOption func(*config.Config)

func withAuth(c *config.Config) { c.HTTP.Auth.Enabled = true }

func newHarness(t *testing.T, store cas.Store, opts ...harnessOption) *harness {
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
	for _, o := range opts {
		o(&cfg)
	}

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

	handler, err := blobs.New(blobs.Options{Store: store, Logger: slog.New(slog.DiscardHandler)})
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

// do issues a request against the blob endpoint. headers are name/value pairs.
func (h *harness) do(t *testing.T, method, target string, headers ...string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, h.url+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func body(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func contentPath(h hashing.Hash) string {
	return httpapi.APIPrefix + "/blobs/" + h.String() + "/content"
}

// ---------------------------------------------------------------------------
// a real CAS with one blob in it
// ---------------------------------------------------------------------------

// seededBlob writes size deterministic pseudo-random bytes into a real
// on-disk CAS and returns the store, the bytes and the digest. Pseudo-random
// rather than a repeating pattern on purpose: a range test over a repeating
// pattern passes with an off-by-one offset.
func seededBlob(t *testing.T, size int64) (cas.Store, []byte, hashing.Hash) {
	t.Helper()
	data := make([]byte, size)
	rng := rand.New(rand.NewSource(20260820)) //nolint:gosec // deterministic test fixture, not a credential
	if _, err := rng.Read(data); err != nil {
		t.Fatal(err)
	}
	store, err := cas.OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	desc, err := store.Put(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := hashing.HashReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if !desc.Hash.Equal(want) {
		t.Fatalf("the CAS named the blob %s, we hashed it as %s", desc.Hash, want)
	}
	return store, data, desc.Hash
}

// ---------------------------------------------------------------------------
// the acceptance criteria
// ---------------------------------------------------------------------------

const mib = 1 << 20

// A 1 MiB range off a larger blob is the literal example in spec §28.
func TestRangeRequestReturnsPartialContent(t *testing.T) {
	t.Parallel()
	const size = 4*mib + 12345
	store, data, hash := seededBlob(t, size)
	h := newHarness(t, store)

	resp := h.do(t, http.MethodGet, contentPath(hash), "Range", "bytes=0-1048575")
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if got, want := resp.Header.Get("Content-Range"), fmt.Sprintf("bytes 0-%d/%d", mib-1, size); got != want {
		t.Errorf("Content-Range = %q, want %q", got, want)
	}
	got := body(t, resp)
	if len(got) != mib {
		t.Fatalf("read %d bytes, want exactly %d", len(got), mib)
	}
	if !bytes.Equal(got, data[:mib]) {
		t.Error("the first mebibyte of the response is not the first mebibyte of the blob")
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", resp.Header.Get("Accept-Ranges"))
	}
}

// The test that actually proves the endpoint. A single range check passes with
// an off-by-one at a boundary it never crosses; N disjoint ranges reassembled
// into the original digest does not, because BLAKE3 of the concatenation is
// only right if every boundary is right.
func TestDisjointRangesConcatenateToTheBlobDigest(t *testing.T) {
	t.Parallel()
	const size = 3*mib + 7919 // a prime tail, so the last range is ragged
	store, data, hash := seededBlob(t, size)
	h := newHarness(t, store)

	// Deliberately uneven, non-power-of-two spans that do not divide the size:
	// equal spans hide an error that is a constant offset.
	spans := []int64{1, 2, 511, 512, 513, 65535, 65536, 65537, 1, mib, mib - 1, 4096}

	var assembled bytes.Buffer
	var off int64
	for i := 0; off < size; i++ {
		span := spans[i%len(spans)]
		end := off + span - 1
		if end >= size-1 {
			end = size - 1
		}
		rangeHeader := fmt.Sprintf("bytes=%d-%d", off, end)
		resp := h.do(t, http.MethodGet, contentPath(hash), "Range", rangeHeader)
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("%s: status = %d, want 206", rangeHeader, resp.StatusCode)
		}
		wantCR := fmt.Sprintf("bytes %d-%d/%d", off, end, size)
		if got := resp.Header.Get("Content-Range"); got != wantCR {
			t.Fatalf("%s: Content-Range = %q, want %q", rangeHeader, got, wantCR)
		}
		chunk := body(t, resp)
		if int64(len(chunk)) != end-off+1 {
			t.Fatalf("%s: got %d bytes, want %d", rangeHeader, len(chunk), end-off+1)
		}
		assembled.Write(chunk)
		off = end + 1
	}

	if int64(assembled.Len()) != size {
		t.Fatalf("reassembled %d bytes, want %d", assembled.Len(), size)
	}
	got, _, err := hashing.HashReader(bytes.NewReader(assembled.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(hash) {
		t.Errorf("the concatenated ranges hash to %s, want %s", got, hash)
	}
	if !bytes.Equal(assembled.Bytes(), data) {
		t.Error("the reassembled bytes differ from the blob")
	}
}

// A multi-range request is one response, and its parts have to line up too.
// Replication and a web-seed both ask for several pieces at once.
func TestMultipleRangesInOneRequest(t *testing.T) {
	t.Parallel()
	const size = 256 * 1024
	store, data, hash := seededBlob(t, size)
	h := newHarness(t, store)

	resp := h.do(t, http.MethodGet, contentPath(hash), "Range", "bytes=0-99,1000-1999,262143-262143")
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	if mediaType != "multipart/byteranges" {
		t.Fatalf("Content-Type = %q, want multipart/byteranges", mediaType)
	}
	mr := multipart.NewReader(resp.Body, params["boundary"])
	want := [][]byte{data[0:100], data[1000:2000], data[262143:262144]}
	for i := 0; ; i++ {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			if i != len(want) {
				t.Fatalf("got %d parts, want %d", i, len(want))
			}
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(part)
		if err != nil {
			t.Fatal(err)
		}
		if i >= len(want) {
			t.Fatalf("got more than %d parts", len(want))
		}
		if !bytes.Equal(got, want[i]) {
			t.Errorf("part %d is %d bytes and does not match the blob", i, len(got))
		}
	}
}

// If-Range is the resumable-copy primitive: "give me this range, but only if
// the thing has not changed under me, otherwise start over". A stale validator
// must produce the whole object, not a 206 of the wrong bytes and not a 412.
func TestIfRange(t *testing.T) {
	t.Parallel()
	const size = 64 * 1024
	store, data, hash := seededBlob(t, size)
	h := newHarness(t, store)

	other := hashing.MustParse("blake3:" + strings.Repeat("ab", 32))

	tests := []struct {
		name       string
		ifRange    string
		wantStatus int
		wantLen    int
	}{
		{"matching etag serves the range", `"blake3-` + hash.Hex() + `"`, http.StatusPartialContent, 100},
		{"stale etag serves the whole body", `"blake3-` + other.Hex() + `"`, http.StatusOK, size},
		{"absent if-range serves the range", "", http.StatusPartialContent, 100},
		// A date-based If-Range: we send no Last-Modified, so it can never
		// match, and the whole body is the correct answer.
		{"date if-range serves the whole body", "Mon, 01 Jan 2001 00:00:00 GMT", http.StatusOK, size},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			headers := []string{"Range", "bytes=0-99"}
			if tt.ifRange != "" {
				headers = append(headers, "If-Range", tt.ifRange)
			}
			resp := h.do(t, http.MethodGet, contentPath(hash), headers...)
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			got := body(t, resp)
			if len(got) != tt.wantLen {
				t.Fatalf("got %d bytes, want %d", len(got), tt.wantLen)
			}
			if !bytes.Equal(got, data[:tt.wantLen]) {
				t.Error("the bytes returned are not the ones asked for")
			}
		})
	}
}

func TestUnsatisfiableRange(t *testing.T) {
	t.Parallel()
	const size = 4096
	store, _, hash := seededBlob(t, size)
	h := newHarness(t, store)

	for _, bad := range []string{"bytes=4096-", "bytes=99999-100000", "bytes=5000-6000"} {
		t.Run(bad, func(t *testing.T) {
			t.Parallel()
			resp := h.do(t, http.MethodGet, contentPath(hash), "Range", bad)
			if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
				t.Fatalf("status = %d, want 416", resp.StatusCode)
			}
			if got, want := resp.Header.Get("Content-Range"), fmt.Sprintf("bytes */%d", size); got != want {
				t.Errorf("Content-Range = %q, want %q", got, want)
			}
		})
	}
}

func TestHeadReportsTheSizeWithoutTheBody(t *testing.T) {
	t.Parallel()
	const size = 3*mib + 17
	store, _, hash := seededBlob(t, size)
	h := newHarness(t, store)

	resp := h.do(t, http.MethodHead, contentPath(hash))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q, want bytes", got)
	}
	if got, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64); err != nil || got != size {
		t.Errorf("Content-Length = %q (parse error %v), want %d", resp.Header.Get("Content-Length"), err, size)
	}
	if got := body(t, resp); len(got) != 0 {
		t.Errorf("HEAD returned %d bytes of body", len(got))
	}
	if got := resp.Header.Get("ETag"); got != `"blake3-`+hash.Hex()+`"` {
		t.Errorf("ETag = %q", got)
	}
}

// A HEAD with a Range is how a prober asks "can you actually serve ranges" —
// it must answer 206 with a Content-Range and still no body.
func TestHeadWithRange(t *testing.T) {
	t.Parallel()
	const size = 8192
	store, _, hash := seededBlob(t, size)
	h := newHarness(t, store)

	resp := h.do(t, http.MethodHead, contentPath(hash), "Range", "bytes=0-1023")
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", resp.StatusCode)
	}
	if got, want := resp.Header.Get("Content-Range"), fmt.Sprintf("bytes 0-1023/%d", size); got != want {
		t.Errorf("Content-Range = %q, want %q", got, want)
	}
	if got := body(t, resp); len(got) != 0 {
		t.Errorf("HEAD returned %d bytes of body", len(got))
	}
}

func TestContractHeaders(t *testing.T) {
	t.Parallel()
	store, data, hash := seededBlob(t, 1024)
	h := newHarness(t, store)

	resp := h.do(t, http.MethodGet, contentPath(hash))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	want := map[string]string{
		"ETag":                   `"blake3-` + hash.Hex() + `"`,
		"Cache-Control":          "public, max-age=31536000, immutable",
		"Accept-Ranges":          "bytes",
		"X-Content-Type-Options": "nosniff",
		"Content-Type":           "application/octet-stream",
	}
	for k, v := range want {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	// Last-Modified is deliberately absent: it is a property of this peer's
	// copy, and two peers holding identical bytes must advertise identical
	// validators. See the modtime comment in blobs.go.
	if got := resp.Header.Get("Last-Modified"); got != "" {
		t.Errorf("Last-Modified = %q, want it absent — it would vary between peers holding identical bytes", got)
	}
	if got := resp.Header.Get("Content-Disposition"); got != "" {
		t.Errorf("Content-Disposition = %q, want it absent without ?download=1", got)
	}
	if !bytes.Equal(body(t, resp), data) {
		t.Error("the body is not the blob")
	}
}

func TestDownloadQueryAttaches(t *testing.T) {
	t.Parallel()
	store, _, hash := seededBlob(t, 512)
	h := newHarness(t, store)

	tests := []struct {
		query string
		want  string
	}{
		{"", ""},
		{"?download=0", ""},
		{"?download=", ""},
		{"?download=1", `attachment; filename="` + hash.Hex() + `.bin"`},
		{"?download=true", `attachment; filename="` + hash.Hex() + `.bin"`},
		{"?download=yes", `attachment; filename="` + hash.Hex() + `.bin"`},
	}
	for _, tt := range tests {
		t.Run("query="+tt.query, func(t *testing.T) {
			t.Parallel()
			resp := h.do(t, http.MethodGet, contentPath(hash)+tt.query)
			if got := resp.Header.Get("Content-Disposition"); got != tt.want {
				t.Errorf("Content-Disposition = %q, want %q", got, tt.want)
			}
		})
	}
}

// An unknown hash and a malformed one are different mistakes. 404 tells a
// replication client "ask another peer"; 400 tells it "your request is broken
// and retrying anywhere will not help".
func TestUnknownHashReturnsAProblemDocument(t *testing.T) {
	t.Parallel()
	store, _, _ := seededBlob(t, 16)
	h := newHarness(t, store)

	absent := hashing.MustParse("blake3:" + strings.Repeat("cd", 32))
	resp := h.do(t, http.MethodGet, contentPath(absent))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var doc struct {
		Status int    `json:"status"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	raw := body(t, resp)
	if len(raw) == 0 {
		t.Fatal("an unknown blob produced an empty body — a 404 must be a problem document, not silence")
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("body is not JSON: %v (%q)", err, raw)
	}
	if doc.Status != http.StatusNotFound {
		t.Errorf("problem status = %d, want 404", doc.Status)
	}
	if !strings.Contains(doc.Detail, absent.String()) {
		t.Errorf("problem detail %q does not name the blob asked for", doc.Detail)
	}
}

func TestMalformedHashReturnsBadRequest(t *testing.T) {
	t.Parallel()
	store, _, _ := seededBlob(t, 16)
	h := newHarness(t, store)

	valid := strings.Repeat("ab", 32)
	malformed := map[string]string{
		"no algorithm prefix": valid,
		"wrong algorithm":     "sha256:" + valid,
		"too short":           "blake3:" + valid[:62],
		"too long":            "blake3:" + valid + "cd",
		"uppercase":           "blake3:" + strings.ToUpper(valid),
		"non hex":             "blake3:" + strings.Repeat("zz", 32),
		"prefix only":         "blake3:",
		"nonsense":            "not-a-hash",
	}
	for name, raw := range malformed {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			target := httpapi.APIPrefix + "/blobs/" + raw + "/content"
			resp := h.do(t, http.MethodGet, target)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("GET %s: status = %d, want 400 — a malformed identifier is a different mistake from an absent blob",
					target, resp.StatusCode)
			}
		})
	}
}

// The route inherits the /api/v1 read-scope floor. It is asserted here rather
// than assumed, because "mounted under the authenticated router" is a property
// of the wiring and the wiring is what changes.
func TestContentRequiresAuthentication(t *testing.T) {
	t.Parallel()
	store, _, hash := seededBlob(t, 64)
	h := newHarness(t, store, withAuth)

	resp := h.do(t, http.MethodGet, contentPath(hash))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 with authentication enabled and no token", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// flat memory
// ---------------------------------------------------------------------------

// synthStore serves a blob of an arbitrary size without it existing anywhere.
// A 20 GB fixture cannot be written in CI, and writing one would test the
// filesystem rather than the handler: what is under test is that the response
// path never holds more than a buffer, whatever the size claims to be.
type synthStore struct {
	hash hashing.Hash
	size int64
}

func (s synthStore) Open(_ context.Context, h hashing.Hash) (cas.ReadSeekCloser, cas.Descriptor, error) {
	if !h.Equal(s.hash) {
		return nil, cas.Descriptor{}, cas.ErrNotFound
	}
	return &synthReader{size: s.size}, cas.Descriptor{Hash: s.hash, Size: s.size}, nil
}

func (s synthStore) Stat(_ context.Context, h hashing.Hash) (cas.Descriptor, error) {
	if !h.Equal(s.hash) {
		return cas.Descriptor{}, cas.ErrNotFound
	}
	return cas.Descriptor{Hash: s.hash, Size: s.size}, nil
}

func (s synthStore) Has(_ context.Context, h hashing.Hash) (bool, error) { return h.Equal(s.hash), nil }

func (s synthStore) Verify(context.Context, hashing.Hash) error { return nil }

// A synthetic blob exists nowhere on disk, so it has no local path. Returning
// an error rather than a plausible one is the contract: LocalPath's own
// comment says a store with no local paths is free to refuse.
func (s synthStore) LocalPath(context.Context, hashing.Hash) (string, error) {
	return "", errReadOnly
}

func (s synthStore) Put(context.Context, io.Reader) (cas.Descriptor, error) {
	return cas.Descriptor{}, errReadOnly
}

func (s synthStore) PutExpecting(context.Context, io.Reader, hashing.Hash) (cas.Descriptor, error) {
	return cas.Descriptor{}, errReadOnly
}

func (s synthStore) OpenPartial(context.Context, hashing.Hash) (cas.Partial, error) {
	return nil, errReadOnly
}

func (s synthStore) Link(context.Context, string, cas.Materialisation) (cas.Descriptor, error) {
	return cas.Descriptor{}, errReadOnly
}

func (s synthStore) Delete(context.Context, hashing.Hash) error { return errReadOnly }

func (s synthStore) Walk(_ context.Context, fn func(cas.Descriptor) error) error {
	return fn(cas.Descriptor{Hash: s.hash, Size: s.size})
}

var errReadOnly = errors.New("synthStore: read only")

var _ cas.Store = synthStore{}

// synthReader is a seekable stream of `size` bytes whose contents are whatever
// the caller's buffer already held. That is deliberate: filling the buffer
// would make the soak measure memset throughput instead of the response path,
// and no assertion in the memory tests looks at the bytes. Every test that
// cares what the bytes are runs against a real CAS.
type synthReader struct {
	size int64
	off  int64
}

func (r *synthReader) Read(p []byte) (int, error) {
	if r.off >= r.size {
		return 0, io.EOF
	}
	n := int64(len(p))
	if remaining := r.size - r.off; n > remaining {
		n = remaining
	}
	r.off += n
	return int(n), nil
}

func (r *synthReader) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.off + offset
	case io.SeekEnd:
		abs = r.size + offset
	default:
		return 0, fmt.Errorf("synthReader: bad whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("synthReader: negative position")
	}
	r.off = abs
	return abs, nil
}

func (r *synthReader) Close() error { return nil }

// peakHeapServing streams a whole blob and returns the highest HeapAlloc
// observed while the response was in flight, together with the total bytes the
// runtime allocated over the transfer.
//
// Sampling *during* the transfer is the point. Reading MemStats only after the
// body is drained measures nothing: a handler that buffered the entire blob has
// already dropped the reference by then, so the assertion would pass with the
// bug in place. That is exactly the shape of vacuous test this repo has been
// caught by before.
func peakHeapServing(t *testing.T, h *harness, hash hashing.Hash, size int64) (peakHeap, totalAlloc uint64) {
	t.Helper()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	peakHeap = before.HeapAlloc

	resp := h.do(t, http.MethodGet, contentPath(hash))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// At most ~256 samples whatever the size, so a 20 GiB run does not spend
	// its time in stop-the-world MemStats reads.
	sampleEvery := size / 256
	if sampleEvery < mib {
		sampleEvery = mib
	}

	buf := make([]byte, 256*1024)
	var read, nextSample int64
	var m runtime.MemStats
	for {
		n, err := resp.Body.Read(buf)
		read += int64(n)
		if read >= nextSample {
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peakHeap {
				peakHeap = m.HeapAlloc
			}
			nextSample = read + sampleEvery
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading the body after %d bytes: %v", read, err)
		}
	}
	if read != size {
		t.Fatalf("read %d bytes, want %d", read, size)
	}

	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if after.HeapAlloc > peakHeap {
		peakHeap = after.HeapAlloc
	}
	return peakHeap, after.TotalAlloc - before.TotalAlloc
}

// memoryHeadroom is how much the large transfer is allowed to exceed the small
// one. It is generous — GC scheduling, the race detector's shadow state and the
// test binary's own noise all land inside it — and still an order of magnitude
// below the smallest blob a buffering implementation would hold.
const memoryHeadroom = 64 * mib

// flatMemory serves a small blob and a large one and asserts that neither the
// live heap nor the cumulative allocation grew with the blob.
//
// The assertion is a comparison rather than an absolute number, because an
// absolute number drifts with the Go version, the race detector and whatever
// else the test binary is holding. What must be true is the *shape*: serving
// 20 GiB costs the same as serving 1 MiB.
func flatMemory(t *testing.T, largeSize int64) {
	t.Helper()
	hash := hashing.MustParse("blake3:" + strings.Repeat("7a", 32))

	small := newHarness(t, synthStore{hash: hash, size: mib})
	smallPeak, smallTotal := peakHeapServing(t, small, hash, mib)

	large := newHarness(t, synthStore{hash: hash, size: largeSize})
	largePeak, largeTotal := peakHeapServing(t, large, hash, largeSize)

	t.Logf("small blob %d bytes: peak heap %d B, total alloc %d B", mib, smallPeak, smallTotal)
	t.Logf("large blob %d bytes: peak heap %d B, total alloc %d B", largeSize, largePeak, largeTotal)
	t.Logf("delta: peak heap %+d B, total alloc %+d B, blob size ratio %.0fx",
		int64(largePeak)-int64(smallPeak), int64(largeTotal)-int64(smallTotal), float64(largeSize)/float64(mib))

	if largePeak > smallPeak+memoryHeadroom {
		t.Errorf("peak heap while serving %d bytes was %d B against %d B for %d bytes — "+
			"memory is growing with the blob, so something on the response path is buffering "+
			"instead of streaming (ADR-0013: a 20 GB remux is a normal case)",
			largeSize, largePeak, smallPeak, mib)
	}
	if largeTotal > smallTotal+memoryHeadroom {
		t.Errorf("serving %d bytes allocated %d B in total against %d B for %d bytes — "+
			"cumulative allocation is growing with the blob",
			largeSize, largeTotal, smallTotal, mib)
	}
}

// The cheap variant. 256 MiB is small enough to run everywhere and large enough
// that any implementation holding the blob in memory blows the headroom by 4x.
func TestServingMemoryIsFlatInBlobSize(t *testing.T) {
	flatMemory(t, 256*mib)
}

// The real claim: 20 GB, the size ADR-0013 calls a normal case. Skipped under
// -short so the ordinary test run stays fast.
func TestServingMemoryIsFlatForATwentyGigabyteBlob(t *testing.T) {
	if testing.Short() {
		t.Skip("20 GiB soak: skipped under -short")
	}
	flatMemory(t, 20*(1<<30))
}
