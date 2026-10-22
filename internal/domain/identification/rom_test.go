package identification

import (
	"reflect"
	"testing"
)

// The scan rules (ADR-0121 §2): a platform directory names the platform, an
// extension only one platform uses is the fallback, and a shared shape is
// evidence of nothing outside a platform directory.
func TestROMScanRules(t *testing.T) {
	cases := []struct {
		path, wantKey, wantTitle, wantRegion, wantRevision, wantDisc, wantType string
	}{
		{"snes/Super Mario World (USA).sfc", "rom:snes:super mario world", "Super Mario World", "USA", "", "", "cartridge"},
		{"snes/Super Mario World (Europe) (Rev 1).zip", "rom:snes:super mario world", "Super Mario World", "Europe", "Rev 1", "", "archive"},
		{"Pokemon - Red Version (USA, Europe).gb", "rom:gb:pokemon red version", "Pokemon Red Version", "USA, Europe", "", "", "cartridge"},
		{"psx/Final Fantasy VII (USA)/Final Fantasy VII (USA) (Disc 2).bin", "rom:psx:final fantasy vii", "Final Fantasy VII", "USA", "", "2", "disc-image"},
		{"psx/Final Fantasy VII/Final Fantasy VII (USA) (Rev 1) (Disc 1).bin", "rom:psx:final fantasy vii", "Final Fantasy VII", "USA", "Rev 1", "1", "disc-image"},
		{"PS2/Okami (USA).chd", "rom:ps2:okami", "Okami", "USA", "", "", "compressed"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			c := Identify(tc.path, ROM)
			if !c.Identified || c.ContentType != ROM {
				t.Fatalf("not identified as a rom: %+v", c)
			}
			if c.WorkKey != tc.wantKey || c.Title != tc.wantTitle || c.EditionType != tc.wantType {
				t.Errorf("key=%q title=%q type=%q, want %q %q %q", c.WorkKey, c.Title, c.EditionType, tc.wantKey, tc.wantTitle, tc.wantType)
			}
			for attr, want := range map[string]string{"region": tc.wantRegion, "revision": tc.wantRevision, "disc": tc.wantDisc} {
				got, _ := c.EditionAttributes[attr].(string)
				if got != want {
					t.Errorf("edition %s = %q, want %q", attr, got, want)
				}
			}
		})
	}
}

// A shared shape at the library root, or a ROM shape outside a ROM library,
// is not a ROM: `.iso` and `.zip` say "a file", not "a SNES dump".
func TestROMShapesNeedContext(t *testing.T) {
	if c := Identify("Some Game (USA).iso", ROM); c.ContentType == ROM {
		t.Errorf("a root .iso was identified as a rom: %+v", c)
	}
	if c := Identify("snes/notes.txt", ROM); c.ContentType == ROM {
		t.Errorf("a text file in a platform directory was identified as a rom: %+v", c)
	}
	if c := Identify("Super Mario World (USA).sfc", Movie); c.ContentType == ROM {
		t.Errorf("a .sfc in a movie library was identified as a rom: %+v", c)
	}
}

// A want and a scan converge (Describe's whole point): the descriptor's tags
// are not identity, so wanting "Super Mario World (USA)" meets a scanned
// European dump on one Work.
func TestDescribeROMConvergesWithTheScan(t *testing.T) {
	key, _, err := DescribeROM("SNES", "Super Mario World (USA)")
	if err != nil {
		t.Fatal(err)
	}
	if scanned := Identify("snes/Super Mario World (Europe).sfc", ROM).WorkKey; key != scanned {
		t.Errorf("wanted key %q, scanned key %q: they must be one Work", key, scanned)
	}
	if other, _, _ := DescribeROM("gb", "Super Mario World"); other == key {
		t.Error("the same title on two platforms collapsed onto one Work")
	}
	if _, _, err := DescribeROM("playstation", "Okami"); err == nil {
		t.Error("a platform outside the vocabulary was accepted")
	}
	if p, ok := ROMPlatformFromKey(key); !ok || p != "snes" {
		t.Errorf("ROMPlatformFromKey(%q) = %q %v, want snes", key, p, ok)
	}
	if _, ok := ROMPlatformFromKey("game:super mario world"); ok {
		t.Error("a game key was read as naming a rom platform")
	}
	// Describe cannot key a ROM (it has no platform); it must not pretend to.
	if k, sort := Describe(ROM, "Super Mario World (USA)", 0); k != "" || sort != "super mario world" {
		t.Errorf("Describe(rom) = %q %q, want an empty key and the sort title", k, sort)
	}
}

