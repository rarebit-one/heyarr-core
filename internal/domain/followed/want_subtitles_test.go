package followed

import (
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/desired"
)

// A followed source's wanted subtitle languages (ADR-0085 §6): normalised on
// Validate, and projected as one item-scoped subtitle want per language.

func validSubSource() Source {
	return Source{
		WorkID:           "w1",
		Type:             TypeTVSeries,
		FeedRef:          "12345",
		QualityProfileID: "q1",
	}
}

func TestValidateNormalisesWantSubtitles(t *testing.T) {
	s := validSubSource()
	s.WantSubtitles = []string{"EN", " en ", "", "de", "EN"}
	if err := s.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(s.WantSubtitles) != 2 || s.WantSubtitles[0] != "en" || s.WantSubtitles[1] != "de" {
		t.Fatalf("want_subtitles = %v, want [en de]", s.WantSubtitles)
	}
}

func TestValidateEmptyWantSubtitlesIsNil(t *testing.T) {
	s := validSubSource()
	s.WantSubtitles = []string{"", "  "}
	if err := s.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(s.WantSubtitles) != 0 {
		t.Errorf("want_subtitles = %v, want empty", s.WantSubtitles)
	}
}

func TestProjectSubtitleWants(t *testing.T) {
	s := validSubSource()
	s.WantSubtitles = []string{"en", "de"}

	wants := s.ProjectSubtitleWants("it-1", "q-sub")
	if len(wants) != 2 {
		t.Fatalf("got %d subtitle wants, want 2", len(wants))
	}
	for i, lang := range []string{"en", "de"} {
		w := wants[i]
		if w.Aspect != desired.AspectSubtitle || w.Language != lang {
			t.Errorf("want[%d] aspect/lang = %s/%s, want subtitle/%s", i, w.Aspect, w.Language, lang)
		}
		if w.Scope != desired.ScopeItem || w.ItemID != "it-1" || w.WorkID != "w1" {
			t.Errorf("want[%d] target = %s/%s/%s", i, w.Scope, w.WorkID, w.ItemID)
		}
		if w.QualityProfileID != "q-sub" {
			t.Errorf("want[%d] profile = %s, want q-sub (the subtitle profile, not the video one)", i, w.QualityProfileID)
		}
		if w.Monitor {
			t.Errorf("want[%d] is monitored; a subtitle is terminal", i)
		}
		// Every projected want must be one the domain accepts.
		if err := w.Validate(); err != nil {
			t.Errorf("want[%d] does not validate: %v", i, err)
		}
	}
}

func TestProjectSubtitleWantsEmptyWhenNoneOrNoProfile(t *testing.T) {
	s := validSubSource()
	if got := s.ProjectSubtitleWants("it-1", "q-sub"); got != nil {
		t.Errorf("no languages should project no wants, got %v", got)
	}
	s.WantSubtitles = []string{"en"}
	if got := s.ProjectSubtitleWants("it-1", ""); got != nil {
		t.Errorf("no subtitle profile should project no wants, got %v", got)
	}
}
