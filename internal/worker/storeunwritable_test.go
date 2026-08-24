package worker

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// TestStoreUnwritableIsRaisedOnce holds the judgement made in #151: twelve jobs
// failing on one store-wide fault is one fault, not twelve, and the log should
// say so once rather than twelve times.
func TestStoreUnwritableIsRaisedOnce(t *testing.T) {
	var alarm onceAlarm
	storeWide := fmt.Errorf("ingest: materialising something: %w", cas.ErrStoreUnwritable)

	raised := 0
	for i := 0; i < 12; i++ {
		if alarm.shouldRaise(storeWide) {
			raised++
		}
	}
	if raised != 1 {
		t.Errorf("a store-wide fault seen 12 times raised the alarm %d times, want 1", raised)
	}
}

// TestStoreUnwritableIgnoresOrdinaryFailures: a job that failed for its own
// reasons must not be dressed up as a store fault, or the alarm stops meaning
// anything.
func TestStoreUnwritableIgnoresOrdinaryFailures(t *testing.T) {
	var alarm onceAlarm
	for _, cause := range []error{
		errors.New("ingest: probing the container: ffprobe not found"),
		fmt.Errorf("cas: writing blob: %w", errors.New("no space left on device")),
		nil,
	} {
		if alarm.shouldRaise(cause) {
			t.Errorf("the store alarm was raised for %v", cause)
		}
	}
	// And the alarm is still armed for the fault it is actually for.
	if !alarm.shouldRaise(fmt.Errorf("wrapped: %w", cas.ErrStoreUnwritable)) {
		t.Error("the alarm was spent on failures that were not store faults")
	}
}

// TestStoreUnwritableRaisesOnceUnderConcurrency: jobs fail on their own
// goroutines, so exactly-once has to survive that.
func TestStoreUnwritableRaisesOnceUnderConcurrency(t *testing.T) {
	var alarm onceAlarm
	var mu sync.Mutex
	raised := 0
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if alarm.shouldRaise(fmt.Errorf("job: %w", cas.ErrStoreUnwritable)) {
				mu.Lock()
				raised++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if raised != 1 {
		t.Errorf("64 concurrent store faults raised the alarm %d times, want 1", raised)
	}
}
