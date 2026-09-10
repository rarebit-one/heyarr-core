package identification

import "testing"

// Real public release names (ADR-0026): a title asserts its own quality, so
// these are the fixtures. Each recognised token and each deliberately
// unrecognised name is a case, so a branch of the table that never saw a real
// name is fixtured or absent.
func TestParseReleaseQuality(t *testing.T) {
	cases := []struct {
		name  string
		title string
		want  ReleaseQuality
	}{
		{
			name:  "1080p web-dl hevc",
			title: "Slow.Horses.S01E01.1080p.WEB-DL.DDP5.1.x265-GRP",
			want:  ReleaseQuality{Resolution: "1080p", Source: "web-dl", Codec: "x265"},
		},
		{
			name:  "2160p uhd bluray remux with hdr",
			title: "Some.Film.2019.2160p.UHD.BluRay.REMUX.HDR.HEVC-GRP",
			want:  ReleaseQuality{Resolution: "2160p", Source: "remux", Codec: "x265", DynamicRange: "HDR"},
		},
		{
			name:  "720p webrip avc",
			title: "Some.Show.S02E03.720p.WEBRip.x264-GRP",
			want:  ReleaseQuality{Resolution: "720p", Source: "webrip", Codec: "x264"},
		},
		{
			name:  "dv and hdr together",
			title: "Some.Film.2160p.ATVP.WEB-DL.DV.HDR.H265-GRP",
			want:  ReleaseQuality{Resolution: "2160p", Source: "web-dl", Codec: "x265", DynamicRange: "DV HDR"},
		},
		{
			name:  "xvid with no resolution token",
			title: "Some.Old.Show.S01E01.XviD-AFG",
			want:  ReleaseQuality{Codec: "xvid"},
		},
		{
			name:  "4k folds to 2160p",
			title: "Some.Movie.4K.BluRay.x265",
			want:  ReleaseQuality{Resolution: "2160p", Source: "bluray", Codec: "x265"},
		},
		{
			name:  "nothing recognisable is empty",
			title: "Some Perfectly Ordinary Title",
			want:  ReleaseQuality{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseReleaseQuality(tc.title)
			if got != tc.want {
				t.Errorf("ParseReleaseQuality(%q) = %+v, want %+v", tc.title, got, tc.want)
			}
			if got.Empty() != (tc.want == ReleaseQuality{}) {
				t.Errorf("Empty() = %v, want %v", got.Empty(), tc.want == ReleaseQuality{})
			}
		})
	}
}

// Real public release names (ADR-0026): the season/episode a release name spells
// out, read by the same regexes the scanner uses on a filename (ADR-0093).
func TestParseReleaseSeasonEpisode(t *testing.T) {
	cases := []struct {
		name  string
		title string
		want  ReleaseSeason
	}{
		{
			name:  "single episode",
			title: "Slow.Horses.S01E01.1080p.WEB-DL.DDP5.1.x265-GRP",
			want:  ReleaseSeason{Determined: true, Season: 1, Episodes: []int{1}},
		},
		{
			name:  "whole-season pack, sxx",
			title: "Slow.Horses.S03.COMPLETE.2160p.WEB-DL.x265-GRP",
			want:  ReleaseSeason{Determined: true, Season: 3},
		},
		{
			name:  "whole-season pack, season word",
			title: "Slow Horses Season 2 1080p WEB-DL",
			want:  ReleaseSeason{Determined: true, Season: 2},
		},
		{
			name:  "multi-episode file",
			title: "Slow.Horses.S02E01E02.1080p.WEB-DL.x264-GRP",
			want:  ReleaseSeason{Determined: true, Season: 2, Episodes: []int{1, 2}},
		},
		{
			name:  "nxnn spelling",
			title: "Slow Horses 2x05 720p HDTV x264-GRP",
			want:  ReleaseSeason{Determined: true, Season: 2, Episodes: []int{5}},
		},
		{
			name:  "specials episode is season zero",
			title: "Slow.Horses.S00E01.1080p.WEB-DL.x265-GRP",
			want:  ReleaseSeason{Determined: true, Season: 0, Episodes: []int{1}},
		},
		{
			name:  "no season marker is undetermined",
			title: "Slow.Horses.1080p.WEB-DL.x265-GRP",
			want:  ReleaseSeason{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseReleaseSeasonEpisode(tc.title)
			if got.Determined != tc.want.Determined || got.Season != tc.want.Season {
				t.Errorf("ParseReleaseSeasonEpisode(%q) = %+v, want %+v", tc.title, got, tc.want)
			}
			if len(got.Episodes) != len(tc.want.Episodes) {
				t.Fatalf("episodes = %v, want %v", got.Episodes, tc.want.Episodes)
			}
			for i := range got.Episodes {
				if got.Episodes[i] != tc.want.Episodes[i] {
					t.Errorf("episodes = %v, want %v", got.Episodes, tc.want.Episodes)
				}
			}
		})
	}
}

// ContainsEpisode is the containment rule the gate leans on: an S01E01 want is
// contained by exactly that episode and by an S01 pack, and never by another
// season or another episode.
func TestReleaseSeasonContainsEpisode(t *testing.T) {
	cases := []struct {
		name            string
		rel             ReleaseSeason
		season, episode int
		want            bool
	}{
		{"exact episode", ReleaseSeason{Determined: true, Season: 1, Episodes: []int{1}}, 1, 1, true},
		{"season pack of same season", ReleaseSeason{Determined: true, Season: 1}, 1, 1, true},
		{"multi-episode covering it", ReleaseSeason{Determined: true, Season: 1, Episodes: []int{1, 2}}, 1, 2, true},
		{"different season episode", ReleaseSeason{Determined: true, Season: 3, Episodes: []int{1}}, 1, 1, false},
		{"different season pack", ReleaseSeason{Determined: true, Season: 3}, 1, 1, false},
		{"same season wrong episode", ReleaseSeason{Determined: true, Season: 1, Episodes: []int{5}}, 1, 1, false},
		{"undetermined contains nothing", ReleaseSeason{}, 1, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rel.ContainsEpisode(tc.season, tc.episode); got != tc.want {
				t.Errorf("ContainsEpisode(%d,%d) = %v, want %v", tc.season, tc.episode, got, tc.want)
			}
		})
	}
}
