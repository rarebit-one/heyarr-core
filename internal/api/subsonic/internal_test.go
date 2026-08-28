package subsonic

import "testing"

func TestIDRoundTrip(t *testing.T) {
	t.Run("album", func(t *testing.T) {
		got, ok := decodeAlbumID(albumID("work-123"))
		if !ok || got != "work-123" {
			t.Fatalf("album id round-trip = %q, %v", got, ok)
		}
	})
	t.Run("track", func(t *testing.T) {
		got, ok := decodeTrackID(trackID("edition-456"))
		if !ok || got != "edition-456" {
			t.Fatalf("track id round-trip = %q, %v", got, ok)
		}
	})
	t.Run("artist survives awkward names", func(t *testing.T) {
		for _, name := range []string{"Bearing", "AC/DC", "Sigur Rós", "a b  c", "?query=1&x=2"} {
			got, ok := decodeArtistID(artistID(name))
			if !ok || got != name {
				t.Fatalf("artist id round-trip of %q = %q, %v", name, got, ok)
			}
		}
	})
}

func TestDecodeRejectsWrongKind(t *testing.T) {
	// A track id is not an album id; feeding one to the other must not decode,
	// or a client's mixed-up id would query the wrong table.
	if _, ok := decodeAlbumID(trackID("x")); ok {
		t.Fatal("a track id decoded as an album id")
	}
	if _, ok := decodeTrackID(albumID("x")); ok {
		t.Fatal("an album id decoded as a track id")
	}
	if _, ok := decodeArtistID("ar:%%%not-base64%%%"); ok {
		t.Fatal("invalid base64 decoded as an artist id")
	}
	if _, ok := decodeAlbumID("al:"); ok {
		t.Fatal("an empty album id decoded")
	}
}

func TestIndexKey(t *testing.T) {
	cases := map[string]string{
		"azimuth":       "A",
		"Contour Lines": "C",
		"9 Lives":       "#",
		"":              "#",
		"  spaced":      "S",
	}
	for in, want := range cases {
		if got := indexKey(in); got != want {
			t.Errorf("indexKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStripArticle(t *testing.T) {
	cases := map[string]string{
		"The Cartographers": "Cartographers",
		"Theremin":          "Theremin", // not a whole-word article
		"La Vie":            "Vie",
		"Los Angeles":       "Angeles",
		"Bearing":           "Bearing",
	}
	for in, want := range cases {
		if got := stripArticle(in); got != want {
			t.Errorf("stripArticle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSuffixOf(t *testing.T) {
	if got := suffixOf("01 - Datum.FLAC", "flac"); got != "flac" {
		t.Errorf("filename suffix = %q, want flac", got)
	}
	if got := suffixOf("", "MP3"); got != "mp3" {
		t.Errorf("fallback suffix = %q, want mp3", got)
	}
}

func TestAlbumOrderPersonalTypes(t *testing.T) {
	// The three history/starred types are personal state the server cannot read;
	// they must be flagged so the endpoint returns empty rather than an ORDER BY
	// that fabricates a ranking.
	for _, personal := range []string{"recent", "frequent", "starred"} {
		if _, isPersonal := albumOrder(personal); !isPersonal {
			t.Errorf("albumOrder(%q) should be flagged personal", personal)
		}
	}
	for _, catalogue := range []string{"", "newest", "alphabeticalByName", "byYear", "random"} {
		if _, isPersonal := albumOrder(catalogue); isPersonal {
			t.Errorf("albumOrder(%q) should be a catalogue read", catalogue)
		}
	}
}
