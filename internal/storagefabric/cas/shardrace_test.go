package cas

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// shardMates searches for n distinct payloads whose BLAKE3 digests share their
// first fanoutLevels*fanoutWidth hex characters, i.e. land in the same two-level
// shard directory.
//
// It searches rather than hard-coding digests: a hard-coded digest is a fact
// about today's hash function pinned into a test that is not about hashing, and
// it would stop exercising the shard the moment the layout changed. The search
// is deterministic — the same inputs in the same order every run — so the test
// is not flaky even though the digests are not written down.
func shardMates(tb testing.TB, n int) (prefix string, payloads [][]byte) {
	tb.Helper()
	got := shardSearch()
	if got.err != "" {
		tb.Fatal(got.err)
	}
	if len(got.seeds) < n {
		tb.Fatalf("found %d shard mates, want %d", len(got.seeds), n)
	}
	for _, seed := range got.seeds[:n] {
		payloads = append(payloads, seedPayload(seed))
	}
	return got.prefix, payloads
}

type shardMatch struct {
	prefix string
	seeds  []int
	err    string
}

// The search runs once per process: it is deterministic, so repeating it under
// -count=N would only burn time.
var shardSearch = sync.OnceValue(func() shardMatch {
	const (
		maxTries = 4_000_000
		want     = shardMateCount
	)
	buckets := make(map[string][]int, 1<<16)
	for i := range maxTries {
		hasher := hashing.New()
		if _, err := hasher.Write(seedPayload(i)); err != nil {
			return shardMatch{err: fmt.Sprintf("hashing seed %d: %v", i, err)}
		}
		key := hasher.Sum().Hex()[:fanoutLevels*fanoutWidth]
		buckets[key] = append(buckets[key], i)
		if len(buckets[key]) == want {
			return shardMatch{prefix: key, seeds: buckets[key]}
		}
	}
	return shardMatch{err: fmt.Sprintf("no %d payloads shared a shard prefix within %d candidates", want, maxTries)}
})

// shardMateCount is how many same-shard payloads the search looks for; the
// concurrency tests take as many as they need from that set.
const shardMateCount = 8

func seedPayload(i int) []byte {
	// Padded so every payload is a distinct, non-trivial file rather than a
	// handful of bytes: ingest materialisation reflinks and hardlinks real
	// files, and a zero-length one is not a representative source.
	return []byte(fmt.Sprintf("heyarr shard-race fixture %d\n%s", i, strings.Repeat("x", 64)))
}

