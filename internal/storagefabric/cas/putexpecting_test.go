package cas

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// PutExpecting is the receiving half of invariant 1: the destination knows
// which bytes it asked for, and nothing else it is told counts (§21, M4-09).
//
// The order of the tests below is the order the acceptance condition demands:
// the successful receive is asserted FIRST, so that every refusal afterwards is
// known to be a refusal of something that would otherwise have worked. A
// refusal test written on its own passes just as well against a store that
// refuses everything.

func hashOf(t *testing.T, content []byte) hashing.Hash {
	t.Helper()
	h, _, err := hashing.HashReader(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("hashing the fixture: %v", err)
	}
	return h
}

func quarantineNames(t *testing.T, s *FS) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(s.Root(), quarantineDir))
	if err != nil {
		t.Fatalf("reading quarantine: %v", err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestPutExpectingPublishesBytesThatMatch(t *testing.T) {
	s := newStore(t)
	content := bytes.Repeat([]byte("arrival"), 5000)
	want := hashOf(t, content)

	desc, err := s.PutExpecting(t.Context(), bytes.NewReader(content), want)
	if err != nil {
		t.Fatalf("PutExpecting: %v", err)
	}
	if desc.Hash != want {
		t.Fatalf("descriptor hash = %s, want %s", desc.Hash, want)
	}
	if desc.Size != int64(len(content)) {
		t.Fatalf("descriptor size = %d, want %d", desc.Size, len(content))
	}
	if desc.Deduplicated {
		t.Fatal("the first receive reported a deduplication")
	}

	// Read back off the disk rather than trusting the descriptor. The
	// descriptor is what this function SAID; the file is what it did.
	if got := readAll(t, s, want); !bytes.Equal(got, content) {
		t.Fatal("the published blob is not the bytes that were received")
	}
	if got := hashOf(t, readAll(t, s, want)); got != want {
		t.Fatalf("the bytes on disk hash to %s, not to the name they were published under %s", got, want)
	}
	if names := quarantineNames(t, s); len(names) != 0 {
		t.Fatalf("a successful receive quarantined %v", names)
	}
	if leftovers := tempCount(t, s); leftovers != 0 {
		t.Fatalf("a successful receive left %d staging files behind", leftovers)
	}
}

func tempCount(t *testing.T, s *FS) int {
	t.Helper()
	files, err := s.TempFiles()
	if err != nil {
		t.Fatalf("TempFiles: %v", err)
	}
	return len(files)
}

// The refusal, reproduced only after the acceptance above has shown the same
// call path succeeding on honest bytes.
func TestPutExpectingQuarantinesBytesThatDoNotMatch(t *testing.T) {
	s := newStore(t)
	honest := []byte("the bytes that were asked for")
	expected := hashOf(t, honest)

	// A source serving something else under that name: a restored backup, a
	// mis-sharded store, or a peer that is lying. The destination cannot tell
	// which and does not need to.
	wrong := []byte("something else entirely, of a different length")
	_, err := s.PutExpecting(t.Context(), bytes.NewReader(wrong), expected)

	var corrupt *Corruption
	if !errors.As(err, &corrupt) {
		t.Fatalf("PutExpecting error = %v, want a *Corruption", err)
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Fatal("the corruption does not unwrap to ErrCorrupt, so callers matching on the sentinel would miss it")
	}
	if corrupt.Hash != expected {
		t.Fatalf("the corruption names %s as expected, want %s", corrupt.Hash, expected)
	}
	if corrupt.Actual != hashOf(t, wrong) {
		t.Fatalf("the corruption names %s as actual, want %s", corrupt.Actual, hashOf(t, wrong))
	}
	if corrupt.Size != int64(len(wrong)) {
		t.Fatalf("the corruption counted %d bytes, want %d", corrupt.Size, len(wrong))
	}

	// Nothing claimable.
	if has, err := s.Has(t.Context(), expected); err != nil || has {
		t.Fatalf("Has(%s) = %v, %v — wrong bytes became an addressable blob", expected, has, err)
	}
	if _, _, err := s.Open(t.Context(), expected); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open after a refused receive = %v, want ErrNotFound", err)
	}

	// And the evidence is kept (ADR-0018).
	names := quarantineNames(t, s)
	if len(names) != 1 {
		t.Fatalf("quarantine holds %v, want exactly one entry", names)
	}
	if !strings.HasPrefix(names[0], expected.Hex()+".") {
		t.Fatalf("the quarantined file is %q, which is not named for the digest that was expected", names[0])
	}
	if corrupt.Path == "" {
		t.Fatal("the corruption does not say where the bytes were preserved, which is the actionable half")
	}
	preserved, err := os.ReadFile(filepath.Clean(corrupt.Path)) // #nosec G304 -- the path came from this store
	if err != nil {
		t.Fatalf("reading the quarantined bytes: %v", err)
	}
	if !bytes.Equal(preserved, wrong) {
		t.Fatal("the quarantined file is not the bytes that were refused")
	}
}

// A stream that dies mid-body. This path has no resumption and must not acquire
// any — it stages under a name nothing can find again on purpose (ADR-0035
// puts resumable staging in [FS.OpenPartial], where the unit is a verified
// chunk) — so the only property here is that nothing survives to be mistaken
// for a replica.
func TestPutExpectingLeavesNothingAddressableWhenInterrupted(t *testing.T) {
	s := newStore(t)
	content := bytes.Repeat([]byte("interrupted"), 10000)
	expected := hashOf(t, content)

	broken := io.MultiReader(
		bytes.NewReader(content[:len(content)/2]),
		errReader{errors.New("the connection went away")},
	)
	if _, err := s.PutExpecting(t.Context(), broken, expected); err == nil {
		t.Fatal("an interrupted receive reported success")
	}
	if has, err := s.Has(t.Context(), expected); err != nil || has {
		t.Fatalf("Has after an interrupted receive = %v, %v — half a blob became addressable", has, err)
	}
	// A half-arrived transfer is not corruption either: nothing was ever
	// offered as complete, so there is nothing to preserve as evidence.
	if names := quarantineNames(t, s); len(names) != 0 {
		t.Fatalf("an interrupted receive quarantined %v", names)
	}
	// Nor does it leave the partial bytes lying under tmp/. A receive that
	// returns cleans up after itself; ReapTemp exists for the process that was
	// killed and never got the chance.
	if leftovers := tempCount(t, s); leftovers != 0 {
		t.Fatalf("an interrupted receive left %d staging files behind", leftovers)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// Invariant 9 at the store level: receiving the same bytes twice is one blob.
func TestPutExpectingIsIdempotent(t *testing.T) {
	s := newStore(t)
	content := []byte("received twice")
	expected := hashOf(t, content)

	first, err := s.PutExpecting(t.Context(), bytes.NewReader(content), expected)
	if err != nil {
		t.Fatalf("first receive: %v", err)
	}
	second, err := s.PutExpecting(t.Context(), bytes.NewReader(content), expected)
	if err != nil {
		t.Fatalf("second receive: %v", err)
	}
	if first.Deduplicated {
		t.Fatal("the first receive claimed a deduplication")
	}
	if !second.Deduplicated {
		t.Fatal("the second receive did not report that the bytes were already held")
	}
	if got := readAll(t, s, expected); !bytes.Equal(got, content) {
		t.Fatal("the second receive changed the published bytes")
	}
	if leftovers := tempCount(t, s); leftovers != 0 {
		t.Fatalf("the second receive left %d staging files behind", leftovers)
	}
}

// An empty expectation is a caller that has not decided what it asked for, and
// receiving under it would name the bytes after nothing at all.
func TestPutExpectingRefusesAnEmptyExpectation(t *testing.T) {
	s := newStore(t)
	if _, err := s.PutExpecting(t.Context(), bytes.NewReader([]byte("x")), hashing.Hash{}); err == nil {
		t.Fatal("PutExpecting accepted a zero expected digest")
	}
}
