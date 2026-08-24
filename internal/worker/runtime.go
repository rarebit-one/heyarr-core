package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// Handler runs one job type.
//
// A handler must be safe to re-run: the queue will re-run it (ADR-0008). It
// must also stop when ctx is cancelled — the context is tied to the lease, so
// cancellation means something else may already be running this work.
type Handler interface {
	Handle(ctx context.Context, job jobs.Job) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, job jobs.Job) error

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, job jobs.Job) error { return f(ctx, job) }

// Registration describes a handler and how it should be run.
type Registration struct {
	Handler Handler
	// MaxConcurrent caps how many of this type may run at once. Zero means the
	// pool's slot count is the only limit.
	//
	// This exists because job types have very different shapes: running sixteen
	// hashers against eight spinning disks is slower than running four, since
	// they contend for the same heads.
	MaxConcurrent int
	// RequiredCapability, when set, means this worker only claims the type if
	// it advertises the capability.
	RequiredCapability string
}

// Registry maps job types to handlers.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]Registration
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{entries: map[string]Registration{}} }

// Register adds a handler for a job type. Registering a type twice is a
// programming error and panics at wiring time rather than silently taking the
// last one — a job silently handled by the wrong code is far worse than a
// startup crash.
func (r *Registry) Register(jobType string, reg Registration) {
	if jobType == "" {
		panic("worker: cannot register a handler for an empty job type")
	}
	if reg.Handler == nil {
		panic("worker: cannot register a nil handler for " + jobType)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.entries[jobType]; exists {
		panic("worker: a handler is already registered for " + jobType)
	}
	r.entries[jobType] = reg
}

// RegisterFunc is Register for a plain function.
func (r *Registry) RegisterFunc(jobType string, fn HandlerFunc) {
	r.Register(jobType, Registration{Handler: fn})
}

// Lookup returns the registration for a job type.
func (r *Registry) Lookup(jobType string) (Registration, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	reg, ok := r.entries[jobType]
	return reg, ok
}

// Types lists every registered job type.
func (r *Registry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.entries))
	for t := range r.entries {
		out = append(out, t)
	}
	return out
}

// Queue is the subset of the job queue the runtime needs. Narrow on purpose, so
// the runtime can be tested without a database.
type Queue interface {
	Claim(ctx context.Context, opts jobs.ClaimOptions) (jobs.Job, error)
	Heartbeat(ctx context.Context, id, owner string, ttl time.Duration) error
	Complete(ctx context.Context, id, owner string) error
	Fail(ctx context.Context, id, owner string, cause error) error
	ReapExpiredLeases(ctx context.Context) (int, error)
}

// Config configures a Runtime.
type Config struct {
	// Owner identifies this worker in leases. Must be unique per process.
	Owner string
	// Slots is the maximum number of jobs running concurrently.
	Slots int
	// Capabilities this worker advertises (§75).
	Capabilities []string
	// LeaseTTL is how long a claimed lease is valid.
	LeaseTTL time.Duration
	// HeartbeatInterval is how often a held lease is renewed. It must be
	// comfortably shorter than LeaseTTL, or a slow renewal loses the lease.
	HeartbeatInterval time.Duration
	// PollInterval is how long to wait after finding no work.
	PollInterval time.Duration
	// ReapInterval is how often expired leases are returned to pending. Zero
	// disables reaping in this process.
	ReapInterval time.Duration
	// DrainTimeout bounds how long a graceful shutdown waits for in-flight
	// jobs. Beyond it, running handlers are cancelled.
	DrainTimeout time.Duration
}

// Defaults.
const (
	DefaultSlots             = 4
	DefaultLeaseTTL          = 60 * time.Second
	DefaultHeartbeatInterval = 15 * time.Second
	DefaultPollInterval      = 2 * time.Second
	DefaultReapInterval      = 30 * time.Second
	DefaultDrainTimeout      = 60 * time.Second
)

