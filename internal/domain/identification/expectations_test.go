package identification

import (
	"reflect"
	"testing"
)

// These expectations are written by hand, on purpose. The golden files are
// generated from the code under test, so on their own they only prove that
// nothing changed — including a wrong answer that was wrong from the start.
// Everything the rest of the system actually depends on is pinned here.
func TestLoadBearingFields(t *testing.T) {
	r := Default()

	tests := []struct {
		path        string
		libraryType string

		contentType  string
		workKey      string
		title        string
		sortTitle    string
		year         int
		editionKey   string
		editionLabel string
		editionType  string
		language     string
		assetRole    string
		rule         string
		work         map[string]any
		edition      map[string]any
	}{
		{
			path: "Movie Title (2019)/Movie Title (2019) - 2160p.mkv", libraryType: Movie,
			contentType: Movie, workKey: "movie:movie title:2019", title: "Movie Title",
			sortTitle: "movie title", year: 2019, editionKey: "2160p", editionLabel: "2160p",
			assetRole: RolePrimary, rule: "movie/title-year-dir",
			work: map[string]any{}, edition: map[string]any{"resolution": "2160p"},
		},
		{
			path: "Movie.Title.2019.2160p.WEB-DL.x265-GRP.mkv", libraryType: Movie,
			contentType: Movie, workKey: "movie:movie title:2019", title: "Movie Title",
			sortTitle: "movie title", year: 2019, editionKey: "2160p-web-dl-x265",
			editionLabel: "2160p", editionType: "web-dl", assetRole: RolePrimary,
			rule: "movie/title-year", work: map[string]any{},
			edition: map[string]any{"resolution": "2160p", "source": "web-dl", "codec": "x265"},
		},
		{
			path: "movie_title_2019.mkv", libraryType: Movie,
			contentType: Movie, workKey: "movie:movie title:2019", title: "Movie Title",
			sortTitle: "movie title", year: 2019, editionKey: "default",
			assetRole: RolePrimary, rule: "movie/title-year",
			work: map[string]any{}, edition: map[string]any{},
		},
		{
			path: "The.Matrix.1999.1080p.BluRay.x264-AMIABLE.mkv", libraryType: Movie,
			contentType: Movie, workKey: "movie:matrix:1999", title: "The Matrix",
			sortTitle: "matrix", year: 1999, editionKey: "1080p-bluray-x264",
			editionLabel: "1080p", editionType: "bluray", assetRole: RolePrimary,
			rule: "movie/title-year", work: map[string]any{},
			edition: map[string]any{"resolution": "1080p", "source": "bluray", "codec": "x264"},
		},
		{
			// The year window is what keeps a number in the title from being
			// read as a release year.
			path: "Movies/Blade Runner 2049 (2017)/Blade Runner 2049 (2017) Bluray-1080p.mkv", libraryType: Movie,
			contentType: Movie, workKey: "movie:blade runner 2049:2017", title: "Blade Runner 2049",
			sortTitle: "blade runner 2049", year: 2017, editionKey: "1080p-bluray",
			editionLabel: "1080p", editionType: "bluray", assetRole: RolePrimary,
			rule: "movie/title-year-dir", work: map[string]any{},
			edition: map[string]any{"resolution": "1080p", "source": "bluray"},
		},
		{
			path: "Movie Title (2019)/poster.jpg", libraryType: Movie,
			contentType: Movie, workKey: "movie:movie title:2019", title: "Movie Title",
			sortTitle: "movie title", year: 2019, editionKey: "default",
			assetRole: RoleArtwork, rule: "movie/title-year",
			work: map[string]any{}, edition: map[string]any{},
		},
		{
			path: "Movie Title (2019)/Movie Title (2019).fr.srt", libraryType: Movie,
			contentType: Movie, workKey: "movie:movie title:2019", title: "Movie Title",
			sortTitle: "movie title", year: 2019, editionKey: "default", language: "fr",
			assetRole: RoleSubtitle, rule: "movie/title-year-dir",
			work: map[string]any{}, edition: map[string]any{},
		},
		{
			path: "Movie Title (2019)/Subs/Movie Title (2019).eng.forced.srt", libraryType: Movie,
			contentType: Movie, workKey: "movie:movie title:2019", title: "Movie Title",
			sortTitle: "movie title", year: 2019, editionKey: "default", language: "en",
			assetRole: RoleSubtitle, rule: "movie/title-year-dir",
			work: map[string]any{}, edition: map[string]any{},
		},
		{
			path: "Movie Title (2019)/Featurettes/Deleted Scene.mkv", libraryType: Movie,
			contentType: Movie, workKey: "movie:movie title:2019", title: "Movie Title",
			sortTitle: "movie title", year: 2019, editionKey: "default",
			assetRole: RoleExtra, rule: "movie/title-year",
			work: map[string]any{}, edition: map[string]any{},
		},
		{
			path: "Series/Season 02/S02E05 - Title.mkv", libraryType: Series,
			contentType: Series, workKey: "series:series", title: "Series",
			sortTitle: "series", editionKey: "season-02", editionLabel: "Season 02",
			assetRole: RolePrimary, rule: "series/sxxexx", work: map[string]any{},
			edition: map[string]any{"season": 2, "episode": 5, "episode_title": "Title"},
		},
		{
			path: "The Wire (2002)/Season 01/The Wire - S01E01 - The Target.mkv", libraryType: Series,
			contentType: Series, workKey: "series:wire:2002", title: "The Wire",
			sortTitle: "wire", year: 2002, editionKey: "season-01", editionLabel: "Season 01",
			assetRole: RolePrimary, rule: "series/sxxexx", work: map[string]any{},
			edition: map[string]any{"season": 1, "episode": 1, "episode_title": "The Target"},
		},
		{
			path: "Show/Season 02/Show.s02e05e06.1080p.mkv", libraryType: Series,
			contentType: Series, workKey: "series:show", title: "Show", sortTitle: "show",
			editionKey: "season-02", editionLabel: "Season 02", assetRole: RolePrimary,
			rule: "series/sxxexx", work: map[string]any{},
			edition: map[string]any{"season": 2, "episode": 5, "episodes": []int{5, 6}},
		},
		{
			path: "Series/Specials/S00E01 - Christmas Special.mkv", libraryType: Series,
			contentType: Series, workKey: "series:series", title: "Series", sortTitle: "series",
			editionKey: "season-00", editionLabel: "Specials", assetRole: RolePrimary,
			rule: "series/sxxexx", work: map[string]any{},
			edition: map[string]any{"season": 0, "episode": 1, "episode_title": "Christmas Special"},
		},
		{
			path: "Show Name/Season 2/Show Name - 2x05 - Episode.mkv", libraryType: Series,
			contentType: Series, workKey: "series:show name", title: "Show Name",
			sortTitle: "show name", editionKey: "season-02", editionLabel: "Season 02",
			assetRole: RolePrimary, rule: "series/nxnn", work: map[string]any{},
			edition: map[string]any{"season": 2, "episode": 5, "episode_title": "Episode"},
		},
		{
			path: "Show Name/Season 2/05 - Title.mkv", libraryType: Series,
			contentType: Series, workKey: "series:show name", title: "Show Name",
			sortTitle: "show name", editionKey: "season-02", editionLabel: "Season 02",
			assetRole: RolePrimary, rule: "series/season-dir", work: map[string]any{},
			edition: map[string]any{"season": 2, "episode": 5, "episode_title": "Title"},
		},
		{
			path: "Artist/Album (2001)/03 - Track.flac", libraryType: Music,
			contentType: Music, workKey: "music:artist:album:2001", title: "Album",
			sortTitle: "album", year: 2001, editionKey: "flac", editionLabel: "FLAC",
			editionType: "lossless", assetRole: RolePrimary, rule: "music/artist-dir-album-dir",
			work:    map[string]any{"artist": "Artist"},
			edition: map[string]any{"track": 3, "track_title": "Track"},
		},
		{
			path: "Artist/2001 - Album/1-03 Track.flac", libraryType: Music,
			contentType: Music, workKey: "music:artist:album:2001", title: "Album",
			sortTitle: "album", year: 2001, editionKey: "flac", editionLabel: "FLAC",
			editionType: "lossless", assetRole: RolePrimary, rule: "music/artist-dir-album-dir",
			work:    map[string]any{"artist": "Artist"},
			edition: map[string]any{"disc": 1, "track": 3, "track_title": "Track"},
		},
		{
			path: "Artist - Album/01 Track.mp3", libraryType: Music,
			contentType: Music, workKey: "music:artist:album", title: "Album",
			sortTitle: "album", editionKey: "mp3", editionLabel: "MP3",
			editionType: "lossy", assetRole: RolePrimary, rule: "music/artist-album-dir",
			work:    map[string]any{"artist": "Artist"},
			edition: map[string]any{"track": 1, "track_title": "Track"},
		},
		{
			path: "Artist/Album (2001)/cover.jpg", libraryType: Music,
			contentType: Music, workKey: "music:artist:album:2001", title: "Album",
			sortTitle: "album", year: 2001, editionKey: "default",
			assetRole: RoleArtwork, rule: "music/companion",
			work: map[string]any{"artist": "Artist"}, edition: map[string]any{},
		},
		{
			path: "Author/Title.epub", libraryType: Book,
			contentType: Book, workKey: "book:author:title", title: "Title",
			sortTitle: "title", editionKey: "epub", editionLabel: "EPUB",
			editionType: "epub", assetRole: RolePrimary, rule: "book/author-dir",
			work: map[string]any{"author": "Author"}, edition: map[string]any{},
		},
		{
			path: "Author/Series 03 - Title.epub", libraryType: Book,
			contentType: Book, workKey: "book:author:title", title: "Title",
			sortTitle: "title", editionKey: "epub", editionLabel: "EPUB",
			editionType: "epub", assetRole: RolePrimary, rule: "book/author-dir-series",
			work:    map[string]any{"author": "Author", "series": "Series", "series_index": 3},
			edition: map[string]any{},
		},
		{
			path: "Author - Title (2011).epub", libraryType: Book,
			contentType: Book, workKey: "book:author:title:2011", title: "Title",
			sortTitle: "title", year: 2011, editionKey: "epub", editionLabel: "EPUB",
			editionType: "epub", assetRole: RolePrimary, rule: "book/author-title",
			work: map[string]any{"author": "Author"}, edition: map[string]any{},
		},
		{
			path: "Calibre/Isaac Asimov/Foundation (123)/Foundation - Isaac Asimov.epub", libraryType: Book,
			contentType: Book, workKey: "book:isaac asimov:foundation", title: "Foundation",
			sortTitle: "foundation", editionKey: "epub", editionLabel: "EPUB",
			editionType: "epub", assetRole: RolePrimary, rule: "book/calibre",
			work: map[string]any{"author": "Isaac Asimov"}, edition: map[string]any{},
		},
		{
			path: "Author/Title.cbz", libraryType: Book,
			contentType: Book, workKey: "book:author:title", title: "Title",
			sortTitle: "title", editionKey: "cbz", editionLabel: "CBZ",
			editionType: "cbz", assetRole: RolePrimary, rule: "book/author-dir",
			work:    map[string]any{"author": "Author"},
			edition: map[string]any{"format": "comic"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			got := r.Identify(tc.path, tc.libraryType)
			check(t, "ContentType", got.ContentType, tc.contentType)
			check(t, "WorkKey", got.WorkKey, tc.workKey)
			check(t, "Title", got.Title, tc.title)
			check(t, "SortTitle", got.SortTitle, tc.sortTitle)
			check(t, "Year", got.Year, tc.year)
			check(t, "EditionKey", got.EditionKey, tc.editionKey)
			check(t, "EditionLabel", got.EditionLabel, tc.editionLabel)
			check(t, "EditionType", got.EditionType, tc.editionType)
			check(t, "Language", got.Language, tc.language)
			check(t, "AssetRole", got.AssetRole, tc.assetRole)
			check(t, "Rule", got.Rule, tc.rule)
			check(t, "Source", got.Source, SourcePathHeuristic)
			check(t, "Identified", got.Identified, true)
			if !reflect.DeepEqual(got.WorkAttributes, tc.work) {
				t.Errorf("WorkAttributes = %#v, want %#v", got.WorkAttributes, tc.work)
			}
			if !reflect.DeepEqual(got.EditionAttributes, tc.edition) {
				t.Errorf("EditionAttributes = %#v, want %#v", got.EditionAttributes, tc.edition)
			}
		})
	}
}

func check[T comparable](t *testing.T, field string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", field, got, want)
	}
}
