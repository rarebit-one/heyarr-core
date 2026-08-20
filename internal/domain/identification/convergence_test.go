package identification

import "testing"

// An equivalence class is a set of paths that name the same Work. Renaming a
// file from any spelling in the class to any other must not create a second
// Work on the next scan, which means every path in the class must produce
// byte-identical WorkKeys.
//
// Key, where given, pins the expected value as well. Pairwise equality alone
// would still pass if the normaliser degenerated to a constant.
type equivalenceClass struct {
	Name        string
	LibraryType string
	Key         string
	Paths       []string
}

var equivalenceClasses = []equivalenceClass{
	{
		Name:        "movie/separators-and-release-noise",
		LibraryType: Movie,
		Key:         "movie:movie title:2019",
		Paths: []string{
			"Movie Title (2019)/Movie Title (2019) - 2160p.mkv",
			"Movie.Title.2019.2160p.WEB-DL.x265-GRP.mkv",
			"movie_title_2019.mkv",
			"MOVIE.TITLE.2019.720p.HDTV.x264-GRP.mkv",
			"Movie-Title-2019-2160p.mkv",
			"Movies/Movie Title (2019)/Movie Title (2019) [Bluray-1080p].mkv",
			"Movie Title 2019 1080p BluRay x264.mkv",
			"Movie Title (2019)/Movie Title (2019).en.srt",
			"Movie Title (2019)/poster.jpg",
			"Movie Title (2019)/Featurettes/Making Of.mkv",
			"Movie Title (2019)/Subs/Movie Title (2019).eng.forced.srt",
		},
	},
	{
		// Nothing here carries a year, so the release tokens are the only thing
		// standing between these names and four different Works.
		Name:        "movie/no-year-release-noise",
		LibraryType: Movie,
		Key:         "movie:some movie",
		Paths: []string{
			"Some Movie/Some Movie.mkv",
			"Some.Movie.1080p.BluRay.x264-GRP.mkv",
			"Some Movie 2160p.mkv",
			"Some.Movie.WEB-DL.DDP5.1.HDR.x265-GRP.mkv",
			"Some Movie [Bluray-1080p].mkv",
		},
	},
	{
		// "The" has to go, or the curated directory and the scene release are
		// two different films.
		Name:        "movie/leading-article",
		LibraryType: Movie,
		Key:         "movie:matrix:1999",
		Paths: []string{
			"The Matrix (1999)/The Matrix (1999).mkv",
			"The.Matrix.1999.1080p.BluRay.x264-AMIABLE.mkv",
			"the_matrix_1999.mkv",
			"The Matrix (1999)/poster.jpg",
			"Movies/The Matrix (1999)/The Matrix (1999) - Bluray-2160p Remux HDR.mkv",
		},
	},
	{
		// The Work is the series. Season, episode and role must not leak into
		// the key, or every episode becomes its own show.
		Name:        "series/season-and-episode-do-not-shard-the-work",
		LibraryType: Series,
		Key:         "series:expanse:2015",
		Paths: []string{
			"The Expanse (2015)/Season 02/The Expanse - S02E05 - Home.mkv",
			"The Expanse (2015)/Season 02/S02E05.mkv",
			"The Expanse (2015)/Season 2/Episode 5 - Home.mkv",
			"The Expanse (2015)/Season 02/The Expanse - 2x05 - Home.mkv",
			"The Expanse (2015)/Season 03/S03E01.mkv",
			"The Expanse (2015)/Specials/S00E01.mkv",
			"The Expanse (2015)/Season 02/S02E05 - Home.en.srt",
			"The Expanse (2015)/Season 02/season02-poster.jpg",
			"The Expanse (2015)/poster.jpg",
			"The Expanse (2015)/Extras/Gag Reel.mkv",
			"The.Expanse.2015.S02E05.1080p.WEB-DL.DDP5.1.H.264-NTb.mkv",
		},
	},
	{
		Name:        "music/album-is-the-work",
		LibraryType: Music,
		Key:         "music:radiohead:ok computer:1997",
		Paths: []string{
			"Radiohead/OK Computer (1997)/06 - Karma Police.flac",
			"Radiohead/1997 - OK Computer/06 Karma Police.flac",
			"Radiohead/[1997] OK Computer/06 - Karma Police.flac",
			"Music/Radiohead/OK Computer (1997)/cover.jpg",
			"Radiohead/OK Computer (1997)/CD1/06 - Karma Police.flac",
			"Radiohead/OK Computer (1997)/06 - Karma Police.mp3",
			"Radiohead/OK Computer (1997)/Radiohead - OK Computer - 06 - Karma Police.flac",
		},
	},
	{
		Name:        "book/format-and-series-do-not-shard-the-work",
		LibraryType: Book,
		Key:         "book:frank herbert:dune",
		Paths: []string{
			"Frank Herbert/Dune.epub",
			"Frank Herbert/Dune.mobi",
			"Frank Herbert/Dune.azw3",
			"Frank Herbert/Dune Chronicles 01 - Dune.epub",
			"Frank Herbert/Dune.jpg",
			"Calibre/Frank Herbert/Dune (77)/Dune - Frank Herbert.epub",
			"Frank Herbert/Frank Herbert - Dune.epub",
		},
	},
}

