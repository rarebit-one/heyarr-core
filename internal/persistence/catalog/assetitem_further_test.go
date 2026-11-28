package catalog

import (
	"slices"
	"testing"
)

// furtherEpisodeKeys links only what the name spells AND agrees with the
// asserted item about (#939): a mislabelled or unrelated name never spreads an
// asset onto episodes it was not acquired for.
func TestFurtherEpisodeKeys(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, anchor, file string
		want               []string
	}{
		{"two episodes", "S03E08", "Show.S03E08E09.720p.WEB-DL.mkv", []string{"S03E09"}},
		{"three episodes", "S01E01", "Show.S01E01E02E03.mkv", []string{"S01E02", "S01E03"}},
		{"anchor is the second episode", "S03E09", "Show.S03E08E09.mkv", []string{"S03E08"}},
		{"a path, not a bare name", "S03E08", "Show S03/Show.S03E08E09.mkv", []string{"S03E09"}},
		{"single episode", "S03E08", "Show.S03E08.720p.mkv", nil},
		{"season pack name", "S03E08", "Show.S03.1080p.mkv", nil},
		{"another season", "S03E08", "Show.S04E08E09.mkv", nil},
		{"does not hold the anchor", "S03E08", "Show.S03E10E11.mkv", nil},
		{"no season marker", "S03E08", "featurette.mkv", nil},
		{"anchor is not an episode key", "a1b2c3", "Show.S03E08E09.mkv", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := furtherEpisodeKeys(tc.anchor, tc.file); !slices.Equal(got, tc.want) {
				t.Errorf("furtherEpisodeKeys(%q, %q) = %v, want %v", tc.anchor, tc.file, got, tc.want)
			}
		})
	}
}
