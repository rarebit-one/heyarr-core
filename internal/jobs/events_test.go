package jobs

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
)

// Invariant 7, applied to the queue: every state transition emits an event,
// with no exceptions (§76, ADR-0009). Before this the queue was the exception —
// the constants existed and nothing ever emitted them, so "watch a job through
// the event stream" was not achievable and `--wait` had to poll.

func recorded(t *testing.T, log *events.Log, subject string) []events.Event {
	t.Helper()
	all, err := log.Since(t.Context(), 0, nil, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []events.Event
	for _, e := range all {
		if e.SubjectID == subject {
			out = append(out, e)
		}
	}
	return out
}

func payloadOf(t *testing.T, e events.Event) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Payload, &m); err != nil {
		t.Fatalf("decoding %s payload: %v", e.Type, err)
	}
	return m
}

func TestEveryJobTransitionIsRecorded(t *testing.T) {
	q, log, clock := newQueueWithLog(t)

	job := enqueue(t, q, EnqueueOptions{Type: "ingest_artifact", MaxAttempts: 2})
	got := recorded(t, log, job.ID)
	if len(got) != 1 || got[0].Type != events.TypeJobEnqueued {
		t.Fatalf("after enqueue: %v", types(got))
	}
	if p := payloadOf(t, got[0]); p["type"] != "ingest_artifact" || p["state"] != "pending" {
		t.Errorf("enqueue payload = %v", p)
	}

	claimed, err := q.Claim(t.Context(), ClaimOptions{Owner: "w1", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	// A spent attempt is not an outcome: the job runs again, and a client that
	// treated this as failure would give up early.
	if err := q.Fail(t.Context(), claimed.ID, "w1", errors.New("transient")); err != nil {
		t.Fatal(err)
	}
	got = recorded(t, log, job.ID)
	if len(got) != 2 || got[1].Type != events.TypeJobFailed {
		t.Fatalf("after a retryable failure: %v", types(got))
	}
	if p := payloadOf(t, got[1]); p["terminal"] != false || p["state"] != "pending" {
		t.Errorf("retryable failure payload = %v", p)
	}

	// Exhaust it.
	clock.Advance(time.Hour)
	claimed, err = q.Claim(t.Context(), ClaimOptions{Owner: "w1", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Fail(t.Context(), claimed.ID, "w1", errors.New("fatal")); err != nil {
		t.Fatal(err)
	}
	got = recorded(t, log, job.ID)
	if len(got) != 3 {
		t.Fatalf("after exhausting attempts: %v", types(got))
	}
	p := payloadOf(t, got[2])
	if p["terminal"] != true || p["state"] != string(Dead) {
		t.Errorf("terminal failure payload = %v — `scan --wait` branches on exactly this", p)
	}
	if p["error"] != "fatal" {
		t.Errorf("the terminal event does not carry the cause: %v", p)
	}

	// Retry puts it back, and that is a transition too.
	if err := q.Retry(t.Context(), job.ID); err != nil {
		t.Fatal(err)
	}
	got = recorded(t, log, job.ID)
	if len(got) != 4 || got[3].Type != events.TypeJobEnqueued {
		t.Fatalf("after retry: %v", types(got))
	}
	if p := payloadOf(t, got[3]); p["retried"] != true {
		t.Errorf("retry payload = %v", p)
	}

	// ...and success.
	claimed, err = q.Claim(t.Context(), ClaimOptions{Owner: "w1", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Complete(t.Context(), claimed.ID, "w1"); err != nil {
		t.Fatal(err)
	}
	got = recorded(t, log, job.ID)
	if len(got) != 5 || got[4].Type != events.TypeJobSucceeded {
		t.Fatalf("after completion: %v", types(got))
	}
}

// A lease that expires puts the job back to pending. That is a state
// transition, and one a client especially wants to hear about: "the worker
// running your job died" is otherwise only discoverable by polling.
func TestReapingAnExpiredLeaseIsRecordedPerJob(t *testing.T) {
	q, log, clock := newQueueWithLog(t)

	a := enqueue(t, q, EnqueueOptions{Type: "ingest_artifact"})
	b := enqueue(t, q, EnqueueOptions{Type: "ingest_artifact"})
	for range 2 {
		if _, err := q.Claim(t.Context(), ClaimOptions{Owner: "w1", LeaseTTL: time.Minute}); err != nil {
			t.Fatal(err)
		}
	}

	clock.Advance(2 * time.Minute)
	n, err := q.ReapExpiredLeases(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("reaped %d, want 2", n)
	}

	for _, id := range []string{a.ID, b.ID} {
		got := recorded(t, log, id)
		last := got[len(got)-1]
		if last.Type != events.TypeJobFailed {
			t.Errorf("job %s: last event %s, want %s", id, last.Type, events.TypeJobFailed)
			continue
		}
		p := payloadOf(t, last)
		if p["reaped"] != true || p["terminal"] != false {
			t.Errorf("job %s: reap payload = %v", id, p)
		}
	}
}

// The transition and the event it describes are one transaction, or the log can
// disagree with the table — and the log is the record of what happened.
func TestARefusedTransitionRecordsNothing(t *testing.T) {
	q, log, _ := newQueueWithLog(t)

	job := enqueue(t, q, EnqueueOptions{Type: "ingest_artifact"})
	if _, err := q.Claim(t.Context(), ClaimOptions{Owner: "w1", LeaseTTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	before := len(recorded(t, log, job.ID))

	// Completing as the wrong owner must change nothing at all.
	if err := q.Complete(t.Context(), job.ID, "someone-else"); err == nil {
		t.Fatal("completing a job leased by another worker was accepted")
	}
	if after := len(recorded(t, log, job.ID)); after != before {
		t.Errorf("a refused completion recorded %d event(s)", after-before)
	}

	got, err := q.Get(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != Leased {
		t.Errorf("state = %s, want it untouched at %s", got.State, Leased)
	}
}

func TestAQueueWithoutAnEventLogIsRefused(t *testing.T) {
	// The invariant says no exceptions, and an exception a caller can create by
	// leaving a field nil is still an exception.
	if _, err := New(Options{Writer: nil}); err == nil {
		t.Fatal("a queue with no writer was accepted")
	}
	q, _ := newQueue(t)
	if _, err := New(Options{Writer: q.writer}); err == nil {
		t.Fatal("a queue with no event log was accepted")
	}
}

func types(evs []events.Event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}
