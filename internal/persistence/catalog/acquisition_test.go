package catalog_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// Acquisition state, against a real database (§64, M3-03).
//
// The property these exist for is invariant 7: the event and the row commit
// together, always. An event log with gaps is not a log, it is a sample — and
// that is expensive to retrofit and cheap to hold now.

const stamp = "2026-08-01T00:00:00Z"

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

type harness struct {
	db     *sqlite.DB
	cat    *catalog.Catalog
	events *events.Log
	want   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}

	clock := fixedClock{t: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)}
	eventLog, err := events.New(events.Options{
		Writer: db.Writer(), Reader: db.Reader(), Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{
		DB: db, Events: eventLog, PeerName: "test", PeerSite: "test-site", Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}

	h := &harness{db: db, cat: cat, events: eventLog, want: "want-1"}
	h.exec(t, `INSERT INTO quality_profiles
		(id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES ('q1', 'living-room', '', '[]', '[]', '[]', 1, ?, ?)`, stamp, stamp)
	h.exec(t, `INSERT INTO works
		(id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES ('w1', 'movie', 'movie:arrival:2016', 'Arrival', 'arrival', 2016, '{}', ?, ?)`,
		stamp, stamp)
	h.exec(t, `INSERT INTO desired_items
		(id, scope, work_id, edition_id, quality_profile_id, monitor, reason, created_at, updated_at)
		VALUES (?, 'work', 'w1', NULL, 'q1', 1, '', ?, ?)`, h.want, stamp, stamp)
	return h
}

func (h *harness) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := h.db.Writer().Exec(query, args...); err != nil {
		t.Fatalf("seeding (%s): %v", query, err)
	}
}