// The race this test drives is #151: several ingest workers calling Link
// concurrently for distinct content that happens to land in the same shard
// directory. Link used to stat the blob path before it had created that path's
// parent, so it read a directory another worker was concurrently creating.
//
// It does not need the flake to reproduce — it creates the contention directly.
func TestConcurrentLinksIntoOneShard(t *testing.T) {
	const workers = shardMateCount
	prefix, payloads := shardMates(t, workers)
	t.Logf("driving %d concurrent Links into shard %s/%s", workers, prefix[:fanoutWidth], prefix[fanoutWidth:])

	s := newStore(t)
	dir := t.TempDir()
	srcs := make([]string, workers)
	for i, payload := range payloads {
		srcs[i] = filepath.Join(dir, fmt.Sprintf("source-%d.bin", i))
		if err := os.WriteFile(srcs[i], payload, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	descs := make([]Descriptor, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			descs[i], errs[i] = s.Link(context.Background(), srcs[i], Reflink)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("worker %d: Link: %v", i, err)
		}
	}
	for i, desc := range descs {
		if errs[i] != nil {
			continue
		}
		if desc.Deduplicated {
			t.Errorf("worker %d: distinct content reported as deduplicated", i)
		}
		if got := readAll(t, s, desc.Hash); !bytes.Equal(got, payloads[i]) {
			t.Errorf("worker %d: stored bytes do not match the source", i)
		}
	}

	// Every worker's blob must be present: a lost file is the quiet form this
	// failure takes (sighting 5, a short ingest with no error text).
	var n int
	if err := s.Walk(context.Background(), func(Descriptor) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	if n != workers {
		t.Errorf("store holds %d blobs, want %d", n, workers)
	}

	// The shard directory's permissions must be what the layout says, however
	// many workers raced to create it.
	shard := filepath.Dir(s.blobPath(descs[0].Hash))
	info, err := os.Stat(shard)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != dirPerm.Perm() {
		t.Errorf("shard %s has mode %v, want %v", shard, info.Mode().Perm(), dirPerm.Perm())
	}
}

// The failure #151 actually reproduces is narrower than the shard collision and
// nastier: several workers materialising the *same* bytes at once. Every rung of
// the ladder used to write straight onto the blob path, so a rung that failed
// had to clean up after itself — and on darwin a clonefile that failed because
// another worker had already published deleted that worker's blob, whereupon the
// winner's own chmod failed with ENOENT and its ingest job died.
//
// It is the same shape as the six sightings: rare, input-independent, a
// different blob each time, and sometimes a file that simply goes missing rather
// than an error anyone sees.
//
// This drives it directly rather than waiting for the flake: a fresh store per
// round, distinct source files holding identical bytes, all linked at once.
func TestConcurrentLinksOfIdenticalContent(t *testing.T) {
	const (
		rounds  = 32
		workers = 12
	)
	for round := range rounds {
		s := newStore(t)
		dir := t.TempDir()
		content := []byte(fmt.Sprintf("heyarr identical-ingest round %d\n%s",
			round, strings.Repeat("y", 64<<10)))
		srcs := make([]string, workers)
		for i := range workers {
			srcs[i] = filepath.Join(dir, fmt.Sprintf("copy-%d.bin", i))
			if err := os.WriteFile(srcs[i], content, 0o600); err != nil {
				t.Fatal(err)
			}
		}

		descs := make([]Descriptor, workers)
		errs := make([]error, workers)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				descs[i], errs[i] = s.Link(context.Background(), srcs[i], Reflink)
			}()
		}
		close(start)
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Errorf("round %d worker %d: Link: %v", round, i, err)
			}
		}
		if t.Failed() {
			t.FailNow()
		}

		// Identical bytes are one blob however many workers arrived at once,
		// and exactly one of them can honestly claim to have created it.
		var created int
		for i, desc := range descs {
			if !desc.Deduplicated {
				created++
			}
			if !desc.Hash.Equal(descs[0].Hash) {
				t.Fatalf("round %d worker %d: hashes disagree", round, i)
			}
		}
		if created != 1 {
			t.Errorf("round %d: %d workers reported creating the blob, want 1", round, created)
		}

		var n int
		if err := s.Walk(context.Background(), func(Descriptor) error { n++; return nil }); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("round %d: store holds %d blobs, want 1", round, n)
		}
		if got := readAll(t, s, descs[0].Hash); !bytes.Equal(got, content) {
			t.Errorf("round %d: stored bytes do not match the source", round)
		}

		// The blob must still be there, and still read-only, after the losers
		// have finished failing.
		info, err := os.Stat(s.blobPath(descs[0].Hash))
		if err != nil {
			t.Fatalf("round %d: the published blob is gone: %v", round, err)
		}
		if info.Mode().Perm() != blobPerm.Perm() {
			t.Errorf("round %d: blob mode %v, want %v", round, info.Mode().Perm(), blobPerm.Perm())
		}

		// Nothing may be left staging in tmp/.
		temps, err := s.TempFiles()
		if err != nil {
			t.Fatal(err)
		}
		if len(temps) != 0 {
			t.Errorf("round %d: %d staging files left behind", round, len(temps))
		}
	}
}

// Deduplication is the behaviour most at risk from reordering the existence
// check, so it is asserted directly rather than left to the concurrency tests:
// linking content already in the store must report a duplicate and must not
// touch the blob that is already there.
func TestLinkDeduplicatesWithoutRematerialising(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	content := bytes.Repeat([]byte("dedupe me"), 20_000)
	for _, name := range []string{"first.bin", "second.bin"} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	first, err := s.Link(t.Context(), filepath.Join(dir, "first.bin"), Reflink)
	if err != nil {
		t.Fatal(err)
	}
	if first.Deduplicated {
		t.Error("the first Link reported a duplicate")
	}
	before, err := os.Stat(s.blobPath(first.Hash))
	if err != nil {
		t.Fatal(err)
	}

	second, err := s.Link(t.Context(), filepath.Join(dir, "second.bin"), Reflink)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Deduplicated {
		t.Error("linking content already present did not report a duplicate")
	}
	after, err := os.Stat(s.blobPath(second.Hash))
	if err != nil {
		t.Fatal(err)
	}
	// Same inode is how "not re-materialised" is checked, rather than mtime alone.
	if !os.SameFile(before, after) {
		t.Error("the second Link re-materialised a blob that was already there")
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("blob mtime changed from %v to %v", before.ModTime(), after.ModTime())
	}

	// And the shard directory keeps the mode the layout says it has.
	shard, err := os.Stat(filepath.Dir(s.blobPath(first.Hash)))
	if err != nil {
		t.Fatal(err)
	}
	if shard.Mode().Perm() != dirPerm.Perm() {
		t.Errorf("shard mode %v, want %v", shard.Mode().Perm(), dirPerm.Perm())
	}
}
