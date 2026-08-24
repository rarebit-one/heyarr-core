package cas

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

func newStore(t *testing.T) *FS {
	t.Helper()
	s, err := OpenFS(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatalf("OpenFS: %v", err)
	}
	return s
}

func put(t *testing.T, s *FS, content []byte) Descriptor {
	t.Helper()
	d, err := s.Put(t.Context(), bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return d
}

func readAll(t *testing.T, s *FS, h hashing.Hash) []byte {
	t.Helper()
	rc, _, err := s.Open(t.Context(), h)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading blob: %v", err)
	}
	return got
}

// Property: whatever goes in comes back byte-identical, and its name is its
// content. Randomised sizes because the interesting bugs live at buffer
// boundaries.
func TestPutOpenRoundTripsAnySize(t *testing.T) {
	s := newStore(t)
	rng := rand.New(rand.NewPCG(1, 2))

	sizes := []int{0, 1, 7, 4095, 4096, 4097, 1 << 20, (1 << 20) + 1, 3 << 20}
	for range 12 {
		sizes = append(sizes, rng.IntN(5<<20))
	}

	for _, size := range sizes {
		content := make([]byte, size)
		for i := range content {
			content[i] = byte(rng.UintN(256))
		}

		desc := put(t, s, content)
		if desc.Size != int64(size) {
			t.Errorf("size %d: descriptor reports %d", size, desc.Size)
		}

		got := readAll(t, s, desc.Hash)
		if !bytes.Equal(got, content) {
			t.Fatalf("size %d: content did not round trip", size)
		}
		// The blob's name must be the hash of its contents, checked
		// independently rather than trusting what Put returned.
		want, _, err := hashing.HashReader(bytes.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		if !desc.Hash.Equal(want) {
			t.Errorf("size %d: stored as %s, content hashes to %s", size, desc.Hash, want)
		}
	}
}

// Deduplication is a consequence of the layout, not a feature: the path is
// derived from the content, so identical bytes cannot occupy two files.
func TestPutDeduplicates(t *testing.T) {
	s := newStore(t)
	content := []byte("the same bytes twice")

	first := put(t, s, content)
	if first.Deduplicated {
		t.Error("the first Put reported a duplicate")
	}
	second := put(t, s, content)
	if !second.Deduplicated {
		t.Error("the second Put did not report a duplicate")
	}
	if !first.Hash.Equal(second.Hash) {
		t.Fatal("identical bytes produced different hashes")
	}

	var files int
	if err := s.Walk(t.Context(), func(Descriptor) error { files++; return nil }); err != nil {
		t.Fatal(err)
	}
	if files != 1 {
		t.Errorf("the store holds %d blobs after putting identical bytes twice, want 1", files)
	}
}

// A killed Put must leave NOTHING addressable — a half-written file under a
// content address would be corruption that looks like data.
func TestInterruptedPutLeavesNothingAddressable(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(t.Context())

	content := bytes.Repeat([]byte("x"), 4<<20)
	// Cancel partway through the stream.
	r := &cancellingReader{r: bytes.NewReader(content), cancel: cancel, after: 1 << 20}

	if _, err := s.Put(ctx, r); err == nil {
		t.Fatal("Put succeeded despite cancellation")
	}

	var found int
	if err := s.Walk(t.Context(), func(Descriptor) error { found++; return nil }); err != nil {
		t.Fatal(err)
	}
	if found != 0 {
		t.Errorf("an interrupted Put left %d addressable blobs, want 0", found)
	}

	// The bytes it did write are reapable rather than lost.
	entries, err := os.ReadDir(filepath.Join(s.Root(), tmpDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Log("no temporary file survived; acceptable, the cleanup ran")
	}
	removed, err := s.ReapTemp(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("reaped %d temporary files", removed)
}

// Stored blobs must not be writable by accident: they are immutable, and a tool
// that wants to overwrite one should have to work at it.
func TestStoredBlobsAreReadOnly(t *testing.T) {
	s := newStore(t)
	desc := put(t, s, []byte("immutable"))

	info, err := os.Stat(s.blobPath(desc.Hash))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o222 != 0 {
		t.Errorf("blob mode is %v, want no write bits", perm)
	}
}

func TestOpenIsSeekable(t *testing.T) {
	s := newStore(t)
	content := []byte("0123456789abcdefghij")
	desc := put(t, s, content)

	rc, d, err := s.Open(t.Context(), desc.Hash)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if d.Size != int64(len(content)) {
		t.Errorf("descriptor size = %d, want %d", d.Size, len(content))
	}

	// §28 makes range serving a contract; seeking is what implements it.
	if _, err := rc.Seek(10, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(rest) != "abcdefghij" {
		t.Errorf("after seeking to 10 read %q", rest)
	}
}

func TestMissingBlobsAreDistinguishable(t *testing.T) {
	s := newStore(t)
	absent := hashing.MustParse("blake3:" + strings.Repeat("ab", 32))

	if _, _, err := s.Open(t.Context(), absent); !errors.Is(err, ErrNotFound) {
		t.Errorf("Open returned %v, want ErrNotFound", err)
	}
	if _, err := s.Stat(t.Context(), absent); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat returned %v, want ErrNotFound", err)
	}
	has, err := s.Has(t.Context(), absent)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("Has reported a blob that was never stored")
	}
	if err := s.Delete(t.Context(), absent); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete returned %v, want ErrNotFound", err)
	}
}

// Corruption must be detected and the blob quarantined rather than deleted: it
// may be the only copy, and on a hardlink-ingested library it may be the
// original that changed (ADR-0018).
func TestVerifyQuarantinesCorruptBlobs(t *testing.T) {
	s := newStore(t)
	desc := put(t, s, bytes.Repeat([]byte("healthy"), 1000))

	if err := s.Verify(t.Context(), desc.Hash); err != nil {
		t.Fatalf("Verify rejected an intact blob: %v", err)
	}

	// Corrupt one byte in the middle, as bitrot would.
	path := s.blobPath(desc.Hash)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xFF}, 3000); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	err = s.Verify(t.Context(), desc.Hash)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Verify returned %v, want ErrCorrupt", err)
	}

	// Gone from the addressable tree...
	if has, _ := s.Has(t.Context(), desc.Hash); has {
		t.Error("a corrupt blob is still addressable")
	}
	// ...but preserved for inspection, not destroyed.
	entries, err := os.ReadDir(filepath.Join(s.Root(), quarantineDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("quarantine holds %d files, want 1 — a corrupt blob must be preserved", len(entries))
	}
	if !strings.HasPrefix(entries[0].Name(), desc.Hash.Hex()) {
		t.Errorf("quarantined file %q does not name the blob", entries[0].Name())
	}
}

