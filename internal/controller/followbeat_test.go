package controller

import (
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
)

// The follow beat (§55, M12) against a real database. Like the search beat's
// tests, everything uses the injected clock: a backoff asserted with a sleep is
// slow when it passes and flaky when it does not (ADR-0017).

func (h *beatHarness) followScheduler() *followScheduler {
	return newFollowScheduler(h.cat, h.queue, h.clock, discard())
}

func (h *beatHarness) pollJobs(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.db.Reader().QueryRow(
		`SELECT count(*) FROM jobs WHERE type = ?`, followed.PollSourceJobType).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// source seeds a followed source through the catalog — the real create path — so
// the beat reads exactly what a follow would have written.
func (h *beatHarness) source(t *testing.T, id string) string {
	t.Helper()
	work := "w-" + id
	h.exec(t, `INSERT INTO works
		(id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES (?, 'series', ?, 'The Series', 'the series', 2020, '{}', ?, ?)`,
		work, "series:"+id, beatStamp, beatStamp)
	src, err := h.cat.CreateFollowSource(t.Context(), followed.Source{
		ID: id, WorkID: work, Type: followed.TypeTVSeries, FeedRef: "tvdb:" + id,
		QualityProfileID: "q1", Monitor: true, Backfill: followed.BackfillFromNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	return src.ID
}

// A due source is enqueued a poll and its schedule advances, so the next tick
// does not re-enqueue it.
func TestFollowBeatEnqueuesAPollForADueSource(t *testing.T) {
	h := newBeatHarness(t)
	id := h.source(t, "src-1")

	n, err := h.followScheduler().pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the pass enqueued %d polls, want 1", n)
	}
	if got := h.pollJobs(t); got != 1 {
		t.Fatalf("%d poll_source jobs exist, want 1", got)
	}

	got, err := h.cat.FollowSource(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.NextPollAt.IsZero() {
		t.Error("the source's next_poll_at was not advanced, so the next tick will re-enqueue it")
	}
}

// The dedupe key means a second pass over the same due source does not pile up a
// second job (invariant 9).
func TestFollowBeatDoesNotDuplicateAPoll(t *testing.T) {
	h := newBeatHarness(t)
	h.source(t, "src-1")

	if _, err := h.followScheduler().pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A fresh scheduler, as a second controller would be, over the same instant.
	if _, err := h.followScheduler().pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := h.pollJobs(t); got != 1 {
		t.Errorf("%d poll_source jobs exist after two passes, want 1", got)
	}
}

// Hold off while every feed adapter is unhealthy: polling then would fill the
// queue with work that will fail, and every due source is still due when an
// adapter returns.
func TestFollowBeatHoldsOffWhenEveryFeedAdapterIsUnhealthy(t *testing.T) {
	h := newBeatHarness(t)
	h.source(t, "src-1")
	h.recordHealth(t, "tvdb", `["metadata"]`, false)

	n, err := h.followScheduler().pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("the pass enqueued %d polls while every feed adapter was down, want 0", n)
	}
	if got := h.pollJobs(t); got != 0 {
		t.Errorf("%d poll_source jobs exist, want 0", got)
	}
}

// Unknown is not unhealthy: a node that has never run a health pass, or has no
// metadata provider, polls anyway — the job then stays pending and visible for
// want of the capability, which the operator can see (ADR-0025).
func TestFollowBeatSchedulesWhenHealthIsUnknown(t *testing.T) {
	h := newBeatHarness(t)
	h.source(t, "src-1")

	n, err := h.followScheduler().pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the pass enqueued %d polls with no health rows, want 1 — unknown is not unhealthy", n)
	}
}

// A healthy feed adapter beside an unhealthy one is enough to poll: hold-off is
// only for when NOTHING could answer.
func TestFollowBeatSchedulesWhenOneAdapterIsHealthy(t *testing.T) {
	h := newBeatHarness(t)
	h.source(t, "src-1")
	h.recordHealth(t, "tvdb-down", `["metadata"]`, false)
	h.recordHealth(t, "tvdb-up", `["metadata"]`, true)

	n, err := h.followScheduler().pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the pass enqueued %d polls with one healthy adapter, want 1", n)
	}
}
