package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/jobs"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeQueue is an in-memory stand-in, so the runtime's behaviour is tested
// without a database in the way.
type fakeQueue struct {
	mu        sync.Mutex
	pending   []jobs.Job
	leased    map[string]string // job id -> owner
	completed []string
	failed    map[string]error
	// leaseLostFor makes Heartbeat report the lease gone, simulating a reaped
	// worker.
	leaseLostFor map[string]bool
	heartbeats   map[string]int
	reaps        int
}

func newFakeQueue(js ...jobs.Job) *fakeQueue {
	return &fakeQueue{
		pending:      js,
		leased:       map[string]string{},
		failed:       map[string]error{},
		leaseLostFor: map[string]bool{},
		heartbeats:   map[string]int{},
	}
}

func (q *fakeQueue) Claim(_ context.Context, opts jobs.ClaimOptions) (jobs.Job, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, j := range q.pending {
		if len(opts.Types) > 0 && !contains(opts.Types, j.Type) {
			continue
		}
		if j.RequiredCapability != "" && !contains(opts.Capabilities, j.RequiredCapability) {
			continue
		}
		q.pending = append(q.pending[:i], q.pending[i+1:]...)
		q.leased[j.ID] = opts.Owner
		return j, nil
	}
	return jobs.Job{}, jobs.ErrNoWork
}

func (q *fakeQueue) Heartbeat(_ context.Context, id, owner string, _ time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.heartbeats[id]++
	if q.leaseLostFor[id] {
		return jobs.ErrLeaseLost
	}
	if q.leased[id] != owner {
		return jobs.ErrLeaseLost
	}
	return nil
}

func (q *fakeQueue) Complete(_ context.Context, id, owner string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.leased[id] != owner {
		return jobs.ErrLeaseLost
	}
	delete(q.leased, id)
	q.completed = append(q.completed, id)
	return nil
}

func (q *fakeQueue) Fail(_ context.Context, id, owner string, cause error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.leased[id] != owner {
		return jobs.ErrLeaseLost
	}
	delete(q.leased, id)
	q.failed[id] = cause
	return nil
}

func (q *fakeQueue) ReapExpiredLeases(context.Context) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.reaps++
	return 0, nil
}

func (q *fakeQueue) completedIDs() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.completed...)
}

func (q *fakeQueue) failure(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.failed[id]
}

func (q *fakeQueue) loseLease(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.leaseLostFor[id] = true
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func job(id, jobType string) jobs.Job {
	return jobs.Job{ID: id, Type: jobType, State: jobs.Leased, MaxAttempts: 3}
}

func fastConfig(owner string) Config {
	return Config{
		Owner:             owner,
		Slots:             4,
		LeaseTTL:          200 * time.Millisecond,
		HeartbeatInterval: 20 * time.Millisecond,
		PollInterval:      5 * time.Millisecond,
		ReapInterval:      20 * time.Millisecond,
		DrainTimeout:      5 * time.Second,
	}
}

func runFor(t *testing.T, rt *Runtime, until func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx) }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !until() {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	if !until() {
		t.Fatal("the condition was never met")
	}
}

func TestRuntimeRunsRegisteredHandlers(t *testing.T) {
	q := newFakeQueue(job("j1", "hash_blob"), job("j2", "hash_blob"))
	reg := NewRegistry()
	var mu sync.Mutex
	var seen []string
	reg.RegisterFunc("hash_blob", func(_ context.Context, j jobs.Job) error {
		mu.Lock()
		seen = append(seen, j.ID)
		mu.Unlock()
		return nil
	})

	rt, err := NewRuntime(fastConfig("w1"), q, reg, discard())
	if err != nil {
		t.Fatal(err)
	}
	runFor(t, rt, func() bool { return len(q.completedIDs()) == 2 })

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Errorf("the handler saw %d jobs, want 2", len(seen))
	}
}

// A worker must not claim a type it cannot handle — the job is left for a
// worker that can, which is what capability routing is for (§75). It must
// certainly not fail the job, because "nobody here can run this" is not the
// same as "this work is broken".
func TestUnregisteredTypesAreLeftAloneNotFailed(t *testing.T) {
	q := newFakeQueue(job("j1", "nonexistent"), job("j2", "hash_blob"))
	reg := NewRegistry()
	reg.RegisterFunc("hash_blob", func(context.Context, jobs.Job) error { return nil })

	rt, err := NewRuntime(fastConfig("w1"), q, reg, discard())
	if err != nil {
		t.Fatal(err)
	}
	runFor(t, rt, func() bool { return len(q.completedIDs()) == 1 })

	if got := q.completedIDs(); len(got) != 1 || got[0] != "j2" {
		t.Errorf("completed = %v, want just j2", got)
	}
	if err := q.failure("j1"); err != nil {
		t.Errorf("an unhandleable job was failed with %v — it should be left for a worker that can run it", err)
	}
	q.mu.Lock()
	stillPending := len(q.pending)
	q.mu.Unlock()
	if stillPending != 1 {
		t.Errorf("%d jobs left pending, want the unhandleable one still queued", stillPending)
	}
}

