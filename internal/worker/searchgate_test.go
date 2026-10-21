package worker

import (
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
)

// The containment gate as the search job runs it, over real release names
// (ADR-0026): an S01E01 want keeps the exact episode and a season-one pack, and
// rejects a different season and a different episode — each rejection carrying a
// durable, machine-coded reason rather than being dropped (ADR-0093 §2).
func TestGateSeasonEpisode(t *testing.T) {
	t.Parallel()
	candidates := []acquisition.ReleaseCandidate{
		{ID: "ep", Title: "Slow.Horses.S01E01.1080p.WEB-DL.x265-GRP"},
		{ID: "pack", Title: "Slow.Horses.S01.COMPLETE.1080p.WEB-DL.x265-GRP"},
		{ID: "wrong-season", Title: "Slow.Horses.S03.2160p.WEB-DL.x265-GRP"},
		{ID: "wrong-episode", Title: "Slow.Horses.S02E01.1080p.WEB-DL.x264-GRP"},
		{ID: "no-marker", Title: "Slow.Horses.1080p.WEB-DL.x265-GRP"},
	}

	survivors, rejected := gateSeasonEpisode(candidates, 1, 1)

	kept := map[string]bool{}
	for _, s := range survivors {
		kept[s.ID] = true
	}
	if !kept["ep"] || !kept["pack"] {
		t.Errorf("the exact episode and the season pack must survive, kept = %v", kept)
	}
	if kept["wrong-season"] || kept["wrong-episode"] || kept["no-marker"] {
		t.Errorf("a wrong season, wrong episode or unmarked release must not survive, kept = %v", kept)
	}

	// Every rejection is reported with a match reason, never silently dropped.
	if len(rejected) != 3 {
		t.Fatalf("want 3 rejected candidates, got %d", len(rejected))
	}
	for _, r := range rejected {
		if r.Evaluation.Accepted {
			t.Errorf("%s was rejected but marked accepted", r.Candidate.ID)
		}
		reasons := r.Evaluation.RejectedBy()
		if r.Candidate.ID == "no-marker" {
			// Undetermined is not a ResultFail, so it is not in RejectedBy — the
			// same treatment Evaluate gives an undetermined accept gate — but it
			// is still a reason in the evaluation.
			if len(r.Evaluation.Reasons) == 0 {
				t.Error("an undetermined rejection still needs a reason")
			}
			continue
		}
		if len(reasons) != 1 || reasons[0].Section != acquisition.SectionMatch {
			t.Errorf("%s should carry one match rejection reason, got %+v", r.Candidate.ID, reasons)
		}
	}
}

// A season pack for the WRONG season must never be kept, whatever season it is —
// the invariant the observed bug violated.
func TestGateSeasonEpisodeNeverKeepsAWrongSeasonPack(t *testing.T) {
	t.Parallel()
	for season := 0; season <= 9; season++ {
		if season == 1 {
			continue
		}
		title := "Slow.Horses.S0" + string(rune('0'+season)) + ".2160p.WEB-DL.x265-GRP"
		survivors, _ := gateSeasonEpisode([]acquisition.ReleaseCandidate{{ID: "pack", Title: title}}, 1, 1)
		if len(survivors) != 0 {
			t.Errorf("kept %q for an S01E01 want", title)
		}
	}
}

// The ROM platform gate as the search job runs it (ADR-0121 §4): a SNES want
// keeps the SNES release and reports, with a coded reason, the Game Boy one, the
// one that names no platform, and the multi-platform bundle — never drops them.
func TestGateROMPlatform(t *testing.T) {
	in := []acquisition.ReleaseCandidate{
		{ID: "snes", Title: "Tetris Attack (USA) [SNES]"},
		{ID: "gb", Title: "Tetris Attack (USA) [GB]"},
		{ID: "bare", Title: "Tetris Attack (USA)"},
		{ID: "bundle", Title: "Tetris Attack SNES + GB"},
	}
	survivors, rejected := gateROMPlatform(in, "snes", "Tetris Attack")
	if len(survivors) != 1 || survivors[0].ID != "snes" {
		t.Fatalf("survivors %+v, want only the SNES release", survivors)
	}
	want := map[string]string{
		"gb":     acquisition.RulePlatformMatches,
		"bare":   acquisition.RulePlatformUndetermined,
		"bundle": acquisition.RulePlatformUndetermined,
	}
	if len(rejected) != len(want) {
		t.Fatalf("%d rejected, want %d", len(rejected), len(want))
	}
	for _, r := range rejected {
		if r.Evaluation.Accepted || len(r.Evaluation.Reasons) != 1 || r.Evaluation.Reasons[0].Rule != want[r.Candidate.ID] {
			t.Errorf("%s: %+v, want one %q reason, not accepted", r.Candidate.ID, r.Evaluation, want[r.Candidate.ID])
		}
	}
}

// A game named after a platform is not rejected by its own title: "Switch" on
// the PSX reads as a PSX release, not as Switch-and-PSX (ADR-0121 §4).
func TestGateROMPlatformIgnoresTheWantedTitle(t *testing.T) {
	in := []acquisition.ReleaseCandidate{
		{ID: "psx", Title: "Switch (USA) [PSX]"},
		{ID: "bare", Title: "Switch (USA)"},
	}
	survivors, rejected := gateROMPlatform(in, "psx", "Switch")
	if len(survivors) != 1 || survivors[0].ID != "psx" {
		t.Fatalf("survivors %+v, want the PSX release", survivors)
	}
	if len(rejected) != 1 || rejected[0].Evaluation.Reasons[0].Rule != acquisition.RulePlatformUndetermined {
		t.Errorf("rejected %+v, want the untagged release as undetermined (its title is not evidence)", rejected)
	}
}
