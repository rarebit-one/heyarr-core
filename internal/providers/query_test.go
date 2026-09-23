package providers

import "testing"

// ADR-0093: the query carries a season and episode, and Episode > 0 is the
// sentinel for an episode search — season zero (Specials) must not read as
// "not an episode search".
func TestQueryIsEpisode(t *testing.T) {
	cases := []struct {
		name string
		q    Query
		want bool
	}{
		{"no season or episode", Query{Title: "Slow Horses"}, false},
		{"season only is not an episode search", Query{Title: "Slow Horses", Season: 1}, false},
		{"season and episode", Query{Title: "Slow Horses", Season: 1, Episode: 1}, true},
		{"specials episode", Query{Title: "Slow Horses", Season: 0, Episode: 1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.q.IsEpisode(); got != tc.want {
				t.Errorf("IsEpisode() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A season-only query is not an episode search, so Season alone changes
// nothing — guard that the sentinel is Episode.
func TestQuerySearchTerm(t *testing.T) {
	cases := []struct {
		name string
		q    Query
		want string
	}{
		{"plain title", Query{Title: "Slow Horses"}, "Slow Horses"},
		{"title with year", Query{Title: "Slow Horses", Year: 2022}, "Slow Horses 2022"},
		{"episode search drops the year", Query{Title: "Slow Horses", Year: 2022, Season: 1, Episode: 1}, "Slow Horses S01E01"},
		{"episode search is zero-padded", Query{Title: "Slow Horses", Season: 12, Episode: 3}, "Slow Horses S12E03"},
		{"specials episode", Query{Title: "Slow Horses", Season: 0, Episode: 1}, "Slow Horses S00E01"},
		{"season only is not an episode search", Query{Title: "Slow Horses", Season: 1}, "Slow Horses"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.q.SearchTerm(); got != tc.want {
				t.Errorf("SearchTerm() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestQueryValidateRejectsNegativeSeasonEpisode(t *testing.T) {
	if err := (Query{Title: "x", Season: -1}).Validate(); err == nil {
		t.Error("a negative season should be rejected")
	}
	if err := (Query{Title: "x", Episode: -1}).Validate(); err == nil {
		t.Error("a negative episode should be rejected")
	}
	if err := (Query{Title: "x", Season: 0, Episode: 1}).Validate(); err != nil {
		t.Errorf("a Specials episode is valid: %v", err)
	}
}
