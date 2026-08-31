package followed

import (
	"testing"
	"time"
)

// The poll cadence is conservative and bounded: a floor at the base, a ceiling
// at the max, and a backoff between for a source that keeps emitting nothing —
// reused from acquisition.Schedule so the determinism is tested once.
func TestFeedPollCadence(t *testing.T) {
	s := FeedPoll()
	if s.Name != "feed-poll" {
		t.Errorf("name = %q", s.Name)
	}
	if s.Base != 6*time.Hour {
		t.Errorf("base = %s, want 6h", s.Base)
	}
	if s.Max != 24*time.Hour {
		t.Errorf("max = %s, want 24h", s.Max)
	}

	// A fresh source (no fruitless polls) waits about the base; a long-quiet one
	// is capped at the max. Never faster than the floor, never abandoned.
	if d := s.Delay(0); d != s.Base {
		t.Errorf("delay(0) = %s, want the base %s", d, s.Base)
	}
	if d := s.Delay(100); d != s.Max {
		t.Errorf("delay(100) = %s, want the max %s", d, s.Max)
	}
}

// The spread is deterministic and keyed on the source id: two sources on the
// same host land in different slots, and the same source always lands in the
// same slot ("why did this poll at 03:14" has an answer, ADR-0017).
func TestFeedPollSpreadIsDeterministicPerSource(t *testing.T) {
	s := FeedPoll()
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	a1 := s.NextAt(now, 0, "source-a")
	a2 := s.NextAt(now, 0, "source-a")
	if !a1.Equal(a2) {
		t.Errorf("the same source moved: %s then %s", a1, a2)
	}

	b := s.NextAt(now, 0, "source-b")
	if a1.Equal(b) {
		t.Error("two sources landed in the same slot — the spread is not keying on the id")
	}

	// The spread never pushes past base + a quarter of the delay.
	if a1.Before(now.Add(s.Base)) || a1.After(now.Add(s.Base+s.Base/4)) {
		t.Errorf("next poll %s is outside [base, base+base/4] from %s", a1, now)
	}
}

func TestPollDedupeKey(t *testing.T) {
	if got := PollDedupeKey("src-1"); got != "poll_source:src-1" {
		t.Errorf("dedupe key = %q", got)
	}
	if PollDedupeKey("a") == PollDedupeKey("b") {
		t.Error("two sources must have distinct dedupe keys")
	}
	if PollSourceJobType != "poll_source" {
		t.Errorf("job type = %q", PollSourceJobType)
	}
}
