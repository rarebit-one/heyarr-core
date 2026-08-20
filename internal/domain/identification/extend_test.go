// This file is deliberately in the external test package: it may only use what
// a caller in another package could use. If registering a new content type ever
// needs an unexported helper, this file stops compiling.
package identification_test

import (
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
)

// A fourth content type, registered from outside the package with no schema
// change, no new column, and no edit to Identify — the acceptance criterion
// "registering a fourth content type requires no schema change", executed.
const podcast = "podcast"

func podcastRules() []identification.Rule {
	return []identification.Rule{
		{
			Name:        "podcast/show-dir-dated",
			ContentType: podcast,
			Match: func(p identification.Path) (identification.Candidate, bool) {
				if p.Ext != ".mp3" && p.Ext != ".m4a" || len(p.Dirs) == 0 {
					return identification.Candidate{}, false
				}
				date, title, ok := strings.Cut(p.Stem, " - ")
				if !ok || len(date) != len("2006-01-02") {
					return identification.Candidate{}, false
				}
				show := p.Dirs[len(p.Dirs)-1]
				return identification.Candidate{
					WorkKey:           podcast + ":" + strings.ToLower(show),
					Title:             show,
					SortTitle:         strings.ToLower(show),
					WorkAttributes:    map[string]any{},
					EditionAttributes: map[string]any{"published": date, "episode_title": title},
					EditionKey:        date[:4],
					EditionLabel:      date[:4],
					EditionType:       "feed",
				}, true
			},
		},
	}
}

func TestRegisteringAFourthContentType(t *testing.T) {
	r := identification.NewRegistry()
	r.Register(podcastRules()...)

	got := r.Identify("Podcasts/The Bike Shed/2021-05-01 - Episode Title.mp3", podcast)

	if got.ContentType != podcast {
		t.Errorf("ContentType = %q, want %q", got.ContentType, podcast)
	}
	if got.WorkKey != "podcast:the bike shed" {
		t.Errorf("WorkKey = %q, want %q", got.WorkKey, "podcast:the bike shed")
	}
	if got.Rule != "podcast/show-dir-dated" {
		t.Errorf("Rule = %q, want %q", got.Rule, "podcast/show-dir-dated")
	}
	if got.Source != identification.SourcePathHeuristic || !got.Identified {
		t.Errorf("Source = %q, Identified = %v", got.Source, got.Identified)
	}
	if got.AssetRole != "primary" {
		t.Errorf("AssetRole = %q, want primary", got.AssetRole)
	}
	if got.EditionAttributes["published"] != "2021-05-01" {
		t.Errorf("EditionAttributes = %v", got.EditionAttributes)
	}

	// Roles keep working for a content type the package has never heard of.
	art := r.Identify("Podcasts/The Bike Shed/cover.jpg", podcast)
	if art.AssetRole != "artwork" {
		t.Errorf("companion AssetRole = %q, want artwork", art.AssetRole)
	}

	// And a path the new rules do not recognise still ingests.
	unknown := r.Identify("Podcasts/whatever.bin", podcast)
	if unknown.Identified || unknown.WorkKey != "unidentified" {
		t.Errorf("unrecognised path was identified: %#v", unknown)
	}
}

// TestPodcastRulesComposeWithTheBuiltIns: a new content type can be added to
// the default set without disturbing it.
func TestPodcastRulesComposeWithTheBuiltIns(t *testing.T) {
	r := identification.Default()
	r.Register(podcastRules()...)

	if got := r.Identify("Podcasts/The Bike Shed/2021-05-01 - Episode Title.mp3", podcast); got.ContentType != podcast {
		t.Errorf("ContentType = %q, want %q", got.ContentType, podcast)
	}
	if got := r.Identify("Movie Title (2019)/Movie Title (2019) - 2160p.mkv", identification.Movie); got.WorkKey != "movie:movie title:2019" {
		t.Errorf("registering podcast rules disturbed the movie rules: %#v", got)
	}
}
