package jobs

import (
	"context"
	"errors"
	"math/rand/v2"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// fakeClock makes lease expiry and backoff ordinary assertions rather than
// sleeps (ADR-0017).
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newQueue(t *testing.T) (*Queue, *fakeClock) {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatalf("opening database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(t.Context(), db); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	clock := newClock()
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	q, err := New(Options{
		Writer: db.Writer(),
		Reader: db.Reader(),
		Clock:  clock,
		Rand:   rand.New(rand.NewPCG(42, 42)), // deterministic jitter
		Events: eventLog,
	})
	if err != nil {
		t.Fatal(err)
	}
	return q, clock
}

// newQueueWithLog is newQueue for the tests that assert on what the queue
// recorded, rather than only on what it did.
func newQueueWithLog(t *testing.T) (*Queue, *events.Log, *fakeClock) {
	t.Helper()
	q, clock := newQueue(t)
	return q, q.events, clock
}

func enqueue(t *testing.T, q *Queue, opts EnqueueOptions) Job {
	t.Helper()
	if opts.Type == "" {
		opts.Type = "scan_library"
	}
	j, err := q.Enqueue(t.Context(), opts)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return j
}

func TestEnqueueAndClaimRoundTrip(t *testing.T) {
	q, _ := newQueue(t)
	enqueued := enqueue(t, q, EnqueueOptions{
		Type:    "ingest_artifact",
		Payload: map[string]string{"path": "/srv/films/x.mkv"},
	})

	claimed, err := q.Claim(t.Context(), ClaimOptions{Owner: "worker-1"})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claimed.ID != enqueued.ID {
		t.Errorf("claimed %s, enqueued %s", claimed.ID, enqueued.ID)
	}
	if claimed.State != Leased {
		t.Errorf("state = %s, want leased", claimed.State)
	}
	if claimed.LeaseOwner != "worker-1" {
		t.Errorf("owner = %q, want worker-1", claimed.LeaseOwner)
	}
	if claimed.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 — the attempt is counted at claim time so a "+
			"worker that dies repeatedly still walks toward dead", claimed.Attempts)
	}
	if string(claimed.Payload) == "" || string(claimed.Payload) == "{}" {
		t.Errorf("payload did not survive the round trip: %s", claimed.Payload)
	}
}

func TestClaimReportsNoWorkRatherThanFailing(t *testing.T) {
	q, _ := newQueue(t)
	// An idle queue is the common case for a polling worker, not an error.
	if _, err := q.Claim(t.Context(), ClaimOptions{Owner: "w"}); !errors.Is(err, ErrNoWork) {
		t.Errorf("Claim on an empty queue returned %v, want ErrNoWork", err)
	}
}

