package catalog_test

import (
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// The globs are matched by SQLite, so this pins the class each Kodi/Jellyfin name
// falls in by evaluating the real rank expression in a real migrated database.
func TestArtworkCardRankClassifiesKodiNames(t *testing.T) {
	t.Parallel()
	db := testdb.Migrated(t)
	cases := []struct {
		filename string
		rank     int
	}{
		{"poster.jpg", 0},
		{"Cover.PNG", 0},
		{"folder.jpg", 0},
		{"season01-poster.jpg", 1},
		{"Some Movie (2016).jpg", 1},
		{"fanart.jpg", 2},
		{"backdrop.jpg", 2},
		{"banner.jpg", 2},
		{"Yellowstone (2018) - S04E01 - Half the Money [HDTV-1080p][AC3 5.1][x264]-thumb.jpg", 3},
		{"thumb.jpg", 3},
		{"landscape.jpg", 3},
		{"Movie-landscape.jpg", 3},
		{"clearlogo.png", 3},
		{"logo.png", 3},
		{"clearart.png", 3},
		{"disc.png", 3},
		{"discart.png", 3},
		{"characterart.png", 3},
	}
	rank := catalog.ArtworkCardRankSQL("f")
	for _, tc := range cases {
		var got int
		//nolint:gosec // rank is the catalog's literal expression; the filename is bound
		if err := db.Reader().QueryRow(`SELECT `+rank+` FROM (SELECT ? AS f)`, tc.filename).Scan(&got); err != nil {
			t.Fatalf("%s: %v", tc.filename, err)
		}
		if got != tc.rank {
			t.Errorf("%q ranks %d, want %d", tc.filename, got, tc.rank)
		}
	}
}
