package identification

import "testing"

// A GOG installer set is one edition of one game: the .exe and every -N.bin
// data part land together, keyed by the build, and say they are DRM-free GOG
// Windows bytes — the facts the `owned` profile judges (ADR-0118).
func TestAGOGInstallerSetIsOneGameEdition(t *testing.T) {
	r := Default()
	paths := []string{
		"setup_cyberpunk_2077_2.13_(78456).exe",
		"setup_cyberpunk_2077_2.13_(78456)-1.bin",
		"setup_cyberpunk_2077_2.13_(78456)-12.bin",
	}
	for _, libType := range []string{Game, "", Movie} {
		for _, p := range paths {
			got := r.Identify(p, libType)
			if got.ContentType != Game {
				t.Fatalf("%s (library %q) identified as %q by %q, want a game", p, libType, got.ContentType, got.Rule)
			}
			if got.WorkKey != "game:cyberpunk 2077" {
				t.Errorf("%s: work key %q — the title number 2077 is not a year and must survive", p, got.WorkKey)
			}
			if got.Title != "Cyberpunk 2077" {
				t.Errorf("%s: title %q, want %q", p, got.Title, "Cyberpunk 2077")
			}
			if got.EditionKey != "gog:windows:cyberpunk-2077:78456" {
				t.Errorf("%s: edition key %q — every part of one build must share an edition", p, got.EditionKey)
			}
			attrs := got.EditionAttributes
			if attrs["store"] != "gog" || attrs["drm_free"] != true || attrs["platform"] != "windows" ||
				attrs["build_id"] != "78456" || attrs["version"] != "2.13" {
				t.Errorf("%s: edition attributes %v", p, attrs)
			}
			if got.AssetRole != RolePrimary {
				t.Errorf("%s: role %q, want primary", p, got.AssetRole)
			}
		}
	}
}

// The version is the first dotted numeric token, so title numbers stay in the
// title and tags like "(64bit)" never leak into it.
func TestGOGInstallerNamesSplitTitleFromVersion(t *testing.T) {
	cases := []struct {
		base, title, version, build string
	}{
		{
			"setup_the_witcher_3_wild_hunt_game_of_the_year_edition_4.04a_redkit_update_2_(64bit)_(74297).exe",
			"The Witcher 3 Wild Hunt Game Of The Year Edition", "4.04a redkit update 2", "74297",
		},
		{"setup_baldurs_gate_3_4.1.1.6072089_(74184).exe", "Baldurs Gate 3", "4.1.1.6072089", "74184"},
		{"setup_stardew_valley_(1234)-3.bin", "Stardew Valley", "", "1234"},
	}
	for _, tc := range cases {
		got := Identify(tc.base, "")
		if got.ContentType != Game || got.Title != tc.title {
			t.Errorf("%s: %q titled %q, want game %q", tc.base, got.ContentType, got.Title, tc.title)
			continue
		}
		if v, _ := got.EditionAttributes["version"].(string); v != tc.version {
			t.Errorf("%s: version %q, want %q", tc.base, v, tc.version)
		}
		if b, _ := got.EditionAttributes["build_id"].(string); b != tc.build {
			t.Errorf("%s: build %q, want %q", tc.base, b, tc.build)
		}
	}
}

// Inside a game library, everything under "<Title>/" is that game: the
// installer, its DLC, the store extras and a hand-dropped archive all converge
// on the Work the directory names, which is the name a person curated.
func TestAGameDirectoryConvergesOnOneWork(t *testing.T) {
	r := Default()
	const key = "game:witcher 3 wild hunt"
	cases := []struct {
		path, editionKey, role string
	}{
		{"The Witcher 3 Wild Hunt (2015)/setup_the_witcher_3_wild_hunt_4.04a_(74297).exe", "gog:windows:witcher-3-wild-hunt:74297", RolePrimary},
		{"The Witcher 3 Wild Hunt (2015)/dlc/hearts_of_stone/setup_the_witcher_3_hearts_of_stone_1.10_(12345).exe", "gog:windows:witcher-3-hearts-of-stone:12345", RolePrimary},
		{"The Witcher 3 Wild Hunt (2015)/extras/the_witcher_3_soundtrack.zip", "archive", RoleExtra},
		{"The Witcher 3 Wild Hunt (2015)/extras/manual.pdf", "default", RoleExtra},
		{"The Witcher 3 Wild Hunt (2015)/cover.jpg", "default", RoleArtwork},
		{"The Witcher 3 Wild Hunt (2015)/witcher3-goty.7z", "archive", RolePrimary},
	}
	for _, tc := range cases {
		got := r.Identify(tc.path, Game)
		if got.ContentType != Game || got.WorkKey != key {
			t.Errorf("%s: %q %q by %q, want %q", tc.path, got.ContentType, got.WorkKey, got.Rule, key)
			continue
		}
		if got.EditionKey != tc.editionKey {
			t.Errorf("%s: edition %q, want %q", tc.path, got.EditionKey, tc.editionKey)
		}
		if got.AssetRole != tc.role {
			t.Errorf("%s: role %q, want %q", tc.path, got.AssetRole, tc.role)
		}
		if got.Title != "The Witcher 3 Wild Hunt" || got.Year != 2015 {
			t.Errorf("%s: title %q year %d — the curated directory names the game", tc.path, got.Title, got.Year)
		}
	}
}