func (c *Config) applyDefaults() {
	if c.Slots <= 0 {
		c.Slots = DefaultSlots
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = DefaultLeaseTTL
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.ReapInterval == 0 {
		c.ReapInterval = DefaultReapInterval
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = DefaultDrainTimeout
	}
}

// Validate reports a configuration that would misbehave rather than fail.
func (c Config) Validate() error {
	if c.Owner == "" {
		return errors.New("worker: an owner is required — a lease must belong to someone")
	}
	// A heartbeat slower than the lease means the lease expires while the job
	// is still running, another worker claims it, and both run the same work.
	// That is silent duplicate execution, so refuse the configuration.
	if c.HeartbeatInterval >= c.LeaseTTL {
		return fmt.Errorf("worker: heartbeat interval %s must be shorter than the lease TTL %s, "+
			"or leases expire under running jobs and the work is run twice",
			c.HeartbeatInterval, c.LeaseTTL)
	}
	return nil
}

// Runtime claims jobs and runs them.
type Runtime struct {
	cfg      Config
	queue    Queue
	registry *Registry
	log      *slog.Logger

	mu      sync.Mutex
	running map[string]int // job type -> in-flight count

	// Observability. Tests assert on these rather than on timing.
	completed atomicCounter
	failed    atomicCounter
	panicked  atomicCounter

	storeUnwritable onceAlarm
}

// onceAlarm raises a fault that is not this job's fault exactly once.
//
// A store that cannot be written to will refuse the next job too. Reporting it
// per job turns one fault into as many failures as there are jobs — twelve
// identical "job failed" lines at INFO, in #151, for a single store-wide
// condition, with nothing saying they were all the same thing. The first one is
// raised at ERROR with the store's own diagnosis attached; the rest fail the
// way any job fails, so nothing is hidden and the alarm is not rung twelve
// times.
type onceAlarm struct{ raised atomic.Bool }

// shouldRaise reports whether cause is a store-wide fault being seen for the
// first time.
func (a *onceAlarm) shouldRaise(cause error) bool {
	return errors.Is(cause, cas.ErrStoreUnwritable) && a.raised.CompareAndSwap(false, true)
}

type atomicCounter struct {
	mu sync.Mutex
	n  int64
}

func (c *atomicCounter) inc() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *atomicCounter) load() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// NewRuntime constructs a Runtime.
func NewRuntime(cfg Config, queue Queue, registry *Registry, log *slog.Logger) (*Runtime, error) {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if queue == nil {
		return nil, errors.New("worker: a queue is required")
	}
	if registry == nil {
		return nil, errors.New("worker: a registry is required")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Runtime{
		cfg:      cfg,
		queue:    queue,
		registry: registry,
		log:      log.With("worker", cfg.Owner),
		running:  map[string]int{},
	}, nil
}

// Stats reports what this runtime has done.
type Stats struct {
	Completed int64
	Failed    int64
	Panicked  int64
}

// Stats returns the runtime's counters.
func (r *Runtime) Stats() Stats {
	return Stats{
		Completed: r.completed.load(),
		Failed:    r.failed.load(),
		Panicked:  r.panicked.load(),
	}
}

// Run claims and executes jobs until ctx is cancelled, then drains.
//
// Draining means: stop claiming, let in-flight jobs finish. A worker killed
// mid-hash otherwise costs a whole lease interval before the job is reclaimed,
// and re-does work that was nearly complete.
func (r *Runtime) Run(ctx context.Context) error {
	slots := make(chan struct{}, r.cfg.Slots)
	var wg sync.WaitGroup

	if r.cfg.ReapInterval > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.reapLoop(ctx)
		}()
	}

	// Deliberately not "worker started": the worker ROLE reports that the
	// moment it is alive and supervised, which is before the schema is ready
	// and therefore before this runtime exists. Two lines saying "started"
	// about different things is how a startup log stops being readable.
	// This line is load-bearing beyond readability: it is the only place the
	// capabilities this runtime will ACTUALLY claim with are observable, and
	// the acceptance demo asserts on it. It must report r.cfg — a field
	// re-derived from wherever the caller got the list is a log that agrees
	// with itself while the wiring between them is broken, which is a mistake
	// this repository has now made once.
	r.log.Info("worker runtime started",
		"slots", r.cfg.Slots,
		"capabilities", capabilitiesFor(r.cfg),
		"types", r.registry.Types())

