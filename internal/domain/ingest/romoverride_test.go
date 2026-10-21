package ingest

import (
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
)

// An acquired disc image for a ROM want keeps its edition facts (ADR-0121): the
// transfer path has no platform directory, so the path alone leaves it
// unidentified, and the want's platform is what lets its name be read. The Work
// still comes from the want.
func TestAnAcquiredROMKeepsItsEditionFacts(t *testing.T) {
	w := WorkOverride{ContentType: identification.ROM, WorkKey: "rom:psx:final fantasy vii", Title: "Final Fantasy VII"}
	rel := "Final Fantasy VII (USA)/Final Fantasy VII (USA) (Disc 2).bin"

	guess := identification.Identify(rel, identification.ROM)
	if guess.ContentType == identification.ROM {
		t.Fatalf("the path alone identified the file (%+v); the test no longer exercises the gap", guess)
	}
	c := applyWorkOverride(withROMPlatform(guess, rel, w), w)
	if c.WorkKey != w.WorkKey || c.ContentType != identification.ROM {
		t.Errorf("work = %s %q, want the want's", c.ContentType, c.WorkKey)
	}
	if c.EditionKey != "psx:usa" || c.EditionType != "disc-image" {
		t.Errorf("edition = %q type %q, want psx:usa disc-image", c.EditionKey, c.EditionType)
	}
	if c.EditionAttributes["region"] != "USA" || c.EditionAttributes["disc"] != "2" {
		t.Errorf("edition attributes = %v, want region USA disc 2", c.EditionAttributes)
	}
	if c.Source != identification.SourceDesiredItem {
		t.Errorf("source = %q, want the want's", c.Source)
	}
}

// Any other want, and a ROM the path already identified, pass through as they
// were.
func TestWithROMPlatformLeavesOtherWantsAlone(t *testing.T) {
	movie := identification.Identify("Arrival (2016)/Arrival.2016.1080p.mkv", identification.Movie)
	if got := withROMPlatform(movie, "Arrival (2016)/Arrival.2016.1080p.mkv",
		WorkOverride{ContentType: identification.Movie, WorkKey: "movie:arrival:2016"}); got.EditionKey != movie.EditionKey {
		t.Errorf("a movie want's candidate changed: %+v", got)
	}
	scanned := identification.Identify("snes/Super Mario World (USA).sfc", identification.ROM)
	got := withROMPlatform(scanned, "snes/Super Mario World (USA).sfc",
		WorkOverride{ContentType: identification.ROM, WorkKey: "rom:snes:super mario world"})
	if got.Rule != scanned.Rule {
		t.Errorf("an already-identified ROM was re-read: rule %q, was %q", got.Rule, scanned.Rule)
	}
}

// A path that identifies another platform's ROM is re-read with the wanted
// platform, so no PSX edition lands on a SNES Work.
func TestWithROMPlatformReReadsAContradictingPlatform(t *testing.T) {
	rel := "Release/psx/Some Game (USA).bin"
	guess := identification.Identify("psx/Some Game (USA).bin", identification.ROM)
	if guess.ContentType != identification.ROM || guess.WorkKey != "rom:psx:some game" {
		t.Fatalf("fixture: %+v", guess)
	}
	w := WorkOverride{ContentType: identification.ROM, WorkKey: "rom:snes:some game"}
	got := withROMPlatform(guess, rel, w)
	if got.EditionAttributes["platform"] != "snes" || got.EditionKey != "snes:usa" {
		t.Errorf("edition %q attrs %v, want the SNES reading", got.EditionKey, got.EditionAttributes)
	}
}
