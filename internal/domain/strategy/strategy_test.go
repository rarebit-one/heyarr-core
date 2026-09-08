package strategy

import (
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
)

// The load-bearing claim of ADR-0082: a document is TAKEN, not searched, and a
// video is searched. If these two ever agree on a route, the abstraction has
// collapsed back into the one pipeline it exists to split.
func TestDocumentIsDirectAndVideoIsSearched(t *testing.T) {
	if got := For(identification.Document).Route; got != acquisition.RouteDirect {
		t.Errorf("document route = %q, want %q — a captured article is taken from its "+
			"feed, never asked of an indexer", got, acquisition.RouteDirect)
	}
	if got := For(identification.Movie).Route; got != acquisition.RouteSearch {
		t.Errorf("movie route = %q, want %q", got, acquisition.RouteSearch)
	}
	if For(identification.Document).Route == For(identification.Movie).Route {
		t.Fatal("document and movie are on the same route; the per-type strategy has collapsed")
	}
}

// A podcast is direct too: its enclosure is the source of record, not an
// indexer's guess at one.
func TestPodcastIsDirect(t *testing.T) {
	if got := For(podcast).Route; got != acquisition.RouteDirect {
		t.Errorf("podcast route = %q, want %q", got, acquisition.RouteDirect)
	}
}

// An unknown content type — one the follow path minted that the table has not
// caught up with — is the conservative default: searched, and no assumed
// profile. Never RouteDirect, which would take random bytes from a feed as
// definitive.
func TestUnknownContentTypeIsSafelySearchable(t *testing.T) {
	s := For("something-new")
	if s.Route != acquisition.RouteSearch {
		t.Errorf("unknown type route = %q, want %q — the safe reading is ordinary "+
			"searchable content", s.Route, acquisition.RouteSearch)
	}
	if s.DefaultProfile != "" {
		t.Errorf("unknown type default profile = %q, want empty — nothing may be assumed",
			s.DefaultProfile)
	}
}

// Every DefaultProfile a strategy names must be a profile that actually seeds,
// or a follow inheriting it resolves to nothing. This is the test that catches a
// rename of a seeded profile that forgets the strategy table (or vice versa).
func TestEveryDefaultProfileIsSeeded(t *testing.T) {
	seeded := map[string]bool{}
	for _, p := range policy.Defaults() {
		seeded[p.Name] = true
	}
	for _, ct := range []string{
		identification.Movie, identification.Series, identification.Music,
		identification.Book, identification.Document, video, podcast,
	} {
		def := For(ct).DefaultProfile
		if def == "" {
			continue // a type with no default is allowed (music, book).
		}
		if !seeded[def] {
			t.Errorf("content type %q defaults to profile %q, which policy.Defaults() does "+
				"not seed", ct, def)
		}
	}
}

// The invariant ADR-0082 rests on: a default profile belongs to direct-route
// content and only to it. A video's quality bar is a real choice, so a want with
// no profile is refused (§56); a directly-taken document has no such choice, so
// `published` is the one sensible default. If a search-route type ever grows a
// default, this fails on purpose — that is a deliberate decision, not a drift.
func TestOnlyDirectRouteContentHasADefaultProfile(t *testing.T) {
	for _, ct := range []string{
		identification.Movie, identification.Series, identification.Music,
		identification.Book, identification.Document, video, podcast,
	} {
		s := For(ct)
		hasDefault := s.DefaultProfile != ""
		isDirect := s.Route == acquisition.RouteDirect
		if hasDefault != isDirect {
			t.Errorf("%q: has default profile = %v (%q), is direct route = %v — a default "+
				"belongs to direct-route content and only to it", ct, hasDefault, s.DefaultProfile, isDirect)
		}
	}
}
