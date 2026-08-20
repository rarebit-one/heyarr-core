package fixtures

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// smallLibrary keeps the tree shape but shrinks the streaming fixture, so the
// test suite is not writing 200 MB per test. The shape is what these tests are
// about; the size is what the demo is about.
func smallLibrary(t *testing.T) (string, Manifest) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "library")
	m, err := WriteLibrary(root, Options{LargeFileSize: 3 << 20})
	if err != nil {
		t.Fatalf("WriteLibrary: %v", err)
	}
	return root, m
}

func TestTheManifestDescribesWhatIsActuallyOnDisk(t *testing.T) {
	root, m := smallLibrary(t)

	if len(m.Files) == 0 {
		t.Fatal("no files were generated")
	}
	for _, f := range m.Files {
		full := filepath.Join(root, filepath.FromSlash(f.Path))
		info, err := os.Stat(full)
		if err != nil {
			t.Errorf("%s: %v", f.Path, err)
			continue
		}
		if info.Size() != f.Size {
			t.Errorf("%s: manifest says %d bytes, disk says %d", f.Path, f.Size, info.Size())
		}

		// The manifest's hashes are what the demo asserts the catalog against,
		// so a manifest that describes bytes nobody wrote would make the whole
		// exercise circular.
		hash, size, err := hashing.HashFile(full)
		if err != nil {
			t.Errorf("%s: %v", f.Path, err)
			continue
		}
		if size != f.Size {
			t.Errorf("%s: hashed %d bytes, manifest says %d", f.Path, size, f.Size)
		}
		if hash.String() != f.Hash {
			t.Errorf("%s: manifest says %s, the bytes hash to %s", f.Path, f.Hash, hash)
		}
	}
}

// The single most valuable assertion the demo makes: two paths, identical
// bytes, one blob and two assets (§13). If the generator stops producing the
// case, the demo passes while testing nothing — so the generator refuses to
// return a tree that lacks it, and this checks the refusal is real.
func TestTheDuplicatePairIsGeneratedAndUnique(t *testing.T) {
	_, m := smallLibrary(t)

	if m.DuplicateHash == "" {
		t.Fatal("no duplicate pair")
	}
	var paths []string
	for _, f := range m.Files {
		if f.Hash == m.DuplicateHash {
			paths = append(paths, f.Path)
		}
	}
	if len(paths) != 2 {
		t.Fatalf("the duplicate hash covers %d files (%v), want exactly 2", len(paths), paths)
	}
	if filepath.Dir(paths[0]) == filepath.Dir(paths[1]) {
		t.Errorf("the duplicate pair shares a directory (%v) — it must be two different works "+
			"or it does not exercise one-blob-two-assets", paths)
	}

	if want := len(m.Files) - 1; m.DistinctBlobs != want {
		t.Errorf("DistinctBlobs = %d, want %d", m.DistinctBlobs, want)
	}

	// Nothing else may collide, or the arithmetic above stops meaning what it
	// says.
	seen := map[string][]string{}
	for _, f := range m.Files {
		seen[f.Hash] = append(seen[f.Hash], f.Path)
	}
	for hash, ps := range seen {
		if len(ps) > 1 && hash != m.DuplicateHash {
			t.Errorf("unintended collision on %s: %v", hash, ps)
		}
	}
}

