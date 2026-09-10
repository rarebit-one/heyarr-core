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