func TestDeleteRemovesTheBlob(t *testing.T) {
	s := newStore(t)
	desc := put(t, s, []byte("temporary"))

	if err := s.Delete(t.Context(), desc.Hash); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if has, _ := s.Has(t.Context(), desc.Hash); has {
		t.Error("the blob survived Delete")
	}
}

func TestWalkVisitsEveryBlobAndIgnoresJunk(t *testing.T) {
	s := newStore(t)
	want := map[string]int64{}
	for i := range 20 {
		content := bytes.Repeat([]byte{byte(i)}, 100+i)
		d := put(t, s, content)
		want[d.Hash.String()] = d.Size
	}

	// Something an operator left behind, or a rename that never completed.
	junk := filepath.Join(s.Root(), blobsDir, hashing.Algorithm, "aa", "bb", "not-a-hash")
	if err := os.MkdirAll(filepath.Dir(junk), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(junk, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := map[string]int64{}
	if err := s.Walk(t.Context(), func(d Descriptor) error {
		got[d.Hash.String()] = d.Size
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if len(got) != len(want) {
		t.Errorf("Walk visited %d blobs, want %d — junk must be skipped, not reported", len(got), len(want))
	}
	for h, size := range want {
		if got[h] != size {
			t.Errorf("blob %s: Walk reported size %d, want %d", h, got[h], size)
		}
	}
}

// Link is the ingest path that makes adopting a real library affordable
// (ADR-0014). Whichever rung it lands on, the bytes must be correct.
func TestLinkMaterialisesAndDegrades(t *testing.T) {
	for _, mode := range []Materialisation{Reflink, Hardlink, Copy} {
		t.Run(string(mode), func(t *testing.T) {
			s := newStore(t)
			src := filepath.Join(t.TempDir(), "source.bin")
			content := bytes.Repeat([]byte("link me"), 50_000)
			if err := os.WriteFile(src, content, 0o600); err != nil {
				t.Fatal(err)
			}

			desc, err := s.Link(t.Context(), src, mode)
			if err != nil {
				t.Fatalf("Link(%s): %v", mode, err)
			}
			if desc.Size != int64(len(content)) {
				t.Errorf("size = %d, want %d", desc.Size, len(content))
			}
			if got := readAll(t, s, desc.Hash); !bytes.Equal(got, content) {
				t.Error("linked content does not match the source")
			}
			// Whatever rung it used must be one of the ones this mode allows.
			allowed := ladder(mode)
			if !slicesContains(allowed, desc.Materialised) {
				t.Errorf("Link(%s) reported %s, want one of %v", mode, desc.Materialised, allowed)
			}
			t.Logf("%s materialised as %s", mode, desc.Materialised)
		})
	}
}

func TestLinkDeduplicates(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	content := bytes.Repeat([]byte("same"), 10_000)
	for _, name := range []string{"a.bin", "b.bin"} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	first, err := s.Link(t.Context(), filepath.Join(dir, "a.bin"), Reflink)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Link(t.Context(), filepath.Join(dir, "b.bin"), Reflink)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Hash.Equal(second.Hash) {
		t.Fatal("identical files produced different hashes")
	}
	if !second.Deduplicated {
		t.Error("linking identical content twice did not report a duplicate")
	}
	// The rung is what HAPPENED, and on a dedupe nothing happened. Reporting
	// the requested mode here made every deduplicating ingest on a filesystem
	// without cloning claim `reflink` — the one value that filesystem can
	// never produce (#223).
	if second.Materialised != None {
		t.Errorf("a deduplicated Link reported materialised=%s, want %s: "+
			"nothing was materialised, so no rung was reached", second.Materialised, None)
	}

	var n int
	if err := s.Walk(t.Context(), func(Descriptor) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("two identical files produced %d blobs, want 1", n)
	}
}

// A cross-filesystem source cannot be hardlink-ingested, and that is ordinary. It must
// degrade to a copy rather than fail an ingest (ADR-0014).
func TestLinkDegradesRatherThanFailing(t *testing.T) {
	s := newStore(t)
	src := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(src, []byte("degrade me"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Copy is the bottom rung and always works.
	desc, err := s.Link(t.Context(), src, Copy)
	if err != nil {
		t.Fatalf("Link(copy): %v", err)
	}
	if desc.Materialised != Copy {
		t.Errorf("materialised as %s, want copy", desc.Materialised)
	}
}

func TestLinkReportsMissingSources(t *testing.T) {
	s := newStore(t)
	_, err := s.Link(t.Context(), filepath.Join(t.TempDir(), "absent"), Reflink)
	if err == nil {
		t.Fatal("Link succeeded for a missing source")
	}
	if !strings.Contains(err.Error(), "absent") {
		t.Errorf("error = %q, want it to name the path", err)
	}
}

// Concurrent Puts of the same content must converge on one blob, not corrupt
// each other. The scanner will do exactly this when a library holds duplicates.
func TestConcurrentPutsOfIdenticalContent(t *testing.T) {
	s := newStore(t)
	content := bytes.Repeat([]byte("concurrent"), 100_000)

	const writers = 8
	hashes := make([]hashing.Hash, writers)
	errs := make([]error, writers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			d, err := s.Put(context.Background(), bytes.NewReader(content))
			hashes[i], errs[i] = d.Hash, err
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("writer %d failed: %v", i, err)
		}
	}
	for i, h := range hashes {
		if !h.Equal(hashes[0]) {
			t.Errorf("writer %d produced %s, writer 0 produced %s", i, h, hashes[0])
		}
	}
	var n int
	if err := s.Walk(t.Context(), func(Descriptor) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d concurrent identical puts produced %d blobs, want 1", writers, n)
	}
	if got := readAll(t, s, hashes[0]); !bytes.Equal(got, content) {
		t.Error("the surviving blob's content is wrong")
	}
}

func TestOpenFSWritesAndValidatesTheMarker(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cas")
	s, err := OpenFS(root)
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(s.Root(), MarkerName))
	if err != nil {
		t.Fatalf("no marker was written: %v", err)
	}
	var marker Marker
	if err := json.Unmarshal(data, &marker); err != nil {
		t.Fatalf("marker is not valid JSON: %v", err)
	}
	if marker.Version != LayoutVersion || marker.Algo != hashing.Algorithm {
		t.Errorf("marker = %+v, want version %d and algo %q", marker, LayoutVersion, hashing.Algorithm)
	}

	// Reopening an existing root must be fine.
	if _, err := OpenFS(root); err != nil {
		t.Errorf("reopening an existing root failed: %v", err)
	}
}

// A root written by a future layout must be refused, not misread. Same
// reasoning as the schema downgrade guard: an old binary interpreting a new
// layout does not fail loudly, it silently does the wrong thing.
func TestOpenFSRefusesAFutureLayout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cas")
	if _, err := OpenFS(root); err != nil {
		t.Fatal(err)
	}
	future, err := json.Marshal(Marker{Version: LayoutVersion + 1, Algo: hashing.Algorithm})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, MarkerName), future, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = OpenFS(root)
	if err == nil {
		t.Fatal("OpenFS accepted a root from a newer layout")
	}
	if !strings.Contains(err.Error(), "upgrade rather than downgrade") {
		t.Errorf("error = %q, want it to say what to do", err)
	}
}

func TestOpenFSRefusesAForeignAlgorithm(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cas")
	if _, err := OpenFS(root); err != nil {
		t.Fatal(err)
	}
	foreign, err := json.Marshal(Marker{Version: LayoutVersion, Algo: "sha256"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, MarkerName), foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFS(root); err == nil {
		t.Error("OpenFS accepted a root addressed by a different hash algorithm")
	}
}

// The fanout is part of the on-disk contract, documented in docs/cas-layout.md.
// A change here is a layout version bump, so it should be visible in review.
func TestBlobPathLayout(t *testing.T) {
	s := newStore(t)
	h := hashing.MustParse("blake3:" + strings.Repeat("ab", 32))
	got := s.blobPath(h)
	want := filepath.Join(s.Root(), "blobs", "blake3", "ab", "ab", h.Hex())
	if got != want {
		t.Errorf("blobPath = %q, want %q", got, want)
	}
}

func TestReapTempOnlyRemovesOldFiles(t *testing.T) {
	s := newStore(t)
	dir := filepath.Join(s.Root(), tmpDir)
	fresh := filepath.Join(dir, "fresh-1.part")
	if err := os.WriteFile(fresh, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	removed, err := s.ReapTemp(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("reaped %d recent files, want 0", removed)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a recent temporary file was removed")
	}

	removed, err = s.ReapTemp(0)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("reaped %d files with a zero cutoff, want 1", removed)
	}
}

// Hashing a 60 GB file must stop when the job's lease is lost (ADR-0008),
// rather than running to completion first.
func TestPutHonoursCancellation(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.Put(ctx, bytes.NewReader(bytes.Repeat([]byte("x"), 1<<20)))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Put returned %v, want context.Canceled", err)
	}
}

func TestOpenFSRejectsAnEmptyRoot(t *testing.T) {
	if _, err := OpenFS(""); err == nil {
		t.Error("OpenFS accepted an empty root")
	}
}

type cancellingReader struct {
	r      io.Reader
	cancel context.CancelFunc
	after  int
	read   int
}

func (c *cancellingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	if c.read >= c.after && c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	return n, err
}

func slicesContains[T comparable](haystack []T, needle T) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