// TestWorkKeyConvergence is the acceptance criterion "renaming a file to an
// equivalent form does not create a duplicate Work on rescan", asserted
// pairwise rather than as "not empty".
func TestWorkKeyConvergence(t *testing.T) {
	r := Default()
	for _, class := range equivalenceClasses {
		t.Run(class.Name, func(t *testing.T) {
			if len(class.Paths) < 2 {
				t.Fatalf("an equivalence class of %d proves nothing", len(class.Paths))
			}
			keys := make([]string, len(class.Paths))
			for i, p := range class.Paths {
				c := r.Identify(p, class.LibraryType)
				if !c.Identified {
					t.Errorf("%q was not identified at all", p)
				}
				keys[i] = c.WorkKey
			}
			for i := range keys {
				for j := i + 1; j < len(keys); j++ {
					if keys[i] != keys[j] {
						t.Errorf("WorkKeys diverge — a rescan would create two Works:\n  %q -> %q\n  %q -> %q",
							class.Paths[i], keys[i], class.Paths[j], keys[j])
					}
				}
			}
			if class.Key != "" && keys[0] != class.Key {
				t.Errorf("WorkKey = %q, want %q (every path in the class agreed, but on the wrong key)",
					keys[0], class.Key)
			}
		})
	}
}

// TestDistinctWorksStayDistinct is the other half: normalising hard enough to
// converge must not normalise so hard that different works collide.
func TestDistinctWorksStayDistinct(t *testing.T) {
	r := Default()
	distinct := []struct {
		libraryType string
		path        string
	}{
		{Movie, "Movie Title (2019)/Movie Title (2019).mkv"},
		{Movie, "Movie Title (2021)/Movie Title (2021).mkv"},
		{Movie, "Another Title (2019)/Another Title (2019).mkv"},
		{Movie, "The Matrix (1999)/The Matrix (1999).mkv"},
		{Movie, "The Matrix Reloaded (2003)/The Matrix Reloaded (2003).mkv"},
		{Series, "The Expanse (2015)/Season 01/S01E01.mkv"},
		{Series, "The Wire (2002)/Season 01/S01E01.mkv"},
		{Music, "Radiohead/OK Computer (1997)/01 - Airbag.flac"},
		{Music, "Radiohead/Kid A (2000)/01 - Everything in Its Right Place.flac"},
		{Music, "Placebo/OK Computer (1997)/01 - Airbag.flac"},
		{Book, "Frank Herbert/Dune.epub"},
		{Book, "Frank Herbert/Dune Messiah.epub"},
		{Book, "Brian Herbert/Dune.epub"},
	}
	seen := map[string]string{}
	for _, d := range distinct {
		key := r.Identify(d.path, d.libraryType).WorkKey
		if prev, dup := seen[key]; dup {
			t.Errorf("%q and %q collapsed onto the same WorkKey %q", prev, d.path, key)
		}
		seen[key] = d.path
	}
}
