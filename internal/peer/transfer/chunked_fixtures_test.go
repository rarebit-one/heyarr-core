package transfer_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/peer/transfer"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/chunking"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// Fixtures for M5-06 and M5-07 (§20, §84, ADR-0034, ADR-0035).
//
// # Every byte number here is measured on the SOURCE
//
// [servedCounter] wraps the source's blob handler and counts what it writes
// into the response. That is deliberate and it is the difference between an
// assertion and a claim: the destination's own Outcome says how many bytes it
// believes it fetched, and a transfer that fetched nothing and published the
// wrong file would report a very good number. What left the source is a fact
// about the source.

// testChunking is a small chunker so that a fixture measured in hundreds of
// kilobytes has tens of chunks.
//
// The defaults average 1 MiB, which would need a multi-megabyte fixture per
// test to exercise a boundary at all. The property under test is not the
// chunker's — golden_test.go owns that — it is what the transfer does with a
// chunk sequence, and a sequence is a sequence at any scale.
func testChunking() chunking.Config {
	return chunking.Config{Min: 1 << 10, Avg: 4 << 10, Max: 16 << 10}
}

// chunkAll runs the chunker over content and returns the sequence.
func chunkAll(t *testing.T, content []byte, cfg chunking.Config) []chunking.Chunk {
	t.Helper()
	c, err := chunking.New(bytes.NewReader(content), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var out []chunking.Chunk
	for {
		chunk, err := c.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, chunk)
	}
}

// manifestOf chunks content and builds the manifest a source would hold for
// it, keyed by the blob's own whole-object digest (ADR-0034).
func manifestOf(t *testing.T, content []byte) (hashing.Hash, manifests.Manifest) {
	t.Helper()
	whole, _, err := hashing.HashReader(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	cfg := testChunking()
	m, err := manifests.Build(whole, cfg, chunkAll(t, content, cfg),
		time.Date(2026, 8, 24, 11, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Chunks) < 4 {
		t.Fatalf("the fixture chunked into %d chunks, which is too few to distinguish a prefix "+
			"scan from a whole retry", len(m.Chunks))
	}
	return whole, m
}

// servedCounter wraps a source's blob handler and records what it actually
// sent, per request and in total.
type servedCounter struct {
	inner peerapi.BlobServer

	mu       sync.Mutex
	bytes    int64
	requests int
	ranged   int
	// onServed, when set, runs after each response with the running total. It
	// is how a test interrupts a transfer at a chosen number of bytes instead
	// of at a chosen time.
	onServed func(total int64)
	// delay, when set, is slept before each response, so a test can be sure a
	// transfer is still in flight when it kills the process doing it.
	delay time.Duration
}

func (s *servedCounter) Content(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests++
	if r.Header.Get("Range") != "" {
		s.ranged++
	}
	delay := s.delay
	s.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}

	cw := &countingWriter{ResponseWriter: w}
	s.inner.Content(cw, r)

	s.mu.Lock()
	s.bytes += cw.n
	total := s.bytes
	hook := s.onServed
	s.mu.Unlock()
	if hook != nil {
		hook(total)
	}
}

// stats reports what this source has served.
func (s *servedCounter) stats() (bytes int64, requests, ranged int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes, s.requests, s.ranged
}

func (s *servedCounter) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bytes, s.requests, s.ranged = 0, 0, 0
}

func (s *servedCounter) interruptAfter(n int64, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onServed = func(total int64) {
		if total >= n {
			cancel()
		}
	}
}

// countingWriter counts response body bytes. Headers are not counted: the
// assertion is about content moved over the link, and a per-chunk request's
// headers are a rounding error that would make a stated fraction unreadable.
type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	return n, err
}

// chunkedSource is a source peer serving content, its manifest, and a count of
// what it has sent.
type chunkedSource struct {
	self     *node
	addr     string
	store    *cas.FS
	mans     *fakeManifests
	counting *servedCounter
}

func (s *chunkedSource) source() replication.Source {
	return replication.Source{
		PeerID: s.self.peerID, Name: s.self.name,
		Endpoint: "https://" + s.addr, PublicKey: s.self.pub,
	}
}

// startChunkedSource runs a peer surface over content, with the counting
// wrapper in front of the blob handler.
func startChunkedSource(
	t *testing.T, self *node, members mtls.Membership, content []byte,
) *chunkedSource {
	t.Helper()
	store, err := cas.OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if content != nil {
		if _, err := store.Put(t.Context(), bytes.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}
	handler, err := blobs.New(blobs.Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	counting := &servedCounter{inner: handler}
	mans := newFakeManifests()
	srv, err := peerapi.New(peerapi.Options{
		Addr:       "127.0.0.1:0",
		Material:   self.material,
		Members:    members,
		SelfPeerID: self.peerID,
		Blobs:      counting,
		Manifests:  mans,
		Logger:     slog.New(slog.DiscardHandler),
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
			t.Errorf("shutting a source down: %v", err)
		}
	})
	return &chunkedSource{self: self, addr: srv.Addr(), store: store, mans: mans, counting: counting}
}

// addBlob puts more content into a running source and records its manifest.
func (s *chunkedSource) addBlob(t *testing.T, content []byte) (hashing.Hash, manifests.Manifest) {
	t.Helper()
	if _, err := s.store.Put(t.Context(), bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	blob, m := manifestOf(t, content)
	s.mans.store(m)
	return blob, m
}

// fakeIndex is a destination's local chunk index.
//
// It is a map of claims. Nothing in it is checked against a disk, which is the
// point: M5-07's premise is that the index is a claim and the bytes are the
// authority, and a fixture that could not hold a wrong claim could not test
// that.
type fakeIndex struct {
	mu sync.Mutex
	by map[hashing.Hash][]manifests.LocalChunk
}

func newFakeIndex() *fakeIndex {
	return &fakeIndex{by: map[hashing.Hash][]manifests.LocalChunk{}}
}

func (f *fakeIndex) Locate(_ context.Context, digest hashing.Hash) ([]manifests.LocalChunk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]manifests.LocalChunk(nil), f.by[digest]...), nil
}

