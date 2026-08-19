package events

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

func newLog(t *testing.T) *Log {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(t.Context(), db); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	l, err := New(Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestEmitPersistsAndAssignsMonotonicSequence(t *testing.T) {
	l := newLog(t)

	var seqs []int64
	for i := range 10 {
		e, err := l.Emit(t.Context(), TypeBlobCreated, "blob", "b"+string(rune('0'+i)), map[string]int{"i": i})
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		seqs = append(seqs, e.Seq)
		if e.ID == "" {
			t.Error("event has no id")
		}
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Errorf("sequence went backwards: %v", seqs)
			break
		}
	}

	latest, err := l.Latest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if latest != seqs[len(seqs)-1] {
		t.Errorf("Latest = %d, want %d", latest, seqs[len(seqs)-1])
	}
}

// The property that makes reconnection safe: a client that saw seq N and asks
// for everything after N sees no gaps and no duplicates.
func TestSinceIsGaplessAndDuplicateFree(t *testing.T) {
	l := newLog(t)
	const total = 50
	for i := range total {
		if _, err := l.Emit(t.Context(), TypeJobSucceeded, "job", "j", map[string]int{"i": i}); err != nil {
			t.Fatal(err)
		}
	}

	// Read the whole log in small pages, exactly as a reconnecting client would.
	var seen []int64
	var after int64
	for {
		batch, err := l.Since(t.Context(), after, nil, 7)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) == 0 {
			break
		}
		for _, e := range batch {
			seen = append(seen, e.Seq)
			after = e.Seq
		}
	}

	if len(seen) != total {
		t.Fatalf("paged read saw %d events, want %d", len(seen), total)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] != seen[i-1]+1 {
			t.Errorf("gap or duplicate between %d and %d", seen[i-1], seen[i])
		}
	}
}