claimLoop:
	for {
		select {
		case <-ctx.Done():
			break claimLoop
		case slots <- struct{}{}:
		}

		// An EMPTY type list means "no restriction" to the queue, not "nothing
		// is claimable" — so when every registered type is capped out, claiming
		// with an empty filter takes anything at all and blows straight through
		// the caps. Measured at 8 concurrent handlers against a cap of 2 before
		// this check existed.
		claimable := r.claimableTypes()
		if len(claimable) == 0 {
			<-slots
			if !r.sleep(ctx, r.cfg.PollInterval) {
				break claimLoop
			}
			continue
		}

		job, err := r.queue.Claim(ctx, jobs.ClaimOptions{
			Owner:        r.cfg.Owner,
			Capabilities: r.cfg.Capabilities,
			LeaseTTL:     r.cfg.LeaseTTL,
			Types:        claimable,
		})
		if err != nil {
			<-slots
			if errors.Is(err, jobs.ErrNoWork) || errors.Is(err, context.Canceled) {
				if !r.sleep(ctx, r.cfg.PollInterval) {
					break claimLoop
				}
				continue
			}
			r.log.Error("claiming work failed", "error", err)
			if !r.sleep(ctx, r.cfg.PollInterval) {
				break claimLoop
			}
			continue
		}

		// Reserve the per-type slot HERE, not in the handler goroutine. This
		// loop is single-threaded, so checking the cap in claimableTypes and
		// incrementing the counter happen with nothing in between. Doing the
		// increment inside the goroutine leaves a window in which several
		// claims all see the same free slot — measured at 8 concurrent
		// handlers against a cap of 2 before this moved.
		r.track(job.Type, 1)

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer r.track(job.Type, -1)
			r.execute(ctx, job)
		}()
	}

	// Drain. In-flight handlers keep their own contexts, which are cancelled
	// only if they overrun the drain timeout.
	r.log.Info("worker draining", "timeout", r.cfg.DrainTimeout)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		r.log.Info("worker stopped", "stats", r.Stats())
		return nil
	case <-time.After(r.cfg.DrainTimeout):
		r.log.Warn("drain timed out; in-flight jobs were cancelled",
			"timeout", r.cfg.DrainTimeout, "stats", r.Stats())
		return fmt.Errorf("worker: drain did not complete within %s", r.cfg.DrainTimeout)
	}
}

// claimableTypes is the registered types this worker may run, honouring
// per-type capability requirements.
func (r *Runtime) claimableTypes() []string {
	var out []string
	for _, t := range r.registry.Types() {
		reg, ok := r.registry.Lookup(t)
		if !ok {
			continue
		}
		if reg.RequiredCapability != "" && !r.hasCapability(reg.RequiredCapability) {
			continue
		}
		if !r.hasFreeSlotFor(t, reg) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// capabilitiesFor renders a runtime's capabilities for logging, as [] rather
// than null when there are none. "Advertises nothing" is a deliberate, normal
// state — a worker with no FFmpeg (ADR-0023) — and null reads as "never set".
// Someone reading this log is usually reading it because work is not being
// claimed, which is exactly when that distinction matters.
func capabilitiesFor(cfg Config) []string {
	if cfg.Capabilities == nil {
		return []string{}
	}
	return cfg.Capabilities
}

func (r *Runtime) hasCapability(c string) bool {
	for _, have := range r.cfg.Capabilities {
		if have == c {
			return true
		}
	}
	return false
}

func (r *Runtime) hasFreeSlotFor(jobType string, reg Registration) bool {
	if reg.MaxConcurrent <= 0 {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running[jobType] < reg.MaxConcurrent
}

func (r *Runtime) track(jobType string, delta int) {
	r.mu.Lock()
	r.running[jobType] += delta
	if r.running[jobType] <= 0 {
		delete(r.running, jobType)
	}
	r.mu.Unlock()
}

// InFlight reports how many jobs of each type are running.
func (r *Runtime) InFlight() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.running))
	for k, v := range r.running {
		out[k] = v
	}
	return out
}

