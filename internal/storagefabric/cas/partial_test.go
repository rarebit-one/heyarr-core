package cas_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// Resumable staging (§84, ADR-0035, ADR-0018, M5-06).
//
// A partial is bytes nothing believes. These tests assert the two halves of
// that: it is addressable by nothing while it exists, and it becomes a blob
// only after the store has hashed the assembled file whole on its own disk.

func openStore(t *testing.T) *cas.FS {
	t.Helper()
	s, err := cas.OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func digestOf(t *testing.T, b []byte) hashing.Hash {
	t.Helper()
	h, _, err := hashing.HashReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The baseline: bytes appended in order, verified whole, published, readable.
// Without it every refusal below is unfalsifiable — they would all pass on a
// staging path that never published anything.
func TestAPartialAssemblesAndPublishes(t *testing.T) {
	s := openStore(t)
	content := bytes.Repeat([]byte("assembled from pieces"), 500)
	want := digestOf(t, content)

	p, err := s.OpenPartial(t.Context(), want)
	if err != nil {
		t.Fatal(err)
	}
	for _, piece := range [][]byte{content[:1000], content[1000:6000], content[6000:]} {
		if _, err := p.Append(t.Context(), bytes.NewReader(piece)); err != nil {
			t.Fatal(err)
		}
	}
	if p.Size() != int64(len(content)) {
		t.Fatalf("the partial is %d bytes, the content is %d", p.Size(), len(content))
	}

	desc, err := p.Publish(t.Context(), want)
	if err != nil {
		t.Fatalf("publishing an assembled partial: %v", err)
	}
	if !desc.Hash.Equal(want) {
		t.Errorf("published %s, want %s", desc.Hash, want)
	}
	if desc.Size != int64(len(content)) {
		t.Errorf("published %d bytes, want %d", desc.Size, len(content))
	}
	held, err := s.Has(t.Context(), want)
	if err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Error("the blob is not present after publication")
	}
	if err := s.Verify(t.Context(), want); err != nil {
		t.Errorf("the published blob does not verify: %v", err)
	}
	// The staging file is gone, so nothing is left for the reaper to bill for.
	files, err := s.TempFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("publication left %d staging files behind: %+v", len(files), files)
	}
}

// A partial is addressable by nothing. Four different questions, because these
// are four different code paths and each of them is how a partial could become
// a blob by accident.
func TestAPartialIsAddressableByNothing(t *testing.T) {
	s := openStore(t)
	content := bytes.Repeat([]byte("half a blob"), 400)
	want := digestOf(t, content)

	p, err := s.OpenPartial(t.Context(), want)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	if _, err := p.Append(t.Context(), bytes.NewReader(content[:2000])); err != nil {
		t.Fatal(err)
	}

	if held, err := s.Has(t.Context(), want); err != nil || held {
		t.Errorf("Has = %v (err %v) for a blob that is half received", held, err)
	}
	if _, _, err := s.Open(t.Context(), want); !errors.Is(err, cas.ErrNotFound) {
		t.Errorf("Open of a half-received blob returned %v, want ErrNotFound", err)
	}
	if _, err := s.Stat(t.Context(), want); !errors.Is(err, cas.ErrNotFound) {
		t.Errorf("Stat of a half-received blob returned %v, want ErrNotFound", err)
	}
	var walked int
	if err := s.Walk(t.Context(), func(cas.Descriptor) error { walked++; return nil }); err != nil {
		t.Fatal(err)
	}
	if walked != 0 {
		t.Errorf("Walk visited %d blobs, and the only bytes in this store are a partial transfer",
			walked)
	}
}

// An abandoned partial is reapable on the existing path, and its age is what
// decides. No reference count, no negotiation with the job queue (ADR-0035).
func TestAnAbandonedPartialIsReapedByAge(t *testing.T) {
	s := openStore(t)
	want := digestOf(t, []byte("abandoned"))

	p, err := s.OpenPartial(t.Context(), want)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Append(t.Context(), strings.NewReader("abandon")); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	files, err := s.TempFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name != cas.PartialName(want) {
		t.Fatalf("the staging files are %+v, want one named %s", files, cas.PartialName(want))
	}

	// Young enough to be somebody's transfer in flight: left alone.
	removed, err := s.ReapTemp(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("the reaper removed %d files that are seconds old", removed)
	}
	// Old enough by the window the caller states: reaped.
	removed, err = s.ReapTemp(0)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("the reaper removed %d files, want 1", removed)
	}
}

// A second attempt finds what the first left, by a name derived from the
// digest — which is what makes a killed process resumable without anything
// having recorded where its bytes were.
func TestASecondAttemptFindsWhatTheFirstLeft(t *testing.T) {
	s := openStore(t)
	content := bytes.Repeat([]byte("resumed"), 300)
	want := digestOf(t, content)

	first, err := s.OpenPartial(t.Context(), want)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Append(t.Context(), bytes.NewReader(content[:700])); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := s.OpenPartial(t.Context(), want)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if second.Size() != 700 {
		t.Fatalf("the second attempt found %d bytes, the first left 700", second.Size())
	}
	got := make([]byte, 700)
	if _, err := second.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content[:700]) {
		t.Error("the bytes the second attempt read back are not the ones the first wrote")
	}
}

