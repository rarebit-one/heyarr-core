package identification

import "testing"

func TestMatchMusicTags(t *testing.T) {
	cases := []struct {
		name, title, artist string
		tags                map[string]string
		want                bool
	}{
		{"exact", "Time After Time", "Cyndi Lauper", map[string]string{"TITLE": "Time After Time", "ARTIST": "Cyndi Lauper"}, true},
		{"conflicting keys", "Jump", "Van Halen", map[string]string{"title": "Jump", "artist": "Van Halen", "ARTIST": "Other"}, false},
		{"wrong artist", "Time After Time", "Cyndi Lauper", map[string]string{"title": "Time After Time", "artist": "Tuck & Patti"}, false},
		{"different performance", "Jump", "Van Halen", map[string]string{"title": "Jump (Live)", "artist": "Van Halen"}, false},
		{"wrong title", "Jump", "Van Halen", map[string]string{"title": "Jump Jump Jump", "artist": "Van Halen"}, false},
		{"missing", "Jump", "Van Halen", map[string]string{"title": "Jump"}, false},
		{"punctuation", "What's Love Got to Do with It", "Tina Turner", map[string]string{"title": "What’s Love Got to Do with It", "artist": "Tina Turner"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := MatchMusicTags(tc.title, tc.artist, tc.tags)
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestMusicReleaseCouldContain(t *testing.T) {
	for _, tc := range []struct {
		release string
		want    bool
	}{
		{"Van Halen - Jump (1984) FLAC", true},
		{"Van Halen - Greatest Hits 1996 FLAC", true},
		{"Van Halen - The Album FLAC", true},
		{"Van Halen - Greatest Hits Live", false},
		{"Koyote - Greatest Hits Jump", false},
		{"Van Halen Tribute - Greatest Hits", false},
		{"Koyote - Jump Jump Jump", false},
		{"Van Halen - Jump Live 1990", false},
		{"Van Halen - Jumping", false},
	} {
		if got := MusicReleaseCouldContain(tc.release, "Jump", "Van Halen"); got != tc.want {
			t.Errorf("%q: got %v", tc.release, got)
		}
	}
}

func TestMusicReleaseCouldContainCompilationWithoutSongTitle(t *testing.T) {
	if !MusicReleaseCouldContain("Cyndi Lauper - The Essential Cyndi Lauper (Best Of)", "Time After Time", "Cyndi Lauper") {
		t.Fatal("artist compilation rejected")
	}
	if MusicReleaseCouldContain("Tuck & Patti - Best Of Time After Time", "Time After Time", "Cyndi Lauper") {
		t.Fatal("wrong artist compilation accepted")
	}
	if MusicReleaseCouldContain("Cyndi Lauper - Best Of (Live)", "Time After Time", "Cyndi Lauper") {
		t.Fatal("live compilation accepted")
	}
}