// The defensive path: if a job of an unregistered type is somehow claimed —
// the registry changed under a running worker, or another process enqueued it
// during a rolling upgrade — it must fail the JOB with an actionable message
// rather than crash the worker.
func TestAClaimedUnregisteredTypeFailsTheJobNotTheWorker(t *testing.T) {
	q := newFakeQueue()
	reg := NewRegistry()
	rt, err := NewRuntime(fastConfig("w1"), q, reg, discard())
	if err != nil {
		t.Fatal(err)
	}

	// Hand execute a job the registry does not know about.
	q.mu.Lock()
	q.leased["orphan"] = "w1"
	q.mu.Unlock()
	rt.execute(context.Background(), job("orphan", "vanished_type"))

	failure := q.failure("orphan")
	if failure == nil {
		t.Fatal("a claimed job with no handler was not failed")
	}
	if want := "no handler is registered"; !strings.Contains(failure.Error(), want) {
		t.Errorf("failure = %q, want it to say %q", failure, want)
	}
}

// A panicking handler must not take the process down.
func TestPanickingHandlerFailsTheJobAndKeepsRunning(t *testing.T) {
	q := newFakeQueue(job("boom", "explodes"), job("fine", "works"))
	reg := NewRegistry()
	reg.RegisterFunc("explodes", func(context.Context, jobs.Job) error { panic("kaboom") })
	reg.RegisterFunc("works", func(context.Context, jobs.Job) error { return nil })

	rt, err := NewRuntime(fastConfig("w1"), q, reg, discard())
	if err != nil {
		t.Fatal(err)
	}
	runFor(t, rt, func() bool { return len(q.completedIDs()) == 1 && q.failure("boom") != nil })

	if err := q.failure("boom"); err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Errorf("panic failure = %v, want it recorded as a panic", err)
	}
	if rt.Stats().Panicked != 1 {
		t.Errorf("panicked count = %d, want 1", rt.Stats().Panicked)
	}
	// The worker kept going.
	if got := q.completedIDs(); len(got) != 1 || got[0] != "fine" {
		t.Errorf("completed = %v — the worker did not survive the panic", got)
	}
}

// Losing the lease must cancel the running handler, because something else may
// already be running the same work (ADR-0008).
func TestLosingTheLeaseCancelsTheHandler(t *testing.T) {
	q := newFakeQueue(job("j1", "slow"))
	reg := NewRegistry()

	started := make(chan struct{})
	cancelled := make(chan struct{})
	var once sync.Once
	reg.RegisterFunc("slow", func(ctx context.Context, _ jobs.Job) error {
		once.Do(func() { close(started) })
		<-ctx.Done() // must be cancelled when the lease goes
		close(cancelled)
		return ctx.Err()
	})

	rt, err := NewRuntime(fastConfig("w1"), q, reg, discard())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never started")
	}

	q.loseLease("j1")

	select {
	case <-cancelled:
		// The handler observed cancellation, which is the point.
	case <-time.After(5 * time.Second):
		t.Fatal("losing the lease did not cancel the running handler — two workers could run the same job")
	}
	cancel()
	<-done
}

// Draining means in-flight work finishes. A worker killed mid-hash otherwise
// costs a whole lease interval and redoes work that was nearly complete.
func TestShutdownDrainsInFlightWork(t *testing.T) {
	q := newFakeQueue(job("j1", "slow"))
	reg := NewRegistry()

	started := make(chan struct{})
	finished := make(chan struct{})
	reg.RegisterFunc("slow", func(ctx context.Context, _ jobs.Job) error {
		close(started)
		// Keep working past the shutdown signal.
		time.Sleep(300 * time.Millisecond)
		if ctx.Err() != nil {
			t.Error("the handler's context was cancelled during a graceful drain")
		}
		close(finished)
		return nil
	})

	cfg := fastConfig("w1")
	cfg.LeaseTTL = 5 * time.Second
	cfg.HeartbeatInterval = 500 * time.Millisecond
	rt, err := NewRuntime(cfg, q, reg, discard())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx) }()

	<-started
	cancel() // shut down while the job is mid-flight

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run never returned")
	}

	select {
	case <-finished:
	default:
		t.Fatal("the in-flight job was abandoned instead of drained")
	}
	if got := q.completedIDs(); len(got) != 1 {
		t.Errorf("completed = %v, want the drained job to have been marked complete", got)
	}
}