// Two transfers of one blob in one process do not interleave writes into one
// file. Between processes the job lease is what keeps this to one.
func TestOneBlobHasOneStagingFileAtATime(t *testing.T) {
	s := openStore(t)
	want := digestOf(t, []byte("contended"))

	first, err := s.OpenPartial(t.Context(), want)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenPartial(t.Context(), want); !errors.Is(err, cas.ErrPartialBusy) {
		t.Errorf("a second concurrent partial returned %v, want ErrPartialBusy", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	// Released on close, so a later attempt is not locked out by an earlier
	// one that finished.
	again, err := s.OpenPartial(t.Context(), want)
	if err != nil {
		t.Fatalf("reopening after a close: %v", err)
	}
	_ = again.Close()
}

// A partial only ever shrinks. Extending one would invent bytes nothing
// received, which is the failure the truncated-and-extended tamper is about
// from the other side.
func TestAPartialOnlyShrinks(t *testing.T) {
	s := openStore(t)
	want := digestOf(t, []byte("shrink"))
	p, err := s.OpenPartial(t.Context(), want)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	if _, err := p.Append(t.Context(), strings.NewReader("0123456789")); err != nil {
		t.Fatal(err)
	}

	if err := p.Truncate(11); err == nil {
		t.Error("truncating a 10-byte partial to 11 bytes was accepted")
	}
	if err := p.Truncate(-1); err == nil {
		t.Error("truncating to a negative length was accepted")
	}
	if err := p.Truncate(4); err != nil {
		t.Fatal(err)
	}
	if p.Size() != 4 {
		t.Errorf("the partial is %d bytes after truncating to 4", p.Size())
	}
}

// 🔴 The one that matters: an assembly whose pieces are individually fine and
// whose WHOLE is wrong is not published. Nothing else in the store detects an
// ordering fault (ADR-0034), which is why this check is not redundant with any
// per-chunk verification upstream.
func TestAnAssemblyThatDoesNotHashWholeIsQuarantinedNotPublished(t *testing.T) {
	s := openStore(t)
	first := bytes.Repeat([]byte("A"), 500)
	second := bytes.Repeat([]byte("B"), 700)
	want := digestOf(t, append(append([]byte{}, first...), second...))

	p, err := s.OpenPartial(t.Context(), want)
	if err != nil {
		t.Fatal(err)
	}
	// The same two pieces, in the other order: a set of valid pieces and the
	// wrong file.
	if _, err := p.Append(t.Context(), bytes.NewReader(second)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Append(t.Context(), bytes.NewReader(first)); err != nil {
		t.Fatal(err)
	}

	_, err = p.Publish(t.Context(), want)
	var corrupt *cas.Corruption
	if !errors.As(err, &corrupt) {
		t.Fatalf("publishing a mis-ordered assembly returned %v, want a *cas.Corruption", err)
	}
	if !errors.Is(err, cas.ErrCorrupt) {
		t.Error("the corruption does not unwrap to ErrCorrupt")
	}
	if corrupt.Actual.Equal(want) {
		t.Error("the corruption reports the expected digest as what the bytes hash to")
	}
	if held, err := s.Has(t.Context(), want); err != nil || held {
		t.Errorf("Has = %v (err %v) after a refused publication", held, err)
	}
	// Preserved as evidence rather than deleted (ADR-0018).
	if corrupt.Path == "" {
		t.Fatal("the corruption names no quarantine path")
	}
	body, err := os.ReadFile(corrupt.Path) // #nosec G304 -- the path is the store's own report
	if err != nil {
		t.Fatalf("the quarantined bytes are not readable: %v", err)
	}
	if int64(len(body)) != int64(len(first)+len(second)) {
		t.Errorf("the quarantined file is %d bytes, the assembly was %d",
			len(body), len(first)+len(second))
	}
	if filepath.Base(corrupt.Path) == cas.PartialName(want) {
		t.Error("the quarantined file is still under the staging name")
	}
}

// Publishing under a digest that is not this partial's is refused, so a caller
// holding two transfers cannot publish one under the other's name.
func TestAPartialPublishesOnlyUnderItsOwnDigest(t *testing.T) {
	s := openStore(t)
	mine := digestOf(t, []byte("mine"))
	other := digestOf(t, []byte("other"))

	p, err := s.OpenPartial(t.Context(), mine)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()
	if _, err := p.Append(t.Context(), strings.NewReader("mine")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Publish(t.Context(), other); err == nil {
		t.Error("a partial published itself under another blob's digest")
	}
}

// A store refuses to stage a transfer with no expectation at all, on the same
// reasoning PutExpecting does: a destination verifies against what it asked
// for, and a receive with nothing to check against cannot.
func TestStagingRequiresAnExpectation(t *testing.T) {
	s := openStore(t)
	if _, err := s.OpenPartial(t.Context(), hashing.Hash{}); err == nil {
		t.Error("a partial was opened with no expected digest")
	}
}