// A DepotDownloader tree is one Steam edition per depot manifest, whatever the
// files inside are — including a video that the movie rules would otherwise
// claim when nothing biases the registry. DRM-freeness is unknown and left
// unset, never guessed.
func TestASteamDepotTreeIsOneGameEdition(t *testing.T) {
	r := Default()
	for _, p := range []string{
		"Hades/depots/1145361/7106934245113154441/Hades.exe",
		"Hades/depots/1145361/7106934245113154441/Content/Movies/Intro.mp4",
		"Hades/depots/1145361/7106934245113154441/x64/fmod.dll",
	} {
		for _, libType := range []string{Game, ""} {
			got := r.Identify(p, libType)
			if got.ContentType != Game || got.WorkKey != "game:hades" {
				t.Errorf("%s (library %q): %q %q by %q, want game:hades", p, libType, got.ContentType, got.WorkKey, got.Rule)
				continue
			}
			if got.EditionKey != "steam:1145361:7106934245113154441" {
				t.Errorf("%s: edition %q", p, got.EditionKey)
			}
			if got.EditionAttributes["store"] != "steam" {
				t.Errorf("%s: store %v, want steam", p, got.EditionAttributes["store"])
			}
			if _, set := got.EditionAttributes["drm_free"]; set {
				t.Errorf("%s: drm_free set on a Steam depot; a depot does not say", p)
			}
		}
	}
}

// The generic container shapes are evidence only inside a game library. A
// fake movie torrent's setup.exe, a music folder's artwork zip and an
// unbiased stray archive must not mint phantom game Works (#227's shape).
func TestGenericContainersAreGamesOnlyInAGameLibrary(t *testing.T) {
	r := Default()
	for _, tc := range []struct{ path, libType string }{
		{"Movie.Title.2019.1080p.WEB-DL/setup.exe", Movie},
		{"Artist/Album (2001)/artwork.zip", Music},
		{"Some Game.zip", ""},
		{"Some Game/game.exe", ""},
	} {
		if got := r.Identify(tc.path, tc.libType); got.ContentType == Game {
			t.Errorf("%s in a %q library identified as a game by %q", tc.path, tc.libType, got.Rule)
		}
	}

	if got := r.Identify("Some Game.zip", Game); got.ContentType != Game || got.Title != "Some Game" {
		t.Errorf("a root-level archive in a game library: %q %q, want game %q", got.ContentType, got.Title, "Some Game")
	}
	if got := r.Identify("Some Game.tar.gz", Game); got.Title != "Some Game" {
		t.Errorf("a .tar.gz keeps its .tar out of the title, got %q", got.Title)
	}
	// A root-level loose file that is not a container names nothing.
	if got := r.Identify("readme.txt", Game); got.ContentType == Game {
		t.Errorf("a root-level readme identified as game %q", got.Title)
	}
}

// A want for a game and the installer that satisfies it must agree on the
// key, or wanting and then scanning produces two Works (Describe's contract).
// A store listing's year and a curated directory's "(2015)" are display facts.
func TestAWantedGameConvergesWithItsInstaller(t *testing.T) {
	scanned := Identify("setup_cyberpunk_2077_2.13_(78456).exe", Game)
	for _, tc := range []struct {
		title string
		year  int
	}{
		{"Cyberpunk 2077", 2020},
		{"Cyberpunk 2077", 0},
		{"Cyberpunk 2077 (2020)", 0},
	} {
		key, sort := Describe(Game, tc.title, tc.year)
		if key != scanned.WorkKey {
			t.Errorf("Describe(game, %q, %d) = %q, scanned installer = %q", tc.title, tc.year, key, scanned.WorkKey)
		}
		if sort != scanned.SortTitle {
			t.Errorf("sort title %q, scanned %q", sort, scanned.SortTitle)
		}
	}
	// The wanted Work's display title must keep a title number that parseName
	// would take for a year, or the want records a truncated name the later
	// scan never corrects.
	for _, title := range []string{"Cyberpunk 2077", "F1 2020", "Football Manager 2024"} {
		if got := DisplayTitle(Game, title); got != title {
			t.Errorf("DisplayTitle(game, %q) = %q", title, got)
		}
		key, _ := Describe(Game, title, 0)
		if scanned := Identify(title+"/game.exe", Game); scanned.WorkKey != key || scanned.Title != title {
			t.Errorf("%q: scanned %q %q, wanted key %q", title, scanned.WorkKey, scanned.Title, key)
		}
	}
	// Other content types keep the year-aware display: a film's year is not
	// part of its title.
	if got := DisplayTitle(Movie, "Arrival 2016"); got != "Arrival" {
		t.Errorf("DisplayTitle(movie, %q) = %q, want %q", "Arrival 2016", got, "Arrival")
	}
}