// Running sixteen hashers against eight spinning disks is slower than four:
// per-type caps exist because job types have different shapes.
func TestPerTypeConcurrencyIsCapped(t *testing.T) {
	var js []jobs.Job
	for i := range 12 {
		js = append(js, job(string(rune('a'+i)), "hash_blob"))
	}
	q := newFakeQueue(js...)
	reg := NewRegistry()

	var mu sync.Mutex
	inFlight, peak := 0, 0
	release := make(chan struct{})
	reg.Register("hash_blob", Registration{
		MaxConcurrent: 2,
		Handler: HandlerFunc(func(context.Context, jobs.Job) error {
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()

			<-release

			mu.Lock()
			inFlight--
			mu.Unlock()
			return nil
		}),
	})

	cfg := fastConfig("w1")
	cfg.Slots = 8 // deliberately more slots than the type allows
	rt, err := NewRuntime(cfg, q, reg, discard())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx) }()

	// Let it saturate, then check the ceiling held.
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	observed := peak
	mu.Unlock()
	close(release)
	cancel()
	<-done

	if observed > 2 {
		t.Errorf("peak concurrency for hash_blob was %d, want at most 2", observed)
	}
	if observed == 0 {
		t.Error("no jobs ran at all")
	}
	t.Logf("peak concurrent hash_blob handlers: %d (slots: 8, cap: 2)", observed)
}

// A worker must not claim work it cannot run (§75).
func TestCapabilityGatedTypesAreNotClaimed(t *testing.T) {
	q := newFakeQueue(job("t1", "transcode"), job("h1", "hash_blob"))
	reg := NewRegistry()
	reg.Register("transcode", Registration{
		RequiredCapability: "ffmpeg",
		Handler:            HandlerFunc(func(context.Context, jobs.Job) error { return nil }),
	})
	reg.RegisterFunc("hash_blob", func(context.Context, jobs.Job) error { return nil })

	// No ffmpeg advertised.
	rt, err := NewRuntime(fastConfig("plain"), q, reg, discard())
	if err != nil {
		t.Fatal(err)
	}
	runFor(t, rt, func() bool { return len(q.completedIDs()) == 1 })

	if got := q.completedIDs(); len(got) != 1 || got[0] != "h1" {
		t.Errorf("completed = %v, want only the uncapable job", got)
	}
	if q.failure("t1") != nil {
		t.Error("the capability-gated job was claimed and failed rather than left alone")
	}
}

func TestHandlerErrorsFailTheJob(t *testing.T) {
	q := newFakeQueue(job("j1", "fails"))
	reg := NewRegistry()
	sentinel := errors.New("provider unreachable")
	reg.RegisterFunc("fails", func(context.Context, jobs.Job) error { return sentinel })

	rt, err := NewRuntime(fastConfig("w1"), q, reg, discard())
	if err != nil {
		t.Fatal(err)
	}
	runFor(t, rt, func() bool { return q.failure("j1") != nil })

	if !errors.Is(q.failure("j1"), sentinel) {
		t.Errorf("failure = %v, want the handler's error", q.failure("j1"))
	}
	if rt.Stats().Failed != 1 {
		t.Errorf("failed count = %d, want 1", rt.Stats().Failed)
	}
}

// A heartbeat slower than the lease means the lease expires under a running
// job, another worker claims it, and both run the same work — silently. Refuse
// the configuration rather than ship that.
func TestConfigRefusesAHeartbeatSlowerThanTheLease(t *testing.T) {
	cfg := Config{Owner: "w", LeaseTTL: 10 * time.Second, HeartbeatInterval: 30 * time.Second}
	cfg.applyDefaults()
	err := cfg.Validate()
	if err == nil {
		t.Fatal("a heartbeat slower than the lease was accepted")
	}
	if !strings.Contains(err.Error(), "run twice") {
		t.Errorf("error = %q, want it to explain the consequence", err)
	}

	if _, err := NewRuntime(Config{Owner: "w", LeaseTTL: time.Second, HeartbeatInterval: 2 * time.Second},
		newFakeQueue(), NewRegistry(), discard()); err == nil {
		t.Error("NewRuntime accepted the same misconfiguration")
	}
}

func TestConfigRequiresAnOwner(t *testing.T) {
	if _, err := NewRuntime(Config{}, newFakeQueue(), NewRegistry(), discard()); err == nil {
		t.Error("NewRuntime accepted an empty owner — the lease would belong to nobody")
	}
}

// Registering a type twice is a wiring bug. Failing at startup beats silently
// taking the last registration and running jobs through the wrong code.
func TestDuplicateRegistrationPanicsAtWiringTime(t *testing.T) {
	reg := NewRegistry()
	reg.RegisterFunc("dup", func(context.Context, jobs.Job) error { return nil })

	defer func() {
		if recover() == nil {
			t.Error("registering the same job type twice was allowed")
		}
	}()
	reg.RegisterFunc("dup", func(context.Context, jobs.Job) error { return nil })
}

func TestRegistryRejectsEmptyTypeAndNilHandler(t *testing.T) {
	for name, fn := range map[string]func(){
		"empty type":  func() { NewRegistry().Register("", Registration{Handler: HandlerFunc(nil)}) },
		"nil handler": func() { NewRegistry().Register("x", Registration{}) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("the invalid registration was accepted")
				}
			}()
			fn()
		})
	}
}

func TestReaperRuns(t *testing.T) {
	q := newFakeQueue()
	rt, err := NewRuntime(fastConfig("w1"), q, NewRegistry(), discard())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	q.mu.Lock()
	reaps := q.reaps
	q.mu.Unlock()
	if reaps == 0 {
		t.Error("the reaper never ran, so a dead worker's jobs would stay leased forever")
	}
}
