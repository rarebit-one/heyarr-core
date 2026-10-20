package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// storePackageStore answers only what recordStorePackage asks; anything else
// would panic on the nil embedded interface, which is the point.
type storePackageStore struct {
	IngestAcquisitionStore
	selected catalog.Candidate
	recorded []string
	pkg      *catalog.VerifiedStorePackage
}

func (s *storePackageStore) SelectedCandidate(context.Context, string) (catalog.Candidate, error) {
	return s.selected, nil
}

func (s *storePackageStore) RecordVerifiedStoreEditions(_ context.Context, ids []string, pkg catalog.VerifiedStorePackage) error {
	s.recorded = append(s.recorded, ids...)
	s.pkg = &pkg
	return nil
}

var ingestedEditions = []ingest.Result{
	{EditionID: "e1", BlobHash: "b1", BlobSize: 10},
	{EditionID: "e2", BlobHash: "b2", BlobSize: 20},
	{EditionID: "", BlobHash: "", BlobSize: 30},
	{EditionID: "e1", BlobHash: "b3", BlobSize: 40},
}

// ingestedBatch is the verified batch those results came from, with the sizes
// the store vouched for.
var ingestedBatch = verifiedTransfer{
	pack: true,
	artifacts: []artifact{
		{Path: "/p/a.exe", Size: 10},
		{Path: "/p/a-1.bin", Size: 20},
		{Path: "/p/a-2.bin", Size: 30},
		{Path: "/p/dlc.exe", Size: 40},
	},
	storeSizes: map[string]int64{"a.exe": 10, "a-1.bin": 20, "a-2.bin": 30, "dlc.exe": 40},
}

// A store release's ingest records the store's facts on every edition it
// filled (the GOG client completed it only after GOG vouched for every file).
func TestAStoreReleaseRecordsItsEditionsAsVerified(t *testing.T) {
	cand := acquisition.StoreReleaseCandidate("some_game", "Some Game", "want")
	s := &storePackageStore{selected: catalog.Candidate{Attributes: cand.Attributes}}
	if verifyErr, err := recordStorePackage(t.Context(), s, "want", ingestedBatch, ingestedEditions); err != nil || verifyErr != nil {
		t.Fatal(verifyErr, err)
	}
	if s.pkg == nil || s.pkg.Store != "gog" || !s.pkg.HasDRMFree || !s.pkg.DRMFree {
		t.Fatalf("recorded %+v, want a DRM-free GOG package", s.pkg)
	}
	if len(s.recorded) != 3 { // e1, e2, e1 — the catalog de-duplicates
		t.Errorf("recorded editions %v", s.recorded)
	}
	// The package is every file the fetch ingested, across its editions: the
	// unit the store vouched for, and the unit placement must cover.
	if len(s.pkg.Blobs) != 3 {
		t.Errorf("package blobs %v, want all three files", s.pkg.Blobs)
	}
}

// Any other release — an indexer's, a feed's — never puts store facts on an
// edition, even one whose files carry a store name.
func TestANonStoreReleaseRecordsNothing(t *testing.T) {
	for name, attrs := range map[string]acquisition.Attributes{
		"an indexer release": {policy.AttrSizeBytes: policy.Num(1 << 30)},
		"a feed enclosure":   {},
	} {
		s := &storePackageStore{selected: catalog.Candidate{Attributes: attrs}}
		if verifyErr, err := recordStorePackage(t.Context(), s, "want", ingestedBatch, ingestedEditions); err != nil || verifyErr != nil {
			t.Fatal(verifyErr, err)
		}
		if s.pkg != nil {
			t.Errorf("%s recorded a verified store package %+v", name, s.pkg)
		}
	}
}

// A fetched GOG package is a directory of installers: the bundle walk keeps
// every installer part — the .exe and each data .bin — and nothing else.
func TestAGOGPackageDirectoryIsAGameBundle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "some_game")
	for _, f := range []string{
		"setup_some_game_1.2_(1234).exe",
		"setup_some_game_1.2_(1234)-1.bin",
		"dlc/extra/setup_some_game_extra_1.0_(99).exe",
		"some_game.xml",
		"folder.jpg",
	} {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	arts, err := verifyDirectory(dir, identification.Game)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 3 {
		names := make([]string, 0, len(arts))
		for _, a := range arts {
			names = append(names, a.RelPath)
		}
		t.Errorf("bundle kept %v, want the three installer files", names)
	}
}

