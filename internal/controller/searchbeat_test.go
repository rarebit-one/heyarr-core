package controller

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// The search beat (#130) against a real database.
//
// Everything here uses an INJECTED CLOCK. A backoff asserted with time.Sleep is
// a test that is slow when it passes and flaky when it does not, and the house
// rule (ADR-0017) is that time is a parameter.

const beatStamp = "2026-08-01T00:00:00Z"

type movableClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *movableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *movableClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *movableClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

type beatHarness struct {
	db    *sqlite.DB
	cat   *catalog.Catalog
	queue *jobs.Queue
	clock *movableClock
}

func newBeatHarness(t *testing.T) *beatHarness {
	t.Helper()
	ctx := t.Context()
	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{
		DB: db, Events: eventLog, PeerName: "test", PeerSite: "test-site",
	})
	if err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(jobs.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		t.Fatal(err)
	}

	h := &beatHarness{
		db: db, cat: cat, queue: queue,
		clock: &movableClock{t: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
	}
	h.exec(t, `INSERT INTO quality_profiles
		(id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES ('q1', 'living-room', '', '[]', '[]', '[]', 1, ?, ?)`, beatStamp, beatStamp)
	return h
}

func (h *beatHarness) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := h.db.Writer().Exec(query, args...); err != nil {
		t.Fatalf("seeding (%s): %v", query, err)
	}
}