func TestSinceFiltersByType(t *testing.T) {
	l := newLog(t)
	for _, tt := range []string{TypeBlobCreated, TypeJobFailed, TypeJobSucceeded, TypeAssetCreated} {
		if _, err := l.Emit(t.Context(), tt, "", "", nil); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("exact", func(t *testing.T) {
		got, err := l.Since(t.Context(), 0, []string{TypeBlobCreated}, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Type != TypeBlobCreated {
			t.Errorf("exact filter returned %d events: %+v", len(got), got)
		}
	})

	// §76's namespaces are the reason prefixes exist.
	t.Run("prefix", func(t *testing.T) {
		got, err := l.Since(t.Context(), 0, []string{"job.*"}, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Errorf("job.* returned %d events, want 2", len(got))
		}
	})

	t.Run("several patterns", func(t *testing.T) {
		got, err := l.Since(t.Context(), 0, []string{"blob.*", TypeAssetCreated}, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Errorf("returned %d events, want 2", len(got))
		}
	})
}

func TestSubscribeReceivesLiveEvents(t *testing.T) {
	l := newLog(t)
	sub := l.Subscribe(16)
	defer sub.Close()

	if _, err := l.Emit(t.Context(), TypeBlobCreated, "blob", "b1", nil); err != nil {
		t.Fatal(err)
	}

	select {
	case e := <-sub.Events():
		if e.Type != TypeBlobCreated {
			t.Errorf("received %q, want %q", e.Type, TypeBlobCreated)
		}
		if e.Seq == 0 {
			t.Error("live event has no sequence number")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event was delivered to the subscriber")
	}
}

func TestSubscriptionFiltersByType(t *testing.T) {
	l := newLog(t)
	sub := l.Subscribe(16, "job.*")
	defer sub.Close()

	if _, err := l.Emit(t.Context(), TypeBlobCreated, "", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Emit(t.Context(), TypeJobFailed, "", "", nil); err != nil {
		t.Fatal(err)
	}

	select {
	case e := <-sub.Events():
		if e.Type != TypeJobFailed {
			t.Errorf("a filtered subscriber received %q", e.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the matching event was not delivered")
	}
}

// THE property for this package: one stalled client must never wedge the
// writers. An unbounded queue would turn a broken connection into unbounded
// memory growth and take the whole process down instead of the one thing that
// is actually broken.
func TestASlowSubscriberNeverBlocksAWriter(t *testing.T) {
	l := newLog(t)
	stalled := l.Subscribe(2) // deliberately tiny, and never read
	defer stalled.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 200 {
			if _, err := l.Emit(context.Background(), TypeJobSucceeded, "job", "j", map[string]int{"i": i}); err != nil {
				t.Errorf("Emit blocked or failed: %v", err)
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("writes stalled behind a subscriber that stopped reading")
	}

	if stalled.Dropped() == 0 {
		t.Error("the stalled subscriber reported no drops, so it silently missed events instead")
	}
	// Everything still reached the log, which is the record of what happened.
	latest, err := l.Latest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if latest != 200 {
		t.Errorf("log holds %d events, want 200 — durability must not depend on subscribers", latest)
	}
	t.Logf("the stalled subscriber dropped %d events; all 200 were persisted", stalled.Dropped())
}

// A dropped event means the client's view has gaps, and it must be able to
// tell — otherwise it would trust an incomplete stream.
func TestDroppedEventsAreReportedNotHidden(t *testing.T) {
	l := newLog(t)
	sub := l.Subscribe(1)
	defer sub.Close()

	for range 20 {
		if _, err := l.Emit(t.Context(), TypeBlobCreated, "", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	if sub.Dropped() == 0 {
		t.Fatal("events were silently discarded without being counted")
	}

	// And the client can recover the full history via Since.
	all, err := l.Since(t.Context(), 0, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 20 {
		t.Errorf("Since returned %d events, want all 20 — the log is the record", len(all))
	}
}

// Durability before fan-out: a subscriber must never see an event that is not
// in the log, because it may act on it.
func TestEveryDeliveredEventIsAlreadyPersisted(t *testing.T) {
	l := newLog(t)
	sub := l.Subscribe(256)
	defer sub.Close()

	const total = 100
	for i := range total {
		if _, err := l.Emit(t.Context(), TypeIngestCompleted, "asset", "a", map[string]int{"i": i}); err != nil {
			t.Fatal(err)
		}
	}

	for range total {
		select {
		case e := <-sub.Events():
			got, err := l.Since(t.Context(), e.Seq-1, nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].ID != e.ID {
				t.Fatalf("event %s was delivered but is not in the log", e.ID)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("delivery stalled")
		}
	}
}

func TestConcurrentEmitsAllPersistWithDistinctSequences(t *testing.T) {
	l := newLog(t)

	const writers, each = 8, 25
	var wg sync.WaitGroup
	seqs := make(chan int64, writers*each)
	start := make(chan struct{})
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := range each {
				e, err := l.Emit(context.Background(), TypeJobEnqueued, "job", "j", map[string]int{"w": w, "i": i})
				if err != nil {
					t.Errorf("Emit: %v", err)
					return
				}
				seqs <- e.Seq
			}
		}()
	}
	close(start)
	wg.Wait()
	close(seqs)

	seen := map[int64]bool{}
	for s := range seqs {
		if seen[s] {
			t.Errorf("sequence %d was assigned twice", s)
		}
		seen[s] = true
	}
	if len(seen) != writers*each {
		t.Errorf("%d distinct sequences for %d events", len(seen), writers*each)
	}
}

func TestCloseIsIdempotentAndUnsubscribes(t *testing.T) {
	l := newLog(t)
	sub := l.Subscribe(4)
	if l.SubscriberCount() != 1 {
		t.Fatalf("subscriber count = %d, want 1", l.SubscriberCount())
	}
	sub.Close()
	sub.Close() // must not panic on a double close
	if l.SubscriberCount() != 0 {
		t.Errorf("subscriber count = %d after Close, want 0", l.SubscriberCount())
	}
	// Emitting after every subscriber has gone must still work.
	if _, err := l.Emit(t.Context(), TypeSystemStopped, "", "", nil); err != nil {
		t.Errorf("Emit failed with no subscribers: %v", err)
	}
}

func TestEmitRejectsAnEmptyType(t *testing.T) {
	l := newLog(t)
	if _, err := l.Emit(t.Context(), "", "", "", nil); err == nil {
		t.Error("Emit accepted an event with no type")
	}
}

func TestNewRequiresAWriter(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("New accepted a log with no writer")
	}
}

func TestMatchType(t *testing.T) {
	tests := []struct {
		pattern, eventType string
		want               bool
	}{
		{"blob.created", "blob.created", true},
		{"blob.created", "blob.deleted", false},
		{"blob.*", "blob.created", true},
		{"blob.*", "job.failed", false},
		{"*", "anything", true},
		{"", "anything", true},
		{"job.", "job.failed", false},
	}
	for _, tt := range tests {
		if got := matchType(tt.pattern, tt.eventType); got != tt.want {
			t.Errorf("matchType(%q, %q) = %v, want %v", tt.pattern, tt.eventType, got, tt.want)
		}
	}
}

// typeFilter builds part of a SQL string, so what it may emit is pinned here.
// Every caller-supplied value must arrive as a bind parameter; if a future
// change interpolates one, this fails rather than shipping an injection.
func TestTypeFilterOnlyEmitsBoundClauses(t *testing.T) {
	hostile := []string{
		"blob.created",
		"job.*",
		"'; DROP TABLE events; --",
		"x' OR '1'='1",
		"%' OR type LIKE '%",
	}
	clause, args := typeFilter(hostile)

	for _, part := range strings.Split(clause, " OR ") {
		part = strings.TrimSpace(part)
		if part != "type = ?" && part != "type LIKE ?" {
			t.Errorf("typeFilter emitted %q — only bound clauses are allowed in the SQL string", part)
		}
	}
	if len(args) != len(hostile) {
		t.Errorf("got %d bind arguments for %d patterns", len(args), len(hostile))
	}

	// And end to end: a hostile pattern must match nothing rather than execute.
	l := newLog(t)
	if _, err := l.Emit(t.Context(), TypeBlobCreated, "", "", nil); err != nil {
		t.Fatal(err)
	}
	got, err := l.Since(t.Context(), 0, []string{"'; DROP TABLE events; --"}, 100)
	if err != nil {
		t.Fatalf("a hostile type pattern produced an error rather than no matches: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a hostile pattern matched %d events", len(got))
	}
	// The table is still there.
	if _, err := l.Latest(t.Context()); err != nil {
		t.Errorf("the events table did not survive: %v", err)
	}
}
