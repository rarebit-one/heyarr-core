package followed

import (
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/desired"
)

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func validSource() Source {
	return Source{
		WorkID:           "series-1",
		Type:             TypeTVSeries,
		FeedRef:          "tvdb:12345",
		QualityProfileID: "living-room",
	}
}

func TestSourceValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Source)
		wantErr string
	}{
		{name: "a valid tv subscription", mutate: func(*Source) {}},
		{
			name:    "a work is required",
			mutate:  func(s *Source) { s.WorkID = "" },
			wantErr: "must name the work",
		},
		{
			name:    "a feed reference is required",
			mutate:  func(s *Source) { s.FeedRef = "" },
			wantErr: "feed reference",
		},
		{
			name:    "a quality profile is required",
			mutate:  func(s *Source) { s.QualityProfileID = "" },
			wantErr: "quality profile",
		},
		{
			name:    "an unknown type is refused",
			mutate:  func(s *Source) { s.Type = "torrent_tracker" },
			wantErr: "is not a source type",
		},
		{
			// Source-agnostic: the caller expresses intent, the system infers a
			// type — but a type with no adapter yet must fail loudly, not sit
			// unpolled. Generic RSS is still a later phase (Phase 4).
			name:    "an unimplemented type is refused",
			mutate:  func(s *Source) { s.Type = TypeRSSFeed },
			wantErr: "not implemented yet",
		},
		{
			// YouTube is Phase 3 — an implemented type validates like tv_series.
			name:   "an implemented youtube source validates",
			mutate: func(s *Source) { s.Type = TypeYouTubeChannel },
		},
		{
			// Podcast is Phase 2 — an implemented type validates like tv_series.
			name:   "an implemented podcast source validates",
			mutate: func(s *Source) { s.Type = TypePodcast },
		},
		{
			name:    "an oversized reason is refused",
			mutate:  func(s *Source) { s.Reason = strings.Repeat("x", maxReason+1) },
			wantErr: "past the limit",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validSource()
			tc.mutate(&s)
			err := s.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("expected valid, got: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected a refusal mentioning %q, got none", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("refusal should mention %q, said: %v", tc.wantErr, err)
			}
		})
	}
}

// Backfill defaults to from-now — following "from here" is what an operator
// usually means, and walking a thousand-video back-catalogue must be a
// deliberate choice.
func TestBackfillDefaultsToFromNow(t *testing.T) {
	s := validSource()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.Backfill != BackfillFromNow {
		t.Errorf("backfill defaulted to %q, want from_now", s.Backfill)
	}
}

func TestValidateTrims(t *testing.T) {
	s := Source{
		WorkID: "  w  ", Type: TypeTVSeries, FeedRef: "  tvdb:1  ",
		QualityProfileID: " p ", Reason: "  why  ",
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.WorkID != "w" || s.FeedRef != "tvdb:1" || s.QualityProfileID != "p" || s.Reason != "why" {
		t.Errorf("inputs not trimmed: %+v", s)
	}
}

func TestParseType(t *testing.T) {
	for _, ty := range Types() {
		if got, err := ParseType(string(ty)); err != nil || got != ty {
			t.Errorf("ParseType(%q) = (%v, %v)", ty, got, err)
		}
	}
	if _, err := ParseType("TV_SERIES"); err != nil {
		t.Errorf("type parsing should be case-insensitive: %v", err)
	}
	if _, err := ParseType("nonsense"); err == nil {
		t.Error("an unknown type must be refused")
	}
}

// TV (Phase 1), podcast (Phase 2) and YouTube (Phase 3) are wired; generic RSS
// is declared so its phase is an addition rather than a rename.
func TestImplementedTypes(t *testing.T) {
	for _, ty := range []Type{TypeTVSeries, TypePodcast, TypeYouTubeChannel} {
		if !ty.Implemented() {
			t.Errorf("%s is wired and must report implemented", ty)
		}
	}
	for _, ty := range []Type{TypeRSSFeed} {
		if ty.Implemented() {
			t.Errorf("%s is a later phase and must not claim to be implemented", ty)
		}
	}
}

func TestParseBackfill(t *testing.T) {
	for _, b := range Backfills() {
		if got, err := ParseBackfill(string(b)); err != nil || got != b {
			t.Errorf("ParseBackfill(%q) = (%v, %v)", b, got, err)
		}
	}
	if _, err := ParseBackfill("half"); err == nil {
		t.Error("an unknown backfill must be refused")
	}
}

func TestFeedItemValidate(t *testing.T) {
	valid := FeedItem{Key: "S02E05", Title: "The One With The Thing"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a keyed item is valid: %v", err)
	}
	empty := FeedItem{Title: "no key"}
	if err := empty.Validate(); err == nil || !strings.Contains(err.Error(), "source-stable key") {
		t.Fatalf("an item with no key must be refused, got %v", err)
	}
}

// The heart of following: a resolved item projects onto a valid item-scoped
// want that carries the subscription's policy. It must be a DesiredItem the
// existing pipeline accepts, with nothing marking it as source-projected.
func TestProjectWant(t *testing.T) {
	s := validSource()
	s.Monitor = true
	s.Reason = "Kate watches this"
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}

	want := s.ProjectWant("item-abc")

	if want.Scope != desired.ScopeItem {
		t.Errorf("scope = %q, want item", want.Scope)
	}
	if want.WorkID != "series-1" || want.ItemID != "item-abc" {
		t.Errorf("projected want targets (%s, %s)", want.WorkID, want.ItemID)
	}
	if want.QualityProfileID != "living-room" {
		t.Errorf("profile = %q, want inherited living-room", want.QualityProfileID)
	}
	if !want.Monitor {
		t.Error("monitoring is carried onto the projected want")
	}
	if want.Reason != "Kate watches this" {
		t.Errorf("reason = %q, want inherited", want.Reason)
	}

	// It must be a want the existing acquisition pipeline accepts — an
	// item-scoped want it would refuse is not a projection, it is a bug.
	if err := want.Validate(); err != nil {
		t.Fatalf("a projected want must validate against desired's own rules: %v", err)
	}

	kind, id := want.Target()
	if kind != "item" || id != "item-abc" {
		t.Errorf("Target() = (%s, %s), want (item, item-abc)", kind, id)
	}
}

// FeedItem carries a publish time and per-item attributes without a schema
// change per source type — the fields exist and hold what a feed knows.
func TestFeedItemCarriesMetadata(t *testing.T) {
	it := FeedItem{
		Key:         "S02E05",
		Title:       "an episode",
		PublishedAt: mustTime("2026-09-01T00:00:00Z"),
		Attributes:  map[string]string{"season": "2", "episode": "5"},
	}
	if err := it.Validate(); err != nil {
		t.Fatal(err)
	}
	if it.PublishedAt.IsZero() {
		t.Error("a supplied air date is kept")
	}
	if it.Attributes["episode"] != "5" {
		t.Error("per-item attributes are kept without a column per type")
	}
}

// The item id is trimmed, so a resolved id with stray whitespace does not
// produce a want that fails a foreign key at write time.
func TestProjectWantTrimsItemID(t *testing.T) {
	s := validSource()
	want := s.ProjectWant("  item-x  ")
	if want.ItemID != "item-x" {
		t.Errorf("item id = %q, want trimmed", want.ItemID)
	}
}
