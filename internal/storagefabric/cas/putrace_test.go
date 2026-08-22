package cas

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// The race here is #177, the Put-path twin of the Link race #176 fixed: several
// callers streaming the *same* bytes into the store at once.
//
// commit() published with os.Rename, which replaces silently. Every worker that
// stats the blob path before the winner's rename lands therefore sees nothing,
// publishes anyway, and is told it created the blob — so several callers each
// believe they created it, and each rename swaps the inode under whoever was
// already holding the previous one (ADR-0014: a hardlink-ingested blob shares
// its inode with the operator's original, and fsck --deep reasons about
// inodes).
//
// The bytes are identical by construction — the path is the BLAKE3 digest of
// them (ADR-0005) — so nothing observable is wrong afterwards. What is wrong is
// the answer Put gives and the inode it leaves behind, and both are asserted
// here directly rather than waiting for a symptom.
//
// It does not need a flake to reproduce: it creates the contention.
func TestConcurrentPutsReportOneCreatorAndOneInode(t *testing.T) {
	const (
		rounds  = 32
		workers = 12
	)
	for round := range rounds {
		s := newStore(t)
		content := []byte(fmt.Sprintf("heyarr identical-put round %d\n%s",
			round, strings.Repeat("z", 256<<10)))

		descs := make([]Descriptor, workers)
		errs := make([]error, workers)
		// Each worker stats the published blob the moment its own Put returns.
		// A replacing publish shows up as two of these disagreeing: the blob at
		// that path was one file when an early worker looked and a different
		// file when a later one did.
		seen := make([]os.FileInfo, workers)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				descs[i], errs[i] = s.Put(t.Context(), bytes.NewReader(content))
				if errs[i] != nil {
					return
				}
				seen[i], errs[i] = os.Stat(s.blobPath(descs[i].Hash))
			}()
		}
		close(start)
		wg.Wait()

		var broke bool
		for i, err := range errs {
			if err != nil {
				t.Errorf("round %d worker %d: Put: %v", round, i, err)
				broke = true
			}
		}
		if broke {
			// The descriptors from a round that errored say nothing useful.
			// Scoped to this round rather than asked of t.Failed(), so an
			// assertion failure in an earlier round does not stop the later
			// rounds reporting their own counts.
			t.FailNow()
		}

		// Identical bytes are one blob however many callers arrive at once, and
		// exactly one of them can honestly claim to have created it.
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

		// The published inode is stable: whatever file the first worker to look
		// found is still the file at that path for everyone after it.
		final, err := os.Stat(s.blobPath(descs[0].Hash))
		if err != nil {
			t.Fatalf("round %d: the published blob is gone: %v", round, err)
		}
		for i, info := range seen {
			if !os.SameFile(info, final) {
				t.Errorf("round %d: worker %d saw a different inode at the blob path than the one left behind", round, i)
			}
		}

		var n int
		if err := s.Walk(t.Context(), func(Descriptor) error { n++; return nil }); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("round %d: store holds %d blobs, want 1", round, n)
		}
		if got := readAll(t, s, descs[0].Hash); !bytes.Equal(got, content) {
			t.Errorf("round %d: stored bytes do not match the source", round)
		}
		if final.Mode().Perm() != blobPerm.Perm() {
			t.Errorf("round %d: blob mode %v, want %v", round, final.Mode().Perm(), blobPerm.Perm())
		}

		// Nothing may be left behind under tmp/: a loser discards its own
		// temporary file, it does not leave one for ReapTemp to find.
		temps, err := s.TempFiles()
		if err != nil {
			t.Fatal(err)
		}
		if len(temps) != 0 {
			t.Errorf("round %d: %d temporary files left behind", round, len(temps))
		}
	}
}

// The single-writer path, asserted directly rather than left to the concurrency
// test: a second Put of bytes already in the store reports a duplicate and does
// not touch the blob that is already there. This is what publishing with a link
// instead of a rename could plausibly break, so it is checked on its own.
func TestPutDeduplicatesWithoutReplacingTheBlob(t *testing.T) {
	s := newStore(t)
	content := bytes.Repeat([]byte("put me twice"), 20_000)

	first := put(t, s, content)
	if first.Deduplicated {
		t.Error("the first Put reported a duplicate")
	}
	before, err := os.Stat(s.blobPath(first.Hash))
	if err != nil {
		t.Fatal(err)
	}

	second := put(t, s, content)
	if !second.Deduplicated {
		t.Error("the second Put did not report a duplicate")
	}
	after, err := os.Stat(s.blobPath(second.Hash))
	if err != nil {
		t.Fatal(err)
	}
	// Same inode is how "was not republished" is checked, rather than mtime
	// alone: a replacing publish of identical bytes is invisible to a content
	// comparison and visible here.
	if !os.SameFile(before, after) {
		t.Error("the second Put replaced a blob that was already there")
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("blob mtime changed from %v to %v", before.ModTime(), after.ModTime())
	}
	if after.Mode().Perm() != blobPerm.Perm() {
		t.Errorf("blob mode %v, want %v", after.Mode().Perm(), blobPerm.Perm())
	}
}
