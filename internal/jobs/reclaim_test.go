package jobs

import (
	"testing"
	"time"
)

// TestReclaimAllLeasesHandlesFutureDatedLeases is the recover hazard (M7-04): a
// control database restored from a dead node carries leases whose expiry is
// still in the FUTURE by the dead worker's clock. The ordinary reaper only
// reclaims leases already past their expiry, so it would leave these looking
// live; ReclaimAllLeases voids them unconditionally.
func TestReclaimAllLeasesHandlesFutureDatedLeases(t *testing.T) {
	q, clock := newQueue(t)
	enqueue(t, q, EnqueueOptions{Type: "hash_blob"})

	// Claim it with a lease that runs an hour into the future.
	claimed, err := q.Claim(t.Context(), ClaimOptions{Owner: "dead-worker", LeaseTTL: time.Hour})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claimed.State != Leased {
		t.Fatalf("state = %s, want leased", claimed.State)
	}

	// The reaper does NOT touch it — the lease has not expired.
	reaped, err := q.ReapExpiredLeases(t.Context())
	if err != nil {
		t.Fatalf("ReapExpiredLeases: %v", err)
	}
	if reaped != 0 {
		t.Errorf("the reaper reclaimed %d future-dated leases, want 0", reaped)
	}

	// ReclaimAllLeases voids it regardless of the future expiry.
	reclaimed, err := q.ReclaimAllLeases(t.Context())
	if err != nil {
		t.Fatalf("ReclaimAllLeases: %v", err)
	}
	if reclaimed != 1 {
		t.Fatalf("reclaimed %d, want 1", reclaimed)
	}

	// The job is claimable again, and nothing is leased.
	_ = clock
	again, err := q.Claim(t.Context(), ClaimOptions{Owner: "new-worker", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatalf("the reclaimed job could not be claimed by a live worker: %v", err)
	}
	if again.ID != claimed.ID {
		t.Errorf("claimed a different job %s, want the reclaimed %s", again.ID, claimed.ID)
	}
}