// want creates a DesiredItem resting in MISSING, exactly as the API does.
func (h *beatHarness) want(t *testing.T, id string, monitored bool) string {
	t.Helper()
	monitor := 0
	if monitored {
		monitor = 1
	}
	// A work of its own per want: desired_items is unique over (target,
	// profile), which is §61's "not one version per title" showing up in the
	// schema.
	work := "w-" + id
	h.exec(t, `INSERT INTO works
		(id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES (?, 'movie', ?, 'Arrival', 'arrival', 2016, '{}', ?, ?)`,
		work, "movie:"+id+":2016", beatStamp, beatStamp)
	h.exec(t, `INSERT INTO desired_items
		(id, scope, work_id, edition_id, quality_profile_id, monitor, reason, created_at, updated_at)
		VALUES (?, 'work', ?, NULL, 'q1', ?, '', ?, ?)`, id, work, monitor, beatStamp, beatStamp)
	if _, err := h.cat.StartAcquisition(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	return id
}

// satisfy puts a want in CONTENT_SATISFIED without going the long way round.
// The state machine's own tests cover the transitions; what this file is about
// is which SCHEDULE a resting want lands on.
func (h *beatHarness) satisfy(t *testing.T, id string) {
	t.Helper()
	h.exec(t, `UPDATE acquisition_state
		SET managed = 1, content = 'satisfied', placement = 'satisfied'
		WHERE desired_item_id = ?`, id)
}

func (h *beatHarness) recordHealth(t *testing.T, name, capabilities string, healthy bool) {
	t.Helper()
	flag := 0
	if healthy {
		flag = 1
	}
	h.exec(t, `INSERT INTO provider_health
		(name, capabilities, healthy, detail, version, checked_at, created_at, updated_at)
		VALUES (?, ?, ?, '', '', ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET healthy = excluded.healthy`,
		name, capabilities, flag, beatStamp, beatStamp, beatStamp)
}

// searchJobs counts every search_release job in the table, in ANY state.
//
// Any state, deliberately: a dedupe key that only prevented a second PENDING
// job would still allow the pile-up this exists to prevent.
func (h *beatHarness) searchJobs(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.db.Reader().QueryRow(
		`SELECT count(*) FROM jobs WHERE type = ?`, acquisition.SearchJobType).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (h *beatHarness) scheduler() *searchScheduler {
	return newSearchScheduler(h.cat, h.queue, h.clock, discard())
}

func (h *beatHarness) schedule(t *testing.T, id string) catalog.SearchScheduleRow {
	t.Helper()
	row, ok, err := h.cat.SearchSchedule(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("%s has no search schedule row", id)
	}
	return row
}

// THE BUG, and the fix. A MISSING want with no search in flight was
// indistinguishable from one being worked on, because nothing was ever going
// to look at it.
func TestAMissingWantGetsASearchWithoutBeingAsked(t *testing.T) {
	h := newBeatHarness(t)
	id := h.want(t, "want-1", true)

	enqueued, err := h.scheduler().pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Asserted explicitly: a scheduler that enqueues NOTHING would pass a
	// naive version of the "no second search" test below by doing nothing at
	// all, twice.
	if enqueued != 1 {
		t.Fatalf("the first pass enqueued %d searches, want exactly 1", enqueued)
	}
	if got := h.searchJobs(t); got != 1 {
		t.Fatalf("there are %d search jobs, want exactly 1", got)
	}

	row := h.schedule(t, id)
	if row.Schedule != "missing" {
		t.Errorf("schedule = %q, want %q", row.Schedule, "missing")
	}
	if row.Fruitless != 0 {
		t.Errorf("fruitless = %d on a want's first ever search, want 0", row.Fruitless)
	}
	if !row.NextSearchAt.After(h.clock.Now()) {
		t.Errorf("next_search_at %s is not in the future; the want would be searched "+
			"again on the very next tick", row.NextSearchAt)
	}
}

// Invariant 9's local form, and the one the dedupe key exists for.
//
// The clock is moved PAST the want's next due date before the second pass, so
// the want is genuinely due again and the only thing preventing a second
// search is the dedupe key. A version of this test that left the clock alone
// would pass with the dedupe key deleted, which is exactly the weak test the
// sabotage step looks for.
func TestASecondPassDoesNotProduceASecondSearch(t *testing.T) {
	h := newBeatHarness(t)
	id := h.want(t, "want-1", true)
	s := h.scheduler()

	enqueued, err := s.pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if enqueued != 1 {
		t.Fatalf("the first pass enqueued %d searches, want exactly 1", enqueued)
	}

	// Well past due: the schedule is no longer what is holding this back.
	h.clock.set(h.schedule(t, id).NextSearchAt.Add(time.Hour))

	if _, err := s.pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.searchJobs(t); got != 1 {
		t.Fatalf("a second pass over a want whose search is still queued produced %d jobs, "+
			"want exactly 1 — two searches for one want is what the dedupe key prevents "+
			"(invariant 9, ADR-0008)", got)
	}
}

// Invariant 9 across ROLES. Two schedulers, two goroutines, one database, one
// want — and exactly one search.
//
// Both roles PLAN first, so both have decided the same want is due before
// either writes. That is the state the dedupe key exists for, and it is not the
// state two whole concurrent passes would reliably reach: the control plane is
// single-writer (ADR-0003), so the second pass's read would usually land after
// the first pass's write, find nothing due, and the test would be asserting
// that SQLite serialises writers. Removing the dedupe key must fail THIS test,
// and it is only guaranteed to if both roles get as far as enqueueing.
func TestTwoSchedulersProduceOneSearch(t *testing.T) {
	h := newBeatHarness(t)
	h.want(t, "want-1", true)

	first, second := h.scheduler(), h.scheduler()
	plans := make([]struct {
		s   *searchScheduler
		now time.Time
		due []catalog.DueSearch
	}, 0, 2)
	for _, s := range []*searchScheduler{first, second} {
		now, due, err := s.plan(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(due) != 1 {
			t.Fatalf("a role planned %d searches, want 1 — both roles must reach the "+
				"enqueue for this to be a test of the dedupe key", len(due))
		}
		plans = append(plans, struct {
			s   *searchScheduler
			now time.Time
			due []catalog.DueSearch
		}{s, now, due})
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	errs := make([]error, 0, 2)
	start := make(chan struct{})
	for _, p := range plans {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			n, err := p.s.dispatch(t.Context(), p.now, p.due)
			mu.Lock()
			defer mu.Unlock()
			total += n
			if err != nil {
				errs = append(errs, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		t.Fatal(err)
	}

	if got := h.searchJobs(t); got != 1 {
		t.Fatalf("two roles scheduling concurrently produced %d searches for one want, "+
			"want exactly 1 (invariant 9)", got)
	}
	// Both roles legitimately report having ensured a search exists — Enqueue
	// returns the live job rather than an error, because the caller asked for
	// work to happen and it is going to happen. What must not double is the
	// BOOKKEEPING: a streak advanced twice would double the interval twice.
	if total != 2 {
		t.Fatalf("%d of the two roles enqueued; both must, or the dedupe key was never "+
			"exercised", total)
	}
	row := h.schedule(t, "want-1")
	if row.Fruitless != 0 {
		t.Errorf("fruitless = %d after two concurrent first passes, want 0 — the "+
			"compare-and-set let both advance the streak", row.Fruitless)
	}
}

// Decision 2: across calls, not within one. A release that does not exist yet
// is the common case, and asking hourly forever is how a well-behaved system
// becomes a hammering one.
func TestAWantThatKeepsFindingNothingIsSearchedLessOften(t *testing.T) {
	h := newBeatHarness(t)
	id := h.want(t, "want-1", true)
	s := h.scheduler()

	var intervals []time.Duration
	for range 8 {
		if _, err := s.pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		row := h.schedule(t, id)
		intervals = append(intervals, row.NextSearchAt.Sub(row.LastSearchedAt))
		// The want is never satisfied and never moves; every pass is another
		// fruitless one.
		h.clock.set(row.NextSearchAt)
	}

	missing := acquisition.MissingSearches()
	for i := 1; i < len(intervals); i++ {
		if intervals[i] < intervals[i-1] {
			t.Fatalf("interval %d (%s) is shorter than interval %d (%s); the backoff went "+
				"backwards", i, intervals[i], i-1, intervals[i-1])
		}
	}
	if intervals[0] >= intervals[1] {
		t.Errorf("the second interval %s did not grow from the first %s", intervals[1], intervals[0])
	}
	last := intervals[len(intervals)-1]
	if last < missing.Max {
		t.Errorf("after eight fruitless searches the interval is only %s; it should have "+
			"reached the %s ceiling", last, missing.Max)
	}
	// And it stops there rather than growing without bound: a want is never
	// abandoned.
	if last > missing.Max+missing.Max/4 {
		t.Errorf("the interval %s overshot the %s ceiling by more than the spread",
			last, missing.Max)
	}
}

// Decision 3, asserted as TWO SCHEDULES rather than as one interval with a
// multiplier: the two wants land on differently NAMED schedules whose actual
// intervals differ by an order of magnitude.
func TestTheUpgradeCadenceIsSlowerThanTheMissingCadence(t *testing.T) {
	h := newBeatHarness(t)
	missingID := h.want(t, "want-missing", true)
	upgradeID := h.want(t, "want-upgrade", true)
	h.satisfy(t, upgradeID)

	if _, err := h.scheduler().pass(t.Context()); err != nil {
		t.Fatal(err)
	}

	missingRow := h.schedule(t, missingID)
	upgradeRow := h.schedule(t, upgradeID)

	// assert_eq on the enum-like value, not a substring.
	if missingRow.Schedule != "missing" {
		t.Errorf("the unsatisfied want is on schedule %q, want %q", missingRow.Schedule, "missing")
	}
	if upgradeRow.Schedule != "upgrade" {
		t.Errorf("the satisfied want is on schedule %q, want %q", upgradeRow.Schedule, "upgrade")
	}

	missingWait := missingRow.NextSearchAt.Sub(missingRow.LastSearchedAt)
	upgradeWait := upgradeRow.NextSearchAt.Sub(upgradeRow.LastSearchedAt)
	if upgradeWait <= missingWait {
		t.Fatalf("the upgrade wait %s is not longer than the missing wait %s — the two "+
			"schedules have collapsed into one", upgradeWait, missingWait)
	}
	// The load-bearing one. The check above can be satisfied by a few seconds
	// of per-want spread, so on its own it would survive the two schedules
	// being collapsed into one — which is exactly the collapse this test
	// exists to refuse.
	if upgradeWait < 10*missingWait {
		t.Fatalf("the upgrade wait %s is less than ten times the missing wait %s; that is "+
			"one schedule with a knob, not two schedules", upgradeWait, missingWait)
	}
}

// §60's distinction between wanting and monitoring, at the scheduler.
func TestASatisfiedUnmonitoredWantIsNeverSearched(t *testing.T) {
	h := newBeatHarness(t)
	id := h.want(t, "want-1", false)
	h.satisfy(t, id)

	enqueued, err := h.scheduler().pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if enqueued != 0 {
		t.Fatalf("scheduled %d searches for a finished want; the operator said "+
			"'get me this', not 'keep improving this' (§60)", enqueued)
	}
	if got := h.searchJobs(t); got != 0 {
		t.Fatalf("there are %d search jobs for a finished want, want 0", got)
	}
}

// A want with something in flight is not idle, and searching it would race the
// acquisition it already started.
func TestAWantWithASearchInFlightIsNotScheduled(t *testing.T) {
	h := newBeatHarness(t)
	id := h.want(t, "want-1", true)
	if _, err := h.cat.AdvanceAcquisition(t.Context(), id, acquisition.TransitionSearch, ""); err != nil {
		t.Fatal(err)
	}

	enqueued, err := h.scheduler().pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if enqueued != 0 {
		t.Fatalf("scheduled %d searches for a want that is already searching", enqueued)
	}
}

// Decision 5, in the direction chosen: hold off, and do not penalise the want.
func TestEveryIndexerUnhealthyHoldsOffAndResumes(t *testing.T) {
	h := newBeatHarness(t)
	id := h.want(t, "want-1", true)
	h.recordHealth(t, "prowlarr", `["indexer"]`, false)
	s := h.scheduler()

	enqueued, err := s.pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if enqueued != 0 {
		t.Fatalf("scheduled %d searches while every indexer was unhealthy; each one would "+
			"drive a want through SEARCHING and back on a failure, five times, before dying",
			enqueued)
	}
	if got := h.searchJobs(t); got != 0 {
		t.Fatalf("there are %d search jobs, want 0", got)
	}
	// Nothing was penalised for somebody else's outage: an indexer being down
	// is not evidence about any particular want.
	if _, ok, err := h.cat.SearchSchedule(t.Context(), id); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Error("the outage advanced a want's schedule; backoff is for a release that does " +
			"not exist, not for an indexer that is down")
	}

	// The resume needs no catch-up mechanism: being due is a stored date, so
	// the very next tick after an indexer returns picks the want up.
	h.recordHealth(t, "prowlarr", `["indexer"]`, true)
	h.clock.advance(searchBeatInterval)
	enqueued, err = s.pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if enqueued != 1 {
		t.Fatalf("after an indexer came back the pass enqueued %d searches, want 1", enqueued)
	}
}

// Unknown is not unhealthy, on either side of the question.
func TestHealthUnknownAndUnrelatedOutagesDoNotHoldOff(t *testing.T) {
	t.Run("never checked", func(t *testing.T) {
		h := newBeatHarness(t)
		h.want(t, "want-1", true)
		n, err := h.scheduler().pass(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("a node that has never run a health pass scheduled %d searches, want 1 — "+
				"'nobody has looked' is not 'we looked and the answer is no'", n)
		}
	})
	t.Run("a download client is down", func(t *testing.T) {
		h := newBeatHarness(t)
		h.want(t, "want-1", true)
		h.recordHealth(t, "transmission", `["download"]`, false)
		n, err := h.scheduler().pass(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("an unhealthy DOWNLOAD client held off searching (%d scheduled, want 1); "+
				"it says nothing about whether an indexer can answer", n)
		}
	})
	t.Run("one of two indexers is up", func(t *testing.T) {
		h := newBeatHarness(t)
		h.want(t, "want-1", true)
		h.recordHealth(t, "down", `["indexer"]`, false)
		h.recordHealth(t, "up", `["indexer"]`, true)
		n, err := h.scheduler().pass(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("one healthy indexer out of two scheduled %d searches, want 1", n)
		}
	})
}

// The batch bound, so an import of four hundred wants reaches the indexers as
// several passes rather than as one burst — and nothing is skipped.
func TestOnePassIsBounded(t *testing.T) {
	h := newBeatHarness(t)
	for i := range searchBatchLimit + 7 {
		h.want(t, "want-"+itoa(i), true)
	}
	s := h.scheduler()

	first, err := s.pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if first != searchBatchLimit {
		t.Fatalf("one pass enqueued %d searches, want the %d bound", first, searchBatchLimit)
	}
	second, err := s.pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if second != 7 {
		t.Fatalf("the next pass enqueued %d searches, want the remaining 7", second)
	}
	if got := h.searchJobs(t); got != searchBatchLimit+7 {
		t.Fatalf("there are %d search jobs, want one per want (%d)", got, searchBatchLimit+7)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// A cancelled pass stops rather than finishing the batch, and what it did not
// enqueue stays due.
func TestAPassStopsWhenTheControllerIsShuttingDown(t *testing.T) {
	h := newBeatHarness(t)
	h.want(t, "want-1", true)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := h.scheduler().pass(ctx); err != nil {
		t.Fatalf("an ordinary shutdown mid-pass surfaced as an error: %v", err)
	}
}
