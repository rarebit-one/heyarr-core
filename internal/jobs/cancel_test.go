package jobs

import (
	"encoding/json"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/events"
)

// TestCancelPendingEndsOnlyWhatMatchesAndSaysSo: the matching pending jobs go
// to dead with the reason, each with its own terminal job.failed marked
// cancelled; a non-matching job, a job of another type and a LEASED job are
// left exactly as they were (#658).
func TestCancelPendingEndsOnlyWhatMatchesAndSaysSo(t *testing.T) {
	q, log, _ := newQueueWithLog(t)
	ctx := t.Context()

	to := func(peer string) map[string]string { return map[string]string{"peer": peer} }
	// Leased first, while it is the only claimable job, so the claim cannot
	// take anything else.
	running := enqueue(t, q, EnqueueOptions{Type: "replicate_blob", Payload: to("gone"), DedupeKey: "d"})
	if j, err := q.Claim(ctx, ClaimOptions{Owner: "w", Types: []string{"replicate_blob"}}); err != nil || j.ID != running.ID {
		t.Fatalf("claim = %v, %v; want %s", j.ID, err, running.ID)
	}
	gone1 := enqueue(t, q, EnqueueOptions{Type: "replicate_blob", Payload: to("gone"), DedupeKey: "a"})
	gone2 := enqueue(t, q, EnqueueOptions{Type: "replicate_blob", Payload: to("gone"), DedupeKey: "b"})
	kept := enqueue(t, q, EnqueueOptions{Type: "replicate_blob", Payload: to("kept"), DedupeKey: "c"})
	other := enqueue(t, q, EnqueueOptions{Type: "scan_library", Payload: to("gone")})

	matchGone := func(j Job) bool {
		var p map[string]string
		_ = json.Unmarshal(j.Payload, &p)
		return p["peer"] == "gone"
	}
	n, err := q.CancelPending(ctx, "replicate_blob", matchGone, "cancelled: peer removed")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("cancelled %d, want 2", n)
	}

	for _, tc := range []struct {
		id   string
		want State
	}{{gone1.ID, Dead}, {gone2.ID, Dead}, {kept.ID, Pending}, {other.ID, Pending}, {running.ID, Leased}} {
		j, err := q.Get(ctx, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if j.State != tc.want {
			t.Errorf("job %s is %s, want %s", tc.id, j.State, tc.want)
		}
		if tc.want == Dead && j.LastError != "cancelled: peer removed" {
			t.Errorf("job %s last_error %q", tc.id, j.LastError)
		}
	}

	for _, id := range []string{gone1.ID, gone2.ID} {
		evs := recorded(t, log, id)
		last := evs[len(evs)-1]
		if last.Type != events.TypeJobFailed {
			t.Fatalf("job %s: last event %s, want job.failed", id, last.Type)
		}
		p := payloadOf(t, last)
		if p["terminal"] != true || p["permanent"] != true || p["cancelled"] != true {
			t.Fatalf("job %s: cancel event payload %v", id, p)
		}
	}

	// Idempotent: nothing left to cancel.
	if n, err := q.CancelPending(ctx, "replicate_blob", matchGone, "again"); err != nil || n != 0 {
		t.Fatalf("second cancel = %d, %v; want 0, nil", n, err)
	}
}