// THE test for this package. Two workers must never both believe they own the
// same job — that is the whole reason claiming is a single statement.
func TestConcurrentClaimsNeverDoubleClaim(t *testing.T) {
	q, _ := newQueue(t)

	const jobCount = 400
	for i := range jobCount {
		enqueue(t, q, EnqueueOptions{Type: "hash_blob", Payload: map[string]int{"n": i}})
	}

	const workers = 8
	var (
		mu     sync.Mutex
		seen   = map[string]string{} // job id -> claiming worker
		dupes  []string
		claims int
	)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner := "worker-" + string(rune('a'+w))
			<-start
			for {
				job, err := q.Claim(context.Background(), ClaimOptions{Owner: owner})
				if errors.Is(err, ErrNoWork) {
					return
				}
				if err != nil {
					mu.Lock()
					dupes = append(dupes, "claim error: "+err.Error())
					mu.Unlock()
					return
				}
				mu.Lock()
				if prev, dup := seen[job.ID]; dup {
					dupes = append(dupes, job.ID+" claimed by both "+prev+" and "+owner)
				}
				seen[job.ID] = owner
				claims++
				mu.Unlock()

				if err := q.Complete(context.Background(), job.ID, owner); err != nil {
					mu.Lock()
					dupes = append(dupes, "complete error: "+err.Error())
					mu.Unlock()
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	for _, d := range dupes {
		t.Error(d)
	}
	if claims != jobCount {
		t.Errorf("%d claims for %d jobs", claims, jobCount)
	}
	if len(seen) != jobCount {
		t.Errorf("%d distinct jobs claimed, want %d", len(seen), jobCount)
	}
}

// A worker that dies costs one lease interval, not a stuck queue.
func TestExpiredLeasesAreReclaimed(t *testing.T) {
	q, clock := newQueue(t)
	enqueue(t, q, EnqueueOptions{})

	claimed, err := q.Claim(t.Context(), ClaimOptions{Owner: "doomed", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	// Before expiry the job is nobody else's.
	if _, err := q.Claim(t.Context(), ClaimOptions{Owner: "other"}); !errors.Is(err, ErrNoWork) {
		t.Error("a live lease was claimable by another worker")
	}
	n, err := q.ReapExpiredLeases(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("reaped %d live leases, want 0", n)
	}

	clock.Advance(2 * time.Minute)

	n, err = q.ReapExpiredLeases(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reaped %d expired leases, want 1", n)
	}

	reclaimed, err := q.Claim(t.Context(), ClaimOptions{Owner: "survivor"})
	if err != nil {
		t.Fatalf("an expired job was not reclaimable: %v", err)
	}
	if reclaimed.ID != claimed.ID {
		t.Error("a different job was reclaimed")
	}
	// The attempt from the dead worker still counts, so a worker that dies
	// repeatedly walks toward dead rather than retrying forever.
	if reclaimed.Attempts != 2 {
		t.Errorf("attempts = %d after one death and one reclaim, want 2", reclaimed.Attempts)
	}
}

// Losing the lease must be reported, because the handler has to stop:
// something else may already be running the work.
func TestOperationsFailOnceTheLeaseIsLost(t *testing.T) {
	q, clock := newQueue(t)
	enqueue(t, q, EnqueueOptions{})
	job, err := q.Claim(t.Context(), ClaimOptions{Owner: "worker-1", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	clock.Advance(2 * time.Minute)
	if _, err := q.ReapExpiredLeases(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Claim(t.Context(), ClaimOptions{Owner: "worker-2"}); err != nil {
		t.Fatal(err)
	}

	for name, op := range map[string]func() error{
		"heartbeat": func() error { return q.Heartbeat(t.Context(), job.ID, "worker-1", time.Minute) },
		"complete":  func() error { return q.Complete(t.Context(), job.ID, "worker-1") },
		"fail":      func() error { return q.Fail(t.Context(), job.ID, "worker-1", errors.New("x")) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := op(); !errors.Is(err, ErrLeaseLost) {
				t.Errorf("%s by the previous owner returned %v, want ErrLeaseLost", name, err)
			}
		})
	}
}

func TestHeartbeatExtendsTheLease(t *testing.T) {
	q, clock := newQueue(t)
	enqueue(t, q, EnqueueOptions{})
	job, err := q.Claim(t.Context(), ClaimOptions{Owner: "w", LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	clock.Advance(50 * time.Second)
	if err := q.Heartbeat(t.Context(), job.ID, "w", time.Minute); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	clock.Advance(50 * time.Second) // 100s total, past the original TTL

	n, err := q.ReapExpiredLeases(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("a heartbeated lease was reaped")
	}
}

// Idempotency: the same logical work enqueued twice while live is one job.
func TestDedupeKeyMakesEnqueueIdempotent(t *testing.T) {
	q, _ := newQueue(t)
	key := "ingest:root-1:/srv/films/x.mkv"

	first := enqueue(t, q, EnqueueOptions{Type: "ingest_artifact", DedupeKey: key})
	second := enqueue(t, q, EnqueueOptions{Type: "ingest_artifact", DedupeKey: key})

	if first.ID != second.ID {
		t.Errorf("enqueueing the same dedupe key twice created two jobs: %s and %s", first.ID, second.ID)
	}
	stats, err := q.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stats[Pending] != 1 {
		t.Errorf("%d pending jobs, want 1", stats[Pending])
	}
}

// ...but a COMPLETED job must not block the same work later, or a library
// could be scanned exactly once, ever.
func TestDedupeKeyIsReusableOnceTheJobFinishes(t *testing.T) {
	q, _ := newQueue(t)
	key := "scan:library-1"

	first := enqueue(t, q, EnqueueOptions{DedupeKey: key})
	claimed, err := q.Claim(t.Context(), ClaimOptions{Owner: "w"})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Complete(t.Context(), claimed.ID, "w"); err != nil {
		t.Fatal(err)
	}

	second := enqueue(t, q, EnqueueOptions{DedupeKey: key})
	if second.ID == first.ID {
		t.Error("a finished job blocked the same work from being enqueued again")
	}
}

func TestPriorityOrdersClaims(t *testing.T) {
	q, _ := newQueue(t)
	low, high := 200, 10
	enqueue(t, q, EnqueueOptions{Type: "low", Priority: &low})
	enqueue(t, q, EnqueueOptions{Type: "high", Priority: &high})

	claimed, err := q.Claim(t.Context(), ClaimOptions{Owner: "w"})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Type != "high" {
		t.Errorf("claimed %q first, want the higher-priority job", claimed.Type)
	}
}

func TestRunAfterDelaysClaiming(t *testing.T) {
	q, clock := newQueue(t)
	enqueue(t, q, EnqueueOptions{RunAfter: clock.Now().Add(time.Hour)})

	if _, err := q.Claim(t.Context(), ClaimOptions{Owner: "w"}); !errors.Is(err, ErrNoWork) {
		t.Error("a job scheduled for the future was claimable now")
	}
	clock.Advance(2 * time.Hour)
	if _, err := q.Claim(t.Context(), ClaimOptions{Owner: "w"}); err != nil {
		t.Errorf("a due job was not claimable: %v", err)
	}
}

// A box with no GPU must never lease a transcode (§75).
func TestCapabilityRouting(t *testing.T) {
	q, _ := newQueue(t)
	enqueue(t, q, EnqueueOptions{Type: "transcode", RequiredCapability: "ffmpeg"})
	enqueue(t, q, EnqueueOptions{Type: "hash_blob"})

	// A worker without the capability gets only the unrestricted job.
	claimed, err := q.Claim(t.Context(), ClaimOptions{Owner: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Type != "hash_blob" {
		t.Errorf("a worker without ffmpeg claimed %q", claimed.Type)
	}
	if _, err := q.Claim(t.Context(), ClaimOptions{Owner: "plain"}); !errors.Is(err, ErrNoWork) {
		t.Error("a worker without the capability claimed a job requiring it")
	}

	// One that advertises it can.
	capable, err := q.Claim(t.Context(), ClaimOptions{Owner: "encoder", Capabilities: []string{"ffmpeg"}})
	if err != nil {
		t.Fatalf("a capable worker could not claim: %v", err)
	}
	if capable.Type != "transcode" {
		t.Errorf("capable worker claimed %q, want transcode", capable.Type)
	}
}

func TestTypeFilterRestrictsClaims(t *testing.T) {
	q, _ := newQueue(t)
	enqueue(t, q, EnqueueOptions{Type: "scan_library"})
	enqueue(t, q, EnqueueOptions{Type: "ingest_artifact"})

	claimed, err := q.Claim(t.Context(), ClaimOptions{Owner: "w", Types: []string{"ingest_artifact"}})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Type != "ingest_artifact" {
		t.Errorf("claimed %q despite a type filter", claimed.Type)
	}
}

// Backoff must grow and must be jittered. Without jitter, a provider outage
// fails every queued job at once and they all retry in lockstep, hammering it
// back down the moment it recovers.
func TestFailBacksOffWithJitter(t *testing.T) {
	q, clock := newQueue(t)
	enqueue(t, q, EnqueueOptions{MaxAttempts: 10})

	var delays []time.Duration
	for range 5 {
		job, err := q.Claim(t.Context(), ClaimOptions{Owner: "w"})
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := q.Fail(t.Context(), job.ID, "w", errors.New("boom")); err != nil {
			t.Fatalf("fail: %v", err)
		}
		after, err := q.Get(t.Context(), job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.State != Pending {
			t.Fatalf("state after failure = %s, want pending", after.State)
		}
		if after.LastError != "boom" {
			t.Errorf("last_error = %q, want the cause recorded", after.LastError)
		}
		delay := after.RunAfter.Sub(clock.Now())
		delays = append(delays, delay)
		clock.Advance(delay + time.Second)
	}

	// Growth: the last retry must wait meaningfully longer than the first.
	if delays[len(delays)-1] <= delays[0] {
		t.Errorf("backoff did not grow: %v", delays)
	}
	// Jitter: identical delays would mean a thundering herd.
	allSame := true
	for _, d := range delays[1:] {
		if d != delays[0] {
			allSame = false
			break
		}
	}
	if allSame {
		t.Errorf("every backoff was identical (%v) — no jitter", delays[0])
	}
	t.Logf("backoff schedule: %v", delays)
}

// Dead is terminal on purpose: a job that cannot succeed should stop consuming
// worker slots and start being visible.
func TestExhaustedJobsGoDeadRatherThanLoopingForever(t *testing.T) {
	q, clock := newQueue(t)
	enqueue(t, q, EnqueueOptions{MaxAttempts: 3})

	for range 3 {
		job, err := q.Claim(t.Context(), ClaimOptions{Owner: "w"})
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := q.Fail(t.Context(), job.ID, "w", errors.New("always fails")); err != nil {
			t.Fatalf("fail: %v", err)
		}
		clock.Advance(time.Hour)
	}

	stats, err := q.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stats[Dead] != 1 {
		t.Errorf("stats = %v, want one dead job", stats)
	}
	if _, err := q.Claim(t.Context(), ClaimOptions{Owner: "w"}); !errors.Is(err, ErrNoWork) {
		t.Error("a dead job was claimed again")
	}
}

// Retry is the operator action after fixing whatever was wrong.
func TestRetryRevivesADeadJob(t *testing.T) {
	q, clock := newQueue(t)
	enqueue(t, q, EnqueueOptions{MaxAttempts: 1})

	job, err := q.Claim(t.Context(), ClaimOptions{Owner: "w"})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Fail(t.Context(), job.ID, "w", errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)

	if err := q.Retry(t.Context(), job.ID); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	revived, err := q.Get(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if revived.State != Pending {
		t.Errorf("state = %s after Retry, want pending", revived.State)
	}
	if revived.Attempts != 0 {
		t.Errorf("attempts = %d after Retry, want reset to 0", revived.Attempts)
	}
	if _, err := q.Claim(t.Context(), ClaimOptions{Owner: "w"}); err != nil {
		t.Errorf("a retried job was not claimable: %v", err)
	}
}

func TestCompleteMarksSucceeded(t *testing.T) {
	q, _ := newQueue(t)
	enqueue(t, q, EnqueueOptions{})
	job, err := q.Claim(t.Context(), ClaimOptions{Owner: "w"})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Complete(t.Context(), job.ID, "w"); err != nil {
		t.Fatal(err)
	}
	done, err := q.Get(t.Context(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.State != Succeeded {
		t.Errorf("state = %s, want succeeded", done.State)
	}
	if done.LeaseOwner != "" {
		t.Errorf("lease owner = %q after completion, want cleared", done.LeaseOwner)
	}
	if done.FinishedAt.IsZero() {
		t.Error("finished_at was not recorded")
	}
}

func TestEnqueueRejectsAnEmptyType(t *testing.T) {
	q, _ := newQueue(t)
	if _, err := q.Enqueue(t.Context(), EnqueueOptions{}); err == nil {
		t.Error("Enqueue accepted a job with no type")
	}
}

func TestClaimRequiresAnOwner(t *testing.T) {
	q, _ := newQueue(t)
	if _, err := q.Claim(t.Context(), ClaimOptions{}); err == nil {
		t.Error("Claim accepted an empty owner — the lease would belong to nobody")
	}
}

func TestGetReportsMissingJobs(t *testing.T) {
	q, _ := newQueue(t)
	if _, err := q.Get(t.Context(), "no-such-job"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get returned %v, want ErrNotFound", err)
	}
}

func TestNewRequiresAWriter(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Error("New accepted a queue with no writer")
	}
}