func TestGenerationIsDeterministic(t *testing.T) {
	a := filepath.Join(t.TempDir(), "library")
	b := filepath.Join(t.TempDir(), "library")

	ma, err := WriteLibrary(a, Options{LargeFileSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	mb, err := WriteLibrary(b, Options{LargeFileSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}

	if len(ma.Files) != len(mb.Files) {
		t.Fatalf("file counts differ: %d then %d", len(ma.Files), len(mb.Files))
	}
	for i := range ma.Files {
		if ma.Files[i] != mb.Files[i] {
			t.Errorf("file %d differs between runs:\n  %+v\n  %+v", i, ma.Files[i], mb.Files[i])
		}
	}
}

func TestADifferentSeedChangesTheBytes(t *testing.T) {
	// Otherwise Seed is decoration, and a caller who needed two distinct trees
	// would get one and not find out.
	a := filepath.Join(t.TempDir(), "library")
	b := filepath.Join(t.TempDir(), "library")

	ma, err := WriteLibrary(a, Options{LargeFileSize: 1 << 20, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	mb, err := WriteLibrary(b, Options{LargeFileSize: 1 << 20, Seed: 2})
	if err != nil {
		t.Fatal(err)
	}
	if ma.Files[0].Hash == mb.Files[0].Hash {
		t.Error("two seeds produced identical streaming fixtures")
	}
}

// The tree exists to exercise the identifier, so check it actually does rather
// than assuming. This is also an early warning: if a later change to the naive
// identifier stops recognising one of these shapes, the demo's counts move and
// this says why.
func TestTheTreeIsIdentifiableAsIntended(t *testing.T) {
	_, m := smallLibrary(t)

	libraryOf := map[string]string{
		"movies": identification.Movie,
		"tv":     identification.Series,
		"music":  identification.Music,
		"books":  identification.Book,
	}

	identified := 0
	for _, f := range m.Files {
		top, _, _ := strings.Cut(f.Path, "/")
		libType, ok := libraryOf[top]
		if !ok {
			t.Fatalf("%s is not under a known library root", f.Path)
		}
		// The scanner passes the path relative to the ROOT, and each top-level
		// directory is its own root.
		rel := strings.TrimPrefix(f.Path, top+"/")
		cand := identification.Identify(rel, libType)

		if f.ContentType == "unknown" {
			if cand.Identified {
				t.Errorf("%s was expected to defeat the identifier but matched rule %q", f.Path, cand.Rule)
			}
			continue
		}
		if !cand.Identified {
			t.Errorf("%s was not identified", f.Path)
			continue
		}
		identified++
		if cand.ContentType != f.ContentType {
			t.Errorf("%s: identified as %s, manifest says %s", f.Path, cand.ContentType, f.ContentType)
		}
		if cand.AssetRole != f.Role {
			t.Errorf("%s: role %s, manifest says %s", f.Path, cand.AssetRole, f.Role)
		}
	}
	if identified == 0 {
		t.Fatal("nothing was identified — this test would pass on a tree of empty files")
	}
}

func TestTheStreamingFixtureIsTheLargestFile(t *testing.T) {
	_, m := smallLibrary(t)
	if m.LargestPath == "" {
		t.Fatal("no largest file recorded")
	}
	for _, f := range m.Files {
		if f.Size > m.LargestSize {
			t.Errorf("%s is %d bytes, larger than the recorded largest %s at %d",
				f.Path, f.Size, m.LargestPath, m.LargestSize)
		}
	}
	// The range assertions need a file big enough to take several megabyte
	// ranges out of, or they are testing a single read.
	if m.LargestSize < 1<<20 {
		t.Errorf("the streaming fixture is %d bytes, too small to exercise ranges", m.LargestSize)
	}
}

// A fixture that lies about what it is undermines everything built on it: the
// streaming file is the one the range assertions read, and it is named .mkv.
// Generated as raw random bytes it identified as "data" to file(1) — correct
// size, wrong container — so the container header is written first and counted
// within the requested size rather than added on top.
func TestTheStreamingFixtureIsTheContainerItsExtensionClaims(t *testing.T) {
	root, m := smallLibrary(t)

	full := filepath.Join(root, filepath.FromSlash(m.LargestPath))
	f, err := os.Open(full) // #nosec G304 -- generated path under the test root
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(magic, []byte{0x1A, 0x45, 0xDF, 0xA3}) {
		t.Errorf("the streaming fixture starts % x, want the EBML magic 1a 45 df a3", magic)
	}

	info, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != m.LargestSize {
		t.Errorf("size = %d, manifest says %d", info.Size(), m.LargestSize)
	}
	// The header is counted within the requested size, not added to it, or a
	// caller asking for a specific size quietly gets a different one.
	if info.Size() != 3<<20 {
		t.Errorf("asked for %d bytes, got %d", 3<<20, info.Size())
	}
}

func TestManifestJSONIsReadableByAShellScript(t *testing.T) {
	_, m := smallLibrary(t)
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := m.WriteJSON(path); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(path) // #nosec G304 -- test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	var round Manifest
	if err := json.Unmarshal(body, &round); err != nil {
		t.Fatalf("the manifest is not valid JSON: %v", err)
	}
	if round.DuplicateHash != m.DuplicateHash || len(round.Files) != len(m.Files) {
		t.Errorf("the manifest did not round-trip:\n  %+v\n  %+v", m, round)
	}
	if round.DistinctBlobs == 0 {
		t.Error("distinct_blobs did not survive — the demo reads it with jq")
	}
}

func TestWriteLibraryRefusesAnEmptyRoot(t *testing.T) {
	if _, err := WriteLibrary("", Options{}); err == nil {
		t.Fatal("an empty root was accepted")
	}
}
