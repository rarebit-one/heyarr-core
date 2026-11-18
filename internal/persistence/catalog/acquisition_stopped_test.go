package catalog_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// steppedClock is a clock a test moves by hand.
type steppedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *steppedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *steppedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// A stopped transfer's since is when its CURRENT reason was first seen (#883):
// a repeat keeps it, a change of reason restarts it, running clears it. That
// is what status waits a grace on.
func TestAStoppedTransferKeepsWhenItStopped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t)
	clock := &steppedClock{t: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)}
	eventLog, err := events.New(events.Options{Writer: h.db.Writer(), Reader: h.db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{
		DB: h.db, Events: eventLog, PeerName: "test", PeerSite: "test-site", Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	record := func(stopped providers.StopReason) catalog.Acquisition {
		t.Helper()
		tr := transfer(infohash, "Something.mkv")
		tr.Stopped = stopped
		if _, err := cat.RecordAcquisition(ctx, catalog.TransferToAcquisition("acq-1", h.want, "acquire", tr, "")); err != nil {
			t.Fatal(err)
		}
		got, err := cat.AcquisitionFor(ctx, h.want)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	start := clock.Now()

	if got := record(""); got.Stopped != "" || !got.StoppedSince.IsZero() {
		t.Fatalf("a running transfer = (%q, %v), want neither", got.Stopped, got.StoppedSince)
	}

	clock.advance(time.Minute)
	held := start.Add(time.Minute)
	if got := record(providers.StoppedHeld); got.Stopped != providers.StoppedHeld || !got.StoppedSince.Equal(held) {
		t.Fatalf("first held pass = (%q, %v), want (held, %v)", got.Stopped, got.StoppedSince, held)
	}
	clock.advance(time.Hour)
	if got := record(providers.StoppedHeld); !got.StoppedSince.Equal(held) {
		t.Errorf("a repeat moved since to %v, want it kept at %v", got.StoppedSince, held)
	}

	clock.advance(time.Minute)
	if got := record(providers.StoppedAfterRun); got.Stopped != providers.StoppedAfterRun || !got.StoppedSince.Equal(clock.Now()) {
		t.Errorf("a changed reason = (%q, %v), want (after_run, %v)", got.Stopped, got.StoppedSince, clock.Now())
	}

	clock.advance(time.Minute)
	if got := record(""); got.Stopped != "" || !got.StoppedSince.IsZero() {
		t.Errorf("running again = (%q, %v), want neither", got.Stopped, got.StoppedSince)
	}
}

// The migration's CHECK keeps the reason to its vocabulary.
func TestAStoppedReasonOutsideTheVocabularyIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	insert := func(id, external, stopped string) error {
		_, err := h.db.Writer().Exec(`INSERT INTO acquisitions
			(id, desired_item_id, provider, external_id, stopped, created_at, updated_at, last_seen_at)
			VALUES (?, ?, 'p', ?, ?, ?, ?, ?)`, id, h.want, external, stopped, stamp, stamp, stamp)
		return err
	}
	if err := insert("a", "x", "paused"); err == nil {
		t.Error("a stopped reason outside the vocabulary was stored")
	}
	if err := insert("b", "y", "held"); err != nil {
		t.Errorf("a stopped reason in the vocabulary was refused: %v", err)
	}
}