// How a release name spells its platform. The traps are the prefixes: Wii U is
// not Wii, Game Boy Advance is not Game Boy, Neo Geo Pocket Color is not Neo
// Geo. A name spelling two platforms is undetermined.
func TestParseReleasePlatform(t *testing.T) {
	cases := []struct {
		title   string
		want    string
		spelled []string
	}{
		{"Super Mario World (USA) [SNES]", "snes", []string{"snes"}},
		{"Mario Kart 8 [Wii U] [USA]", "wiiu", []string{"wiiu"}},
		{"Super Smash Bros Brawl (Wii) PAL", "wii", []string{"wii"}},
		{"Pokemon Emerald - Game Boy Advance", "gba", []string{"gba"}},
		{"Tetris (World) [GB]", "gb", []string{"gb"}},
		{"Metal Slug - Neo Geo Pocket Color", "ngpc", []string{"ngpc"}},
		{"Okami.PS2.NTSC", "ps2", []string{"ps2"}},
		{"Super Mario World (USA)", "", nil},
		{"Super Mario World [SNES] 2 GB", "snes", []string{"snes"}},
		{"Tetris 2 [GB]", "gb", []string{"gb"}},
		{"Tetris 2.gb", "gb", []string{"gb"}},
		{"Metal Slug.ngc", "ngpc", []string{"ngpc"}},
		{"Metal Slug [NGPC].ngc", "ngpc", []string{"ngpc"}},
		{"Super.Mario.World.USA.SNES.sfc", "snes", []string{"snes"}},
		{"FIFA 2000 (Europe) [GB]", "gb", []string{"gb"}},
		{"Super Mario World [Super Nintendo Entertainment System]", "snes", []string{"snes"}},
		{"Super Mario Bros [Nintendo Entertainment System]", "nes", []string{"nes"}},
		{"Pokemon Crystal 1.4 GB", "", nil},
		{"Sonic Advance (GBA + SNES bundle)", "", []string{"gba", "snes"}},
	}
	for _, tc := range cases {
		t.Run(tc.title, func(t *testing.T) {
			got := ParseReleasePlatform(tc.title)
			if got.Platform != tc.want || got.Determined != (tc.want != "") || !reflect.DeepEqual(got.Spelled, tc.spelled) {
				t.Errorf("got %+v, want platform %q spelled %v", got, tc.want, tc.spelled)
			}
		})
	}
}

// Every platform a want can name is one a release name can spell, by its own
// vocabulary name.
func TestEveryROMPlatformSpellsItself(t *testing.T) {
	for _, p := range ROMPlatforms() {
		if got := ParseReleasePlatform("Some Game (USA) [" + p + "]"); !got.Determined || got.Platform != p {
			t.Errorf("a release tagged [%s] reads as %+v", p, got)
		}
	}
}

// A downloaded disc set's files are all primary ROM files, so the acquisition
// directory walk ingests them rather than refusing the download as holding
// nothing (ADR-0121).
func TestROMExtensionsArePrimary(t *testing.T) {
	for _, ext := range []string{".sfc", ".gba", ".cue", ".ccd", ".bin", ".chd", ".iso", ".zip", ".CUE"} {
		if !IsPrimaryExt(ROM, ext) {
			t.Errorf("%s is not primary for a rom", ext)
		}
	}
	// Every descriptor is ingested beside its image, so each must be primary too.
	for ext := range romDescriptorExts {
		if !IsPrimaryExt(ROM, ext) {
			t.Errorf("%s is not primary for a rom", ext)
		}
	}
	for _, ext := range []string{".txt", ".nfo", ".jpg", ".exe", ".md"} {
		if IsPrimaryExt(ROM, ext) {
			t.Errorf("%s is primary for a rom", ext)
		}
	}
}

// Artwork in a ROM library belongs to the ROM beside it, not to a new Work of
// another type (a picture in a platform directory used to become music).
func TestROMArtworkJoinsItsROM(t *testing.T) {
	for path, key := range map[string]string{
		"snes/Super Mario World (USA).png": "rom:snes:super mario world",
		"psx/Final Fantasy VII/cover.jpg":  "rom:psx:final fantasy vii",
		"psx/Final Fantasy VII/poster.jpg": "rom:psx:final fantasy vii",
	} {
		c := Identify(path, ROM)
		if c.ContentType != ROM || c.WorkKey != key || c.AssetRole != RoleArtwork {
			t.Errorf("%s: %s %q role %q, want rom %q artwork", path, c.ContentType, c.WorkKey, c.AssetRole, key)
		}
		if c.EditionType != "" {
			t.Errorf("%s: artwork set the edition type %q", path, c.EditionType)
		}
	}
}

// What a ROM want can be satisfied by: a primary dump, not artwork, not a
// descriptor, not a stray file.
func TestIsROMDump(t *testing.T) {
	for _, tc := range []struct {
		role, path string
		want       bool
	}{
		{RolePrimary, "/srv/roms/snes/Game (USA).sfc", true},
		{RolePrimary, "/srv/roms/psx/Game/Game (USA).bin", true},
		{RolePrimary, "", true},
		{RoleArtwork, "/srv/roms/snes/Game (USA).png", false},
		{RolePrimary, "/srv/roms/psx/Game/Game (USA).cue", false},
		{RolePrimary, "/srv/roms/snes/readme.txt", false},
	} {
		if got := IsROMDump(tc.role, tc.path); got != tc.want {
			t.Errorf("IsROMDump(%q, %q) = %v, want %v", tc.role, tc.path, got, tc.want)
		}
	}
}

// The regions a release name's tag lists, for the region preference.
func TestParseReleaseRegions(t *testing.T) {
	for title, want := range map[string][]string{
		"Super Mario World (USA) [SNES]":       {"usa"},
		"Pokemon Red Version (USA, Europe).gb": {"usa", "europe"},
		"Tetris (World) (Rev 1)":               {"world"},
		"Some Game (En,Fr,De) (Europe)":        {"europe"},
		"Okami.PS2.NTSC":                       nil,
	} {
		if got := ParseReleaseRegions(title); !reflect.DeepEqual(got, want) {
			t.Errorf("ParseReleaseRegions(%q) = %v, want %v", title, got, want)
		}
	}
}