func (h *harness) eventCount(t *testing.T) int {
	t.Helper()
	evs, err := h.events.Since(context.Background(), 0, nil, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(evs)
}

func (h *harness) rowCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.db.Reader().QueryRow(`SELECT count(*) FROM acquisition_state`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStartIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	rec, err := h.cat.StartAcquisition(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State.Name() != "MISSING" {
		t.Fatalf("a fresh acquisition is MISSING, got %s", rec.State.Name())
	}
	afterFirst := h.eventCount(t)
	if afterFirst == 0 {
		t.Fatal("creating acquisition state is a transition and must emit (invariant 7)")
	}

	// The job that calls this WILL be re-run (invariant 9), and re-running it
	// must not reset a want halfway through a download — nor emit again, or
	// every reconciliation pass would put a row in the log.
	if _, err := h.cat.AdvanceAcquisition(ctx, h.want, acquisition.TransitionSearch, ""); err != nil {
		t.Fatal(err)
	}
	beforeRestart := h.eventCount(t)

	for range 3 {
		again, err := h.cat.StartAcquisition(ctx, h.want)
		if err != nil {
			t.Fatal(err)
		}
		if again.State.Phase != acquisition.PhaseSearching {
			t.Fatalf("re-starting reset an in-flight acquisition to %s", again.State.Phase)
		}
	}
	if got := h.rowCount(t); got != 1 {
		t.Errorf("re-starting created %d rows", got)
	}
	if got := h.eventCount(t); got != beforeRestart {
		t.Errorf("re-starting emitted %d event(s); it changed nothing", got-beforeRestart)
	}
}

// Every legal edge writes a row AND an event, asserted by walking the whole
// happy path rather than by a hand-written list that drifts.
func TestEveryAdvanceEmits(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}

	path := []acquisition.Transition{
		acquisition.TransitionSearch,
		acquisition.TransitionCandidatesFound,
		acquisition.TransitionSelect,
		acquisition.TransitionQueue,
		acquisition.TransitionStartDownload,
		acquisition.TransitionDownloaded,
		acquisition.TransitionVerified,
		acquisition.TransitionIngested,
	}
	for _, tr := range path {
		before := h.eventCount(t)
		rec, err := h.cat.AdvanceAcquisition(ctx, h.want, tr, "")
		if err != nil {
			t.Fatalf("%s: %v", tr, err)
		}
		if got := h.eventCount(t); got != before+1 {
			t.Fatalf("%s emitted %d event(s), want exactly 1", tr, got-before)
		}
		// The row and the domain agree.
		stored, err := h.cat.Acquisition(ctx, h.want)
		if err != nil {
			t.Fatal(err)
		}
		if stored.State != rec.State {
			t.Fatalf("%s: returned %+v, stored %+v", tr, rec.State, stored.State)
		}
	}

	final, err := h.cat.Acquisition(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if final.State.Name() != "AVAILABLE" {
		t.Errorf("the happy path ends AVAILABLE (nothing has evaluated the bytes), got %s",
			final.State.Name())
	}
}

// An illegal transition changes nothing and emits nothing. A machine that
// half-applies and then reports an error is worse than one that refuses.
func TestAnIllegalAdvanceWritesNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	before := h.eventCount(t)

	_, err := h.cat.AdvanceAcquisition(ctx, h.want, acquisition.TransitionIngested, "")
	if err == nil {
		t.Fatal("an idle acquisition cannot be ingested")
	}
	if !errors.Is(err, acquisition.ErrIllegalTransition) {
		t.Errorf("the API turns ErrIllegalTransition into a 409 and everything else into "+
			"a 500, so this must be the former: %v", err)
	}
	if got := h.eventCount(t); got != before {
		t.Errorf("a refused transition emitted %d event(s)", got-before)
	}
	stored, err := h.cat.Acquisition(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State.Phase != acquisition.PhaseIdle {
		t.Errorf("a refused transition moved the row to %s", stored.State.Phase)
	}
}

// Satisfaction moves on a different schedule from the pipeline, and a
// reconciliation pass that changes nothing must emit nothing — otherwise a
// timer over the whole library turns the event log into a heartbeat.
func TestSatisfactionEmitsOnlyOnChange(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	// Get to AVAILABLE so content may be satisfied.
	for _, tr := range []acquisition.Transition{
		acquisition.TransitionSearch, acquisition.TransitionCandidatesFound,
		acquisition.TransitionSelect, acquisition.TransitionQueue,
		acquisition.TransitionStartDownload, acquisition.TransitionDownloaded,
		acquisition.TransitionVerified, acquisition.TransitionIngested,
	} {
		if _, err := h.cat.AdvanceAcquisition(ctx, h.want, tr, ""); err != nil {
			t.Fatal(err)
		}
	}

	before := h.eventCount(t)
	rec, err := h.cat.SetSatisfaction(ctx, h.want,
		acquisition.SatisfactionSatisfied, acquisition.SatisfactionConverging)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State.Name() != "PLACEMENT_CONVERGING" {
		t.Fatalf("Name() = %s, want PLACEMENT_CONVERGING", rec.State.Name())
	}
	if got := h.eventCount(t); got != before+1 {
		t.Fatalf("a satisfaction change should emit once, got %d", got-before)
	}

	afterChange := h.eventCount(t)
	for range 5 {
		if _, err := h.cat.SetSatisfaction(ctx, h.want,
			acquisition.SatisfactionSatisfied, acquisition.SatisfactionConverging); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.eventCount(t); got != afterChange {
		t.Errorf("five no-op reconciliation passes emitted %d event(s); a sweep over a "+
			"steady library must be silent", got-afterChange)
	}
}

// phase_entered_at is what makes "stuck in downloading for a week" findable.
// Reconciliation touching the axes must not reset it, or a want stuck since
// Tuesday looks like it moved five minutes ago.
func TestReconciliationDoesNotResetThePhaseClock(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	for _, tr := range []acquisition.Transition{
		acquisition.TransitionSearch, acquisition.TransitionCandidatesFound,
		acquisition.TransitionSelect, acquisition.TransitionQueue,
		acquisition.TransitionStartDownload, acquisition.TransitionDownloaded,
		acquisition.TransitionVerified, acquisition.TransitionIngested,
	} {
		if _, err := h.cat.AdvanceAcquisition(ctx, h.want, tr, ""); err != nil {
			t.Fatal(err)
		}
	}
	// Backdate the phase clock to simulate a want that has been idle a while.
	h.exec(t, `UPDATE acquisition_state SET phase_entered_at = '2026-07-01T00:00:00Z'
	            WHERE desired_item_id = ?`, h.want)

	if _, err := h.cat.SetSatisfaction(ctx, h.want,
		acquisition.SatisfactionSatisfied, acquisition.SatisfactionSatisfied); err != nil {
		t.Fatal(err)
	}

	var entered string
	if err := h.db.Reader().QueryRow(
		`SELECT phase_entered_at FROM acquisition_state WHERE desired_item_id = ?`, h.want,
	).Scan(&entered); err != nil {
		t.Fatal(err)
	}
	if entered != "2026-07-01T00:00:00Z" {
		t.Errorf("reconciliation reset the phase clock to %q; a want stuck since July "+
			"would look like it moved just now", entered)
	}
}

// A want with no acquisition row is a real state a caller has to handle, and it
// must be a typed error rather than a bare sql.ErrNoRows leaking out.
func TestReadingAnAbsentAcquisitionIsTyped(t *testing.T) {
	h := newHarness(t)
	if _, err := h.cat.Acquisition(context.Background(), "no-such-want"); !errors.Is(err, catalog.ErrNoAcquisition) {
		t.Errorf("expected ErrNoAcquisition, got %v", err)
	}
}