// A store release is held to what the store vouched for: a file deleted or
// truncated between the store's check and ingest fails verification, rather
// than shrinking what is recorded as the whole package.
func TestAStorePackageIsHeldToItsManifest(t *testing.T) {
	writePkg := func(t *testing.T, manifest string) verifiedTransfer {
		t.Helper()
		dir := t.TempDir()
		if manifest != "" {
			if err := os.WriteFile(filepath.Join(dir, acquisition.StoreManifestName), []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return verifiedTransfer{pack: true, path: dir, artifacts: []artifact{
			{Path: filepath.Join(dir, "setup_g_1.0_(1).exe"), Size: 10},
			{Path: filepath.Join(dir, "setup_g_1.0_(1)-1.bin"), Size: 20},
		}}
	}
	store := &storePackageStore{selected: catalog.Candidate{
		Attributes: acquisition.StoreReleaseCandidate("g", "G", "want").Attributes,
	}}
	cases := []struct {
		name, manifest string
		ok             bool
	}{
		{
			"every vouched-for file held at its size",
			`{"store":"gog","files":[{"name":"setup_g_1.0_(1).exe","size":10},{"name":"setup_g_1.0_(1)-1.bin","size":20}]}`, true,
		},
		{
			"a vouched-for part missing",
			`{"store":"gog","files":[{"name":"setup_g_1.0_(1).exe","size":10},{"name":"setup_g_1.0_(1)-2.bin","size":20}]}`, false,
		},
		{
			"a part truncated",
			`{"store":"gog","files":[{"name":"setup_g_1.0_(1).exe","size":10},{"name":"setup_g_1.0_(1)-1.bin","size":99}]}`, false,
		},
		{"no manifest at all", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, verifyErr, err := checkStoreManifest(t.Context(), store, "want", writePkg(t, tc.manifest))
			if err != nil {
				t.Fatal(err)
			}
			if (verifyErr == nil) != tc.ok {
				t.Errorf("verification error %v, want ok=%v", verifyErr, tc.ok)
			}
		})
	}

	// Any other release is not a store package and is not held to one.
	other := &storePackageStore{selected: catalog.Candidate{Attributes: acquisition.Attributes{}}}
	if _, verifyErr, _ := checkStoreManifest(t.Context(), other, "want", writePkg(t, "")); verifyErr != nil {
		t.Errorf("a non-store release was refused: %v", verifyErr)
	}

	// A file the store did not vouch for — an older build's installer left in the
	// directory — is dropped, never ingested as part of the package.
	pkg := writePkg(t, `{"store":"gog","files":[{"name":"setup_g_1.0_(1).exe","size":10}]}`)
	narrowed, verifyErr, err := checkStoreManifest(t.Context(), store, "want", pkg)
	if err != nil || verifyErr != nil {
		t.Fatalf("a package with an extra file was refused: %v %v", verifyErr, err)
	}
	if len(narrowed.artifacts) != 1 || filepath.Base(narrowed.artifacts[0].Path) != "setup_g_1.0_(1).exe" {
		t.Errorf("ingesting %v, want only the vouched-for installer", narrowed.artifacts)
	}
}

// The bytes actually hashed are held to what the store vouched for: a file
// replaced or truncated after the pre-ingest check, but before it was hashed,
// fails verification and nothing is recorded as the package.
func TestAFileThatChangedWhileHashingIsNotVouchedFor(t *testing.T) {
	cand := acquisition.StoreReleaseCandidate("some_game", "Some Game", "want")
	s := &storePackageStore{selected: catalog.Candidate{Attributes: cand.Attributes}}
	changed := append([]ingest.Result(nil), ingestedEditions...)
	changed[1].BlobSize = 7
	verifyErr, err := recordStorePackage(t.Context(), s, "want", ingestedBatch, changed)
	if err != nil {
		t.Fatal(err)
	}
	if verifyErr == nil || s.pkg != nil {
		t.Errorf("verifyErr %v, recorded %+v; a changed file must fail and record nothing", verifyErr, s.pkg)
	}
}

// The store names files by base name only, so a name that appears twice is
// ambiguous and refused, never matched to whichever copy came first.
func TestADuplicateNameInAStorePackageIsRefused(t *testing.T) {
	store := &storePackageStore{selected: catalog.Candidate{
		Attributes: acquisition.StoreReleaseCandidate("g", "G", "want").Attributes,
	}}
	dir := t.TempDir()
	manifest := `{"store":"gog","files":[{"name":"setup_g_1.0_(1).exe","size":10}]}`
	if err := os.WriteFile(filepath.Join(dir, acquisition.StoreManifestName), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	twoCopies := verifiedTransfer{pack: true, path: dir, artifacts: []artifact{
		{Path: filepath.Join(dir, "setup_g_1.0_(1).exe"), Size: 10},
		{Path: filepath.Join(dir, "old", "setup_g_1.0_(1).exe"), Size: 10},
	}}
	if _, verifyErr, err := checkStoreManifest(t.Context(), store, "want", twoCopies); err != nil || verifyErr == nil {
		t.Errorf("two same-named files: verifyErr %v err %v, want refused as ambiguous", verifyErr, err)
	}

	dupManifest := `{"store":"gog","files":[{"name":"x.bin","size":1},{"name":"x.bin","size":1}]}`
	if err := os.WriteFile(filepath.Join(dir, acquisition.StoreManifestName), []byte(dupManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	one := verifiedTransfer{pack: true, path: dir, artifacts: []artifact{{Path: filepath.Join(dir, "x.bin"), Size: 1}}}
	if _, verifyErr, _ := checkStoreManifest(t.Context(), store, "want", one); verifyErr == nil {
		t.Error("a manifest naming one file twice was accepted")
	}
}
