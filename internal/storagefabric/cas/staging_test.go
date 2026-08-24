package cas_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

func stagingStore(t *testing.T) *cas.FS {
	t.Helper()
	s, err := cas.OpenFS(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Staged bytes are not addressable until they are published, and they are
// published only under the digest they actually have.
func TestStagedBytesAreNotAddressableUntilPublished(t *testing.T) {
	s := stagingStore(t)
	staged, err := s.Stage()
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("a whole replacement, assembled out of chunks")
	if _, err := staged.Write(content); err != nil {
		t.Fatal(err)
	}
	digest, err := staged.Digest()
	if err != nil {
		t.Fatal(err)
	}

	// Before the publish: the digest is known and resolves to nothing.
	has, err := s.Has(t.Context(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("staged bytes were addressable before they were published")
	}
	seen := 0
	if err := s.Walk(t.Context(), func(cas.Descriptor) error { seen++; return nil }); err != nil {
		t.Fatal(err)
	}
	if seen != 0 {
		t.Errorf("the store walks %d addressable files with only a staging file on disk", seen)
	}

	desc, err := staged.Publish(digest)
	if err != nil {
		t.Fatal(err)
	}
	if desc.Size != int64(len(content)) {
		t.Errorf("published size = %d, want %d", desc.Size, len(content))
	}
	if err := s.Verify(t.Context(), digest); err != nil {
		t.Errorf("the published blob does not verify: %v", err)
	}
	temps, err := s.TempFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Errorf("publishing left %d staging files behind", len(temps))
	}
}

// The gate that cannot be reordered away: bytes are never published under a
// name they do not have (Invariant 1).
func TestPublishRefusesBytesThatDoNotHashToTheName(t *testing.T) {
	s := stagingStore(t)
	staged, err := s.Stage()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := staged.Write([]byte("these are not those bytes")); err != nil {
		t.Fatal(err)
	}
	other := hashing.MustParse("blake3:" + strings.Repeat("ab", 32))

	if _, err := staged.Publish(other); !errors.Is(err, cas.ErrNotVerified) {
		t.Fatalf("publish error = %v, want ErrNotVerified", err)
	}
	has, err := s.Has(t.Context(), other)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("a refused publish made the bytes addressable anyway")
	}
}

// Discard leaves nothing behind, and is a no-op after a publish so a caller
// can defer it unconditionally.
func TestDiscardIsSafeBeforeAndAfterPublishing(t *testing.T) {
	s := stagingStore(t)
	staged, err := s.Stage()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := staged.Write([]byte("abandoned")); err != nil {
		t.Fatal(err)
	}
	if err := staged.Discard(); err != nil {
		t.Fatal(err)
	}
	temps, err := s.TempFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Errorf("a discarded staging file survived: %v", temps)
	}

	published, err := s.Stage()
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("kept")
	if _, err := published.Write(content); err != nil {
		t.Fatal(err)
	}
	digest, err := published.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := published.Publish(digest); err != nil {
		t.Fatal(err)
	}
	if err := published.Discard(); err != nil {
		t.Fatalf("discarding after a publish must be a no-op: %v", err)
	}
	if err := s.Verify(t.Context(), digest); err != nil {
		t.Errorf("discard after publish removed the published blob: %v", err)
	}
}

// Quarantine moves a blob aside without reading it, and refuses one that is
// not there rather than inventing an empty artefact.
func TestQuarantineMovesTheBlobAsideAndPreservesIt(t *testing.T) {
	s := stagingStore(t)
	content := []byte("bytes that are about to be replaced")
	desc, err := s.Put(t.Context(), strings.NewReader(string(content)))
	if err != nil {
		t.Fatal(err)
	}

	path, err := s.Quarantine(desc.Hash)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path) // #nosec G304 -- a test path
	if err != nil {
		t.Fatalf("the quarantined evidence is not readable: %v", err)
	}
	if string(got) != string(content) {
		t.Error("the quarantined artefact is not the bytes that were quarantined")
	}
	has, err := s.Has(t.Context(), desc.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("the blob is still addressable after being quarantined")
	}
	if _, err := s.Quarantine(desc.Hash); !errors.Is(err, cas.ErrNotFound) {
		t.Errorf("quarantining an absent blob = %v, want ErrNotFound", err)
	}
}

// OpenQuarantined reads an artefact back by name, and refuses anything that
// is not a bare name inside quarantine/.
func TestOpenQuarantinedRefusesAnythingButABareName(t *testing.T) {
	s := stagingStore(t)
	desc, err := s.Put(t.Context(), strings.NewReader("evidence"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Quarantine(desc.Hash); err != nil {
		t.Fatal(err)
	}
	listed, err := s.QuarantinedBlobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("quarantine lists %d artefacts, want 1", len(listed))
	}

	rc, err := s.OpenQuarantined(listed[0].Name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()

	for _, bad := range []string{"", "../../etc/passwd", "sub/dir", ".hidden"} {
		if _, err := s.OpenQuarantined(bad); err == nil {
			t.Errorf("OpenQuarantined(%q) was accepted", bad)
		}
	}
}