// record indexes every chunk of a blob this node holds, exactly as M5-03's
// RecordLocal would after chunking it.
func (f *fakeIndex) record(blob hashing.Hash, chunks []chunking.Chunk) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range chunks {
		f.by[c.Digest] = append(f.by[c.Digest], manifests.LocalChunk{
			Digest: c.Digest, BlobHash: blob, Offset: c.Offset, Length: c.Length,
		})
	}
}

// claim writes one entry by hand, for the stale-entry cases.
func (f *fakeIndex) claim(entry manifests.LocalChunk) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.by[entry.Digest] = append(f.by[entry.Digest], entry)
}

// chunkedDestination is this node: a store, an index and a puller over both.
type chunkedDestination struct {
	puller *transfer.Puller
	store  *cas.FS
	index  *fakeIndex
	root   string
}

func newChunkedDestination(t *testing.T, self *node) *chunkedDestination {
	t.Helper()
	root := t.TempDir()
	return openChunkedDestination(t, self, root)
}

// openChunkedDestination builds a destination over an existing CAS root, so a
// test can point one at the bytes a killed process left behind.
func openChunkedDestination(t *testing.T, self *node, root string) *chunkedDestination {
	t.Helper()
	store, err := cas.OpenFS(root)
	if err != nil {
		t.Fatal(err)
	}
	index := newFakeIndex()
	p, err := transfer.New(transfer.Options{Material: self.material, Store: store, Index: index})
	if err != nil {
		t.Fatal(err)
	}
	return &chunkedDestination{puller: p, store: store, index: index, root: root}
}

// partialPath is where this destination's staging file for a blob is.
//
// Derived through cas.PartialName rather than spelled out, so the tests do not
// re-implement the store's layout — §18 applies to tests too, and a test that
// hard-codes a path is a test that keeps passing after the store changes.
func (d *chunkedDestination) partialPath(blob hashing.Hash) string {
	return filepath.Join(d.root, "tmp", cas.PartialName(blob))
}

// partialSize is how many bytes an interrupted attempt left.
func (d *chunkedDestination) partialSize(t *testing.T, blob hashing.Hash) int64 {
	t.Helper()
	info, err := os.Stat(d.partialPath(blob))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return info.Size()
}

// readBlob reads a published blob back out of a store.
func readBlob(t *testing.T, store *cas.FS, blob hashing.Hash) []byte {
	t.Helper()
	rc, _, err := store.Open(t.Context(), blob)
	if err != nil {
		t.Fatalf("opening %s: %v", blob, err)
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// deterministicContent is a fixture whose bytes are reproducible and not
// compressible into a single chunk.
//
// A repetitive fixture would cut into a handful of enormous chunks and make
// every reuse number an artefact of the fixture; a random one would make a
// failure unreproducible. This is a cheap xorshift, seeded, so the same seed
// is the same bytes on every machine and every run.
//
// The seed is mixed rather than used directly, and that is not decoration. The
// first version of this helper did `x := seed | 1`, which makes seeds 6 and 7
// — and every other adjacent even/odd pair — the SAME byte stream: two tests
// that thought they were comparing a blob against unrelated content were
// comparing it against itself, and both passed for the wrong reason until one
// of them failed for the right one. A fixture that cannot produce distinct
// inputs cannot falsify anything.
func deterministicContent(seed uint64, n int) []byte {
	out := make([]byte, n)
	x := (seed+1)*0x9E3779B97F4A7C15 ^ 0x2545F4914F6CDD1D
	if x == 0 {
		x = 0x106689D45497FDB5
	}
	for i := range out {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		out[i] = byte(x >> 24)
	}
	return out
}

// The fixture generator is itself asserted, because two tests above compare a
// blob against "unrelated" content and a generator that returned the same
// bytes for two seeds would make both of them vacuous.
func TestTheContentFixtureIsDistinctPerSeed(t *testing.T) {
	seen := map[string]uint64{}
	for seed := uint64(0); seed < 16; seed++ {
		body := deterministicContent(seed, 4096)
		if prev, dup := seen[string(body)]; dup {
			t.Fatalf("seeds %d and %d produce identical content", prev, seed)
		}
		seen[string(body)] = seed
		if bytes.Equal(body, make([]byte, len(body))) {
			t.Fatalf("seed %d produced only zeroes", seed)
		}
	}
}