// execute runs one job with a lease-linked context and a heartbeat.
func (r *Runtime) execute(parent context.Context, job jobs.Job) {
	reg, ok := r.registry.Lookup(job.Type)
	if !ok {
		// An unregistered type must fail the job clearly rather than crash the
		// worker: one bad enqueue should not take down everything else.
		err := fmt.Errorf("worker: no handler is registered for job type %q", job.Type)
		r.log.Error("unhandled job type", "job", job.ID, "type", job.Type)
		r.failJob(parent, job, err)
		return
	}

	// The handler's context is deliberately NOT the shutdown context: draining
	// means letting in-flight work finish. It is cancelled when the lease is
	// lost, because then something else may already be running this job.
	jobCtx, cancel := context.WithCancel(context.WithoutCancel(parent))
	defer cancel()

	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		r.heartbeat(jobCtx, cancel, job)
	}()

	err := r.runHandler(jobCtx, reg.Handler, job)

	cancel()
	<-heartbeatDone

	if err != nil {
		r.failJob(parent, job, err)
		return
	}
	if err := r.queue.Complete(context.WithoutCancel(parent), job.ID, r.cfg.Owner); err != nil {
		// Losing the lease at completion means the work was done twice, or is
		// about to be. Worth a loud log: it means a heartbeat was too slow.
		r.log.Error("could not mark the job complete", "job", job.ID, "error", err)
		return
	}
	r.completed.inc()
	r.log.Debug("job completed", "job", job.ID, "type", job.Type)
}

// runHandler runs a handler, converting a panic into a failed job.
//
// A panicking handler must not take the process down: one bad job type would
// otherwise stop every other kind of work in the system.
func (r *Runtime) runHandler(ctx context.Context, h Handler, job jobs.Job) (err error) {
	defer func() {
		if p := recover(); p != nil {
			r.panicked.inc()
			err = fmt.Errorf("worker: handler for %s panicked: %v", job.Type, p)
			r.log.Error("handler panicked", "job", job.ID, "type", job.Type, "panic", p)
		}
	}()
	return h.Handle(ctx, job)
}

// heartbeat renews the lease until the job finishes. Losing the lease cancels
// the job's context, so the handler stops rather than racing whoever claimed it
// next (ADR-0008).
func (r *Runtime) heartbeat(ctx context.Context, cancel context.CancelFunc, job jobs.Job) {
	ticker := time.NewTicker(r.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := r.queue.Heartbeat(context.WithoutCancel(ctx), job.ID, r.cfg.Owner, r.cfg.LeaseTTL)
			if err == nil {
				continue
			}
			if errors.Is(err, jobs.ErrLeaseLost) {
				r.log.Warn("lease lost; cancelling the running job",
					"job", job.ID, "type", job.Type)
				cancel()
				return
			}
			// A transient database error is not proof the lease is gone, so
			// keep going and let expiry decide.
			r.log.Warn("heartbeat failed", "job", job.ID, "error", err)
		}
	}
}

func (r *Runtime) failJob(ctx context.Context, job jobs.Job, cause error) {
	r.failed.inc()
	if err := r.queue.Fail(context.WithoutCancel(ctx), job.ID, r.cfg.Owner, cause); err != nil {
		r.log.Error("could not record the job failure", "job", job.ID, "error", err)
		return
	}
	if r.storeUnwritable.shouldRaise(cause) {
		// Deliberately not a per-job failure. Every job that touches the store
		// will fail this way until an operator changes something, so this is
		// said once, loudly, with the evidence the store gathered (#151).
		r.log.Error("the content-addressed store cannot be written to; every job that writes to it will fail until this is fixed",
			"job", job.ID, "type", job.Type, "cause", cause)
		return
	}
	r.log.Info("job failed", "job", job.ID, "type", job.Type, "cause", cause)
}

// reapLoop returns expired leases to pending, so a worker that died costs one
// lease interval rather than a stuck queue.
func (r *Runtime) reapLoop(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.ReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := r.queue.ReapExpiredLeases(context.WithoutCancel(ctx))
			if err != nil {
				r.log.Warn("reaping expired leases failed", "error", err)
				continue
			}
			if n > 0 {
				r.log.Info("returned expired leases to pending", "count", n)
			}
		}
	}
}

// sleep waits for d, or returns false if the context is cancelled first.
func (r *Runtime) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
