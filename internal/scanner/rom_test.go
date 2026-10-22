package scanner_test

import (
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/scanner"
)

// A rom library's dumps are scanned into held works (ADR-0121). None of their
// extensions is in the built-in media-type table, so before this every one was
// skipped as an unknown extension and a ROM already on disk could never be
// held. A file the ROM rules cannot identify is still skipped.
func TestARomLibraryScansItsDumpsIntoWorks(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *fixtureOptions) { o.contentType = "rom" })
	f.write("snes/Super Mario World (USA).sfc", "smw")
	f.write("Street Fighter II Turbo (USA).sfc", "sf2t")
	f.write("snes/Donkey Kong Country 2 (Europe).zip", "dkc2")
	f.write("psp/Burnout Legends (Europe).iso", "burnout")
	f.write("Loose Archive.zip", "no platform says what this is")
	f.write("psp/notes.xyz", "not a dump")

	p := f.mustScan()
	if p.FilesEnqueued != 4 || p.FilesSkipped != 2 {
		t.Fatalf("enqueued %d and skipped %d, want the 4 dumps enqueued and the 2 others skipped",
			p.FilesEnqueued, p.FilesSkipped)
	}
	f.ingestPending()

	for _, key := range []string{
		"rom:snes:super mario world",
		"rom:snes:street fighter ii turbo",
		"rom:snes:donkey kong country 2",
		"rom:psp:burnout legends",
	} {
		if got := f.count(`SELECT count(*) FROM works WHERE content_type = 'rom' AND work_key = ?`, key); got != 1 {
			t.Errorf("%d rom works keyed %q, want 1", got, key)
		}
	}
	if got := f.count(`SELECT count(*) FROM assets`); got != 4 {
		t.Errorf("%d assets, want one per dump", got)
	}
}

// Only a rom library keeps a file for being a ROM, and an operator's own
// extension table stays the whole policy even there.
func TestAllowFileInKeepsROMsOnlyInARomLibrary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		policy      scanner.Policy
		rel, ctype  string
		wantOK      bool
		wantMIMESet bool
	}{
		{"a dump in a rom library", scanner.Policy{}, "snes/Game (USA).sfc", "rom", true, false},
		{"a shared shape under a platform", scanner.Policy{}, "psp/Game (USA).iso", "rom", true, false},
		{"a shared shape with no platform", scanner.Policy{}, "Game (USA).iso", "rom", false, false},
		{"a dump in a movie library", scanner.Policy{}, "snes/Game (USA).sfc", "movie", false, false},
		{"a known media type is unchanged", scanner.Policy{}, "snes/Game (USA).png", "rom", true, true},
		{
			"an operator's table is the whole policy",
			scanner.Policy{Extensions: map[string]string{".nes": "application/x-nes-rom"}},
			"snes/Game (USA).sfc", "rom", false, false,
		},
	} {
		mime, ok, _ := tc.policy.AllowFileIn(tc.rel, tc.ctype)
		if ok != tc.wantOK || (mime != "") != tc.wantMIMESet {
			t.Errorf("%s: AllowFileIn(%q, %q) = (%q, %v), want ok=%v with a media type: %v",
				tc.name, tc.rel, tc.ctype, mime, ok, tc.wantOK, tc.wantMIMESet)
		}
	}
}
