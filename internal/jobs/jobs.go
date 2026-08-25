// Package jobs is the durable, leased, retryable, capability-routed job queue
// (spec §75).
//
// There is exactly one job system, shared by every content type and every
// plane: §61 names "content-specific job systems" as an *arr constraint to
// avoid. Every later milestone should be "register a new job type" rather than
// "build another scheduler" (ADR-0008).
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/events"
)

// State is where a job is in its life.
type State string

const (
	// Pending means the job is claimable once run_after has passed.
	Pending State = "pending"
	// Leased means a worker holds the job and is heartbeating.
	Leased State = "leased"
	// Succeeded means the job finished.
	Succeeded State = "succeeded"
	// Failed means this attempt failed; it will be retried after a backoff.
	Failed State = "failed"
	// Dead means attempts are exhausted. Terminal, and deliberately not
	// retried forever — a job that cannot succeed should stop consuming a
	// worker slot and start being visible instead.
	Dead State = "dead"
)

// Job is one unit of durable work.
type Job struct {
	ID                 string
	Type               string
	Payload            json.RawMessage
	State              State
	Priority           int
	DedupeKey          string
	RequiredCapability string
	RunAfter           time.Time
	Attempts           int
	MaxAttempts        int
	LeaseOwner         string
	LeaseExpiresAt     time.Time
	LastError          string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	FinishedAt         time.Time
}

// Clock is injected everywhere time is read, so lease expiry and backoff are
// ordinary unit tests rather than sleeps (ADR-0017).
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// Errors returned by the queue.
var (
	// ErrNotFound means no job with that id exists.
	ErrNotFound = errors.New("jobs: not found")
	// ErrNoWork means nothing was claimable. It is an ordinary outcome, not a
	// failure — a worker polling an idle queue gets it constantly.
	ErrNoWork = errors.New("jobs: no claimable work")
	// ErrLeaseLost means the job is no longer held by this owner. A handler
	// that sees it must stop: something else may already be running the work.
	ErrLeaseLost = errors.New("jobs: lease lost")
	// ErrPermanent is a handler saying this work will fail identically however
	// many times it is retried. Wrapping it sends the job straight to dead.
	//
	// # Why the queue needed one at all
	//
	// Until this there was exactly ONE route to dead — attempts exhausted — so
	// a handler that already knew the answer had no way to say so, and the
	// queue spent four more attempts rediscovering it.
	//
	// The cost is not the retries. It is what the retries DO: a probe of a blob
	// that is not media range-probes, gives up past §29's threshold, and then
	// MATERIALISES THE WHOLE BLOB — five times, for a fact established the
	// first time. On a corrupt 60 GB remux that is five whole-blob
	// materialisations to relearn that ffprobe does not recognise it (#232).
	//
	// # What may and may not use it
	//
	// Only a failure that is a property of the INPUT, where the input cannot
	// change. Bytes are immutable and addressed by their digest (invariant 1),
	// so "ffprobe read these bytes and they are not a format it knows" cannot
	// become untrue. A missing binary, a lost lease, a store that is briefly
	// unavailable and an I/O error are all the opposite — they are properties
	// of the moment, and retrying them is exactly right.
	//
	// Getting that wrong is worse than the retries were: a transient failure
	// declared permanent is work that silently never happens. When in doubt,
	// do not wrap.
	//
	// It does NOT bypass the handler's own idempotence (invariant 9). A
	// permanently-failed job can still be retried by hand — `jobs retry` moves
	// dead back to pending — because an operator who has fixed the world is
	// making a different claim than the handler was.
	ErrPermanent = errors.New("jobs: this will fail the same way every time")
)

// Defaults.
const (
	DefaultMaxAttempts  = 5
	DefaultPriority     = 100
	DefaultLeaseTTL     = 60 * time.Second
	defaultBaseBackoff  = 2 * time.Second
	defaultMaxBackoff   = 15 * time.Minute
	timeFormat          = time.RFC3339Nano
	claimableSelectCols = `id, type, payload, state, priority, coalesce(dedupe_key,''),
		required_capability, run_after, attempts, max_attempts,
		coalesce(lease_owner,''), coalesce(lease_expires_at,''),
		coalesce(last_error,''), created_at, updated_at, coalesce(finished_at,'')`
)

// Queue is the durable job queue backed by the controller database.
type Queue struct {
	writer *sql.DB
	reader *sql.DB
	clock  Clock
	rand   *rand.Rand
	events *events.Log
}

// Options configure a Queue.
type Options struct {
	// Writer must be the single-writer pool (ADR-0003): claiming is a write,
	// and funnelling it through one connection turns contention into a queue in
	// Go rather than SQLITE_BUSY returned to whichever worker lost.
	Writer *sql.DB
	// Reader is used for inspection queries that must not block a claim.
	Reader *sql.DB
	Clock  Clock
	// Rand makes backoff jitter reproducible in tests. Nil means seeded.
	Rand *rand.Rand
	// Events records the queue's state transitions. Required, not optional:
	// every state transition emits an event, with no exceptions, and an
	// exception that a caller can create by leaving a field nil is still an
	// exception (§76, ADR-0009).
	Events *events.Log
}

// New constructs a Queue.
func New(opts Options) (*Queue, error) {
	if opts.Writer == nil {
		return nil, errors.New("jobs: a writer database is required")
	}
	if opts.Events == nil {
		return nil, errors.New("jobs: an event log is required — every state transition emits an event (ADR-0009)")
	}
	reader := opts.Reader
	if reader == nil {
		reader = opts.Writer
	}
	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}
	r := opts.Rand
	if r == nil {
		r = rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0x9E3779B97F4A7C15)) // #nosec G404 -- jitter, not security
	}
	return &Queue{writer: opts.Writer, reader: reader, clock: clock, rand: r, events: opts.Events}, nil
}

// inTx runs fn inside a write transaction and publishes whatever it recorded
// once the transaction commits.
//
// Both halves matter. The state change and the event it describes go in one
// transaction, or the log can disagree with the table. And the events fan out
// only AFTER the commit, because a subscriber that acts on a transition which
// then rolls back has acted on something that did not happen.
//
// Note the events are appended with EmitTx, never Emit: the writer pool is a
// single connection and the transaction holds the write lock, so a nested Emit
// would wait for what its own caller is holding and hang until the context
// expired.
func (q *Queue) inTx(ctx context.Context, fn func(tx *sql.Tx) ([]events.Event, error)) error {
	tx, err := q.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("jobs: beginning transaction: %w", err)
	}
	recorded, err := fn(tx)
	if err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("jobs: rolling back: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("jobs: committing: %w", err)
	}
	q.events.Publish(recorded...)
	return nil
}

// transition payloads carry everything a consumer needs to decide what to do
// without reading the job back — which a client watching a stream cannot
// cheaply do, and which would race the next transition anyway.
func transitionPayload(job Job, extra map[string]any) map[string]any {
	out := map[string]any{
		"type":         job.Type,
		"state":        string(job.State),
		"attempts":     job.Attempts,
		"max_attempts": job.MaxAttempts,
	}
	if job.DedupeKey != "" {
		out["dedupe_key"] = job.DedupeKey
	}
	if job.LastError != "" {
		out["error"] = job.LastError
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// EnqueueOptions describe work to be done.
type EnqueueOptions struct {
	Type    string
	Payload any
	// DedupeKey makes Enqueue idempotent while an equivalent job is live.
	DedupeKey string
	// Priority orders claimable work; lower runs first.
	Priority *int
	// RequiredCapability routes the job to workers that advertise it.
	RequiredCapability string
	// RunAfter delays the job. Zero means immediately.
	RunAfter    time.Time
	MaxAttempts int
}

// Enqueue adds a job, or returns the existing one when an equivalent is already
// live under the same dedupe key.
//
// Returning the existing job rather than an error is deliberate: the caller
// asked for work to happen, and it is going to happen. A scanner enqueueing an
// ingest for a file that is already queued wants "fine", not an error to
// handle.
func (q *Queue) Enqueue(ctx context.Context, opts EnqueueOptions) (Job, error) {
	if opts.Type == "" {
		return Job{}, errors.New("jobs: type must be set")
	}
	payload := json.RawMessage("{}")
	if opts.Payload != nil {
		encoded, err := json.Marshal(opts.Payload)
		if err != nil {
			return Job{}, fmt.Errorf("jobs: encoding payload: %w", err)
		}
		payload = encoded
	}

	now := q.clock.Now()
	runAfter := opts.RunAfter
	if runAfter.IsZero() {
		runAfter = now
	}
	priority := DefaultPriority
	if opts.Priority != nil {
		priority = *opts.Priority
	}
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}

	job := Job{
		ID:                 uuid.Must(uuid.NewV7()).String(),
		Type:               opts.Type,
		Payload:            payload,
		State:              Pending,
		Priority:           priority,
		DedupeKey:          opts.DedupeKey,
		RequiredCapability: opts.RequiredCapability,
		RunAfter:           runAfter,
		MaxAttempts:        maxAttempts,
		CreatedAt:          now,
		UpdatedAt:          now,
	}

	err := q.inTx(ctx, func(tx *sql.Tx) ([]events.Event, error) {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO jobs (id, type, payload, state, priority, dedupe_key,
				required_capability, run_after, attempts, max_attempts, created_at, updated_at)
			VALUES (?, ?, ?, 'pending', ?, ?, ?, ?, 0, ?, ?, ?)`,
			job.ID, job.Type, string(job.Payload), job.Priority, nullable(job.DedupeKey),
			job.RequiredCapability, format(job.RunAfter), job.MaxAttempts,
			format(job.CreatedAt), format(job.UpdatedAt)); err != nil {
			return nil, err
		}
		e, err := q.events.EmitTx(ctx, tx, events.TypeJobEnqueued, "job", job.ID, transitionPayload(job, nil))
		if err != nil {
			return nil, err
		}
		return []events.Event{e}, nil
	})
	if err != nil {
		// A unique-index violation means an equivalent job is already live.
		if opts.DedupeKey != "" {
			existing, findErr := q.findLiveByDedupeKey(ctx, opts.DedupeKey)
			if findErr == nil {
				return existing, nil
			}
		}
		return Job{}, fmt.Errorf("jobs: enqueueing %s: %w", opts.Type, err)
	}
	return job, nil
}

// LiveDedupeKeys returns the dedupe keys of every live — pending or leased —
// job of one type.
//
// # Why a reader needs this at all, when the unique index already exists
//
// It does not need it for correctness. The partial-unique index over live
// states (ADR-0008) is what makes a re-enqueue harmless, and that stays the
// guarantee: a caller that skipped this and simply enqueued again would create
// nothing.
//
// It needs it for BOOKKEEPING, and specifically for BOUNDED work. A producer
// that emits at most N jobs per cycle has to know which of its candidates are
// already in flight, or it re-offers the same first N every cycle and the
// remainder is never reached — the bound would turn a converging system into
// one that makes progress only up to its own limit. Counting a dedupe hit as
// "enqueued" has the same effect and is harder to see.
//
// Live only. A succeeded job's key is free by design — that is what lets the
// same work be requested again once it is done, and it is why the index is
// partial rather than total.
func (q *Queue) LiveDedupeKeys(ctx context.Context, jobType string) (map[string]struct{}, error) {
	rows, err := q.reader.QueryContext(ctx,
		`SELECT dedupe_key FROM jobs
		  WHERE type = ? AND dedupe_key IS NOT NULL AND state IN ('pending','leased')`, jobType)
	if err != nil {
		return nil, fmt.Errorf("jobs: reading the live %s keys: %w", jobType, err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string]struct{}{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("jobs: reading the live %s keys: %w", jobType, err)
		}
		out[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jobs: reading the live %s keys: %w", jobType, err)
	}
	return out, nil
}

func (q *Queue) findLiveByDedupeKey(ctx context.Context, key string) (Job, error) {
	row := q.reader.QueryRowContext(ctx, `SELECT `+claimableSelectCols+`
		FROM jobs WHERE dedupe_key = ? AND state IN ('pending','leased')`, key)
	return scanJob(row)
}

// ClaimOptions describe a worker asking for work.
type ClaimOptions struct {
	// Owner identifies the worker holding the lease.
	Owner string
	// Capabilities are what this worker can run. A job requiring a capability
	// the worker lacks is never claimed by it.
	Capabilities []string
	// LeaseTTL is how long the lease is valid before the reaper may take it
	// back. Zero means DefaultLeaseTTL.
	LeaseTTL time.Duration
	// Types, when non-empty, restricts the claim to these job types.
	Types []string
}

// Claim takes the highest-priority due job this worker can run, in one
// statement.
//
// Doing the select and the update as a single UPDATE ... RETURNING is what
// makes concurrent claims safe: there is no window between choosing a job and
// marking it taken, so two workers cannot both believe they own it.
func (q *Queue) Claim(ctx context.Context, opts ClaimOptions) (Job, error) {
	if opts.Owner == "" {
		return Job{}, errors.New("jobs: an owner is required to claim work")
	}
	ttl := opts.LeaseTTL
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	now := q.clock.Now()
	expires := now.Add(ttl)

	// A worker always accepts jobs with no capability requirement, plus the
	// ones it advertises.
	capPlaceholders, capArgs := inClause(append([]string{""}, opts.Capabilities...))
	typeFilter := ""
	var typeArgs []any
	if len(opts.Types) > 0 {
		placeholders, args := inClause(opts.Types)
		typeFilter = " AND type IN (" + placeholders + ")"
		typeArgs = args
	}

	query := `
		UPDATE jobs
		SET state = 'leased', lease_owner = ?, lease_expires_at = ?,
		    attempts = attempts + 1, updated_at = ?
		WHERE id = (
			SELECT id FROM jobs
			WHERE state = 'pending'
			  AND run_after <= ?
			  AND required_capability IN (` + capPlaceholders + `)` + typeFilter + `
			ORDER BY priority ASC, run_after ASC
			LIMIT 1
		)
		RETURNING ` + claimableSelectCols

	args := []any{opts.Owner, format(expires), format(now), format(now)}
	args = append(args, capArgs...)
	args = append(args, typeArgs...)

	job, err := scanJob(q.writer.QueryRowContext(ctx, query, args...))
	if errors.Is(err, ErrNotFound) {
		return Job{}, ErrNoWork
	}
	if err != nil {
		return Job{}, fmt.Errorf("jobs: claiming: %w", err)
	}
	return job, nil
}

// Heartbeat extends a lease. It fails with ErrLeaseLost if the job is no longer
// held by this owner — which is the signal for the handler to stop, because
// something else may already be running the work.
func (q *Queue) Heartbeat(ctx context.Context, id, owner string, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	now := q.clock.Now()
	res, err := q.writer.ExecContext(ctx, `
		UPDATE jobs SET lease_expires_at = ?, updated_at = ?
		WHERE id = ? AND lease_owner = ? AND state = 'leased'`,
		format(now.Add(ttl)), format(now), id, owner)
	if err != nil {
		return fmt.Errorf("jobs: heartbeating %s: %w", id, err)
	}
	return expectOneRow(res, id)
}

// Complete marks a job succeeded.
func (q *Queue) Complete(ctx context.Context, id, owner string) error {
	now := q.clock.Now()
	return q.inTx(ctx, func(tx *sql.Tx) ([]events.Event, error) {
		res, err := tx.ExecContext(ctx, `
			UPDATE jobs SET state = 'succeeded', lease_owner = NULL, lease_expires_at = NULL,
				finished_at = ?, updated_at = ?, last_error = NULL
			WHERE id = ? AND lease_owner = ? AND state = 'leased'`,
			format(now), format(now), id, owner)
		if err != nil {
			return nil, fmt.Errorf("jobs: completing %s: %w", id, err)
		}
		if err := expectOneRow(res, id); err != nil {
			return nil, err
		}
		job, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+claimableSelectCols+` FROM jobs WHERE id = ?`, id))
		if err != nil {
			return nil, err
		}
		e, err := q.events.EmitTx(ctx, tx, events.TypeJobSucceeded, "job", id, transitionPayload(job, nil))
		if err != nil {
			return nil, err
		}
		return []events.Event{e}, nil
	})
}

// Fail records a failed attempt. The job is rescheduled with backoff, or moved
// to dead once attempts are exhausted.
//
// Dead is terminal on purpose: a job that cannot succeed should stop consuming
// worker slots and start being visible instead.
func (q *Queue) Fail(ctx context.Context, id, owner string, cause error) error {
	now := q.clock.Now()

	job, err := q.Get(ctx, id)
	if err != nil {
		return err
	}
	message := ""
	if cause != nil {
		message = cause.Error()
	}

	// A handler that says this will fail identically forever is believed, and
	// the remaining attempts are not spent proving it — see ErrPermanent.
	permanent := errors.Is(cause, ErrPermanent)

	if permanent || job.Attempts >= job.MaxAttempts {
		return q.inTx(ctx, func(tx *sql.Tx) ([]events.Event, error) {
			res, err := tx.ExecContext(ctx, `
				UPDATE jobs SET state = 'dead', lease_owner = NULL, lease_expires_at = NULL,
					last_error = ?, finished_at = ?, updated_at = ?
				WHERE id = ? AND lease_owner = ? AND state = 'leased'`,
				message, format(now), format(now), id, owner)
			if err != nil {
				return nil, fmt.Errorf("jobs: marking %s dead: %w", id, err)
			}
			if err := expectOneRow(res, id); err != nil {
				return nil, err
			}
			job.State, job.LastError = Dead, message
			// Terminal, and the flag says so without a consumer having to know
			// that dead is the state attempts run out into. `heyarr scan
			// --wait` branches on exactly this.
			// `permanent` is carried BESIDE `terminal` rather than instead of
			// it. Both are true, and they answer different questions: terminal
			// is "this job is finished", permanent is "retrying it would not
			// have helped". An operator deciding whether to `jobs retry` needs
			// the second, and a consumer branching on completion needs the
			// first — `heyarr scan --wait` reads exactly that.
			e, err := q.events.EmitTx(ctx, tx, events.TypeJobFailed, "job", id,
				transitionPayload(job, map[string]any{
					"terminal": true, "permanent": permanent,
				}))
			if err != nil {
				return nil, err
			}
			return []events.Event{e}, nil
		})
	}

	retryAt := now.Add(q.backoff(job.Attempts))
	return q.inTx(ctx, func(tx *sql.Tx) ([]events.Event, error) {
		res, err := tx.ExecContext(ctx, `
			UPDATE jobs SET state = 'pending', lease_owner = NULL, lease_expires_at = NULL,
				run_after = ?, last_error = ?, updated_at = ?
			WHERE id = ? AND lease_owner = ? AND state = 'leased'`,
			format(retryAt), message, format(now), id, owner)
		if err != nil {
			return nil, fmt.Errorf("jobs: rescheduling %s: %w", id, err)
		}
		if err := expectOneRow(res, id); err != nil {
			return nil, err
		}
		job.State, job.LastError = Pending, message
		// A spent attempt, not an outcome: the job is going to run again, and a
		// client that treated this as failure would give up early.
		e, err := q.events.EmitTx(ctx, tx, events.TypeJobFailed, "job", id,
			transitionPayload(job, map[string]any{"terminal": false, "run_after": format(retryAt)}))
		if err != nil {
			return nil, err
		}
		return []events.Event{e}, nil
	})
}

// backoff is exponential with full jitter.
//
// Full jitter rather than a fixed schedule because the failure that matters is
// correlated: a provider going down fails every queued job at once, and without
// jitter they all retry in lockstep and hammer it back down the moment it
// recovers.
func (q *Queue) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := defaultBaseBackoff << min(attempt-1, 20)
	if d > defaultMaxBackoff || d <= 0 {
		d = defaultMaxBackoff
	}
	return time.Duration(q.rand.Int64N(int64(d)) + int64(defaultBaseBackoff))
}

// ReapExpiredLeases returns jobs whose lease expired to pending, so a worker
// that died mid-job costs one lease interval rather than a stuck queue.
//
// The attempt was already counted at claim time, so a worker that dies
// repeatedly still walks toward dead rather than retrying forever.
func (q *Queue) ReapExpiredLeases(ctx context.Context) (int, error) {
	now := q.clock.Now()
	const reason = "lease expired; the worker holding this job stopped reporting"

	// The ids are read first so each reaped job can be reported individually.
	// A bulk UPDATE that returned only a count would leave "your job went back
	// to pending because the worker running it died" as something a client
	// could only discover by polling — which is the integration model §61 names
	// as an *arr failure.
	reaped := 0
	err := q.inTx(ctx, func(tx *sql.Tx) ([]events.Event, error) {
		ids, err := expiredLeaseIDs(ctx, tx, format(now))
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return nil, nil
		}

		out := make([]events.Event, 0, len(ids))
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `
				UPDATE jobs SET state = 'pending', lease_owner = NULL, lease_expires_at = NULL,
					last_error = ?, updated_at = ?
				WHERE id = ? AND state = 'leased'`, reason, format(now), id); err != nil {
				return nil, fmt.Errorf("jobs: reaping %s: %w", id, err)
			}
			job, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+claimableSelectCols+` FROM jobs WHERE id = ?`, id))
			if err != nil {
				return nil, err
			}
			e, err := q.events.EmitTx(ctx, tx, events.TypeJobFailed, "job", id,
				transitionPayload(job, map[string]any{"terminal": false, "reaped": true}))
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		reaped = len(ids)
		return out, nil
	})
	if err != nil {
		return 0, err
	}
	return reaped, nil
}

// ReclaimAllLeases returns EVERY leased job to pending, unconditionally, and
// reports how many.
//
// It is for disaster recovery (`recover --from-peer`, M7-04), not for ordinary
// running. A control database restored from a dead node carries leases whose
// expiry is still in the FUTURE by the dead worker's clock, so
// [Queue.ReapExpiredLeases] — which only reclaims leases already past their
// expiry — would leave them looking live, and the restored node would believe
// jobs were running on a worker that no longer exists. §75/ADR-0008 makes a job
// durable, leased and idempotent, and a restore is the extreme case of "it will
// be re-run": on the restored node nothing is leased until a live worker claims
// it afresh.
func (q *Queue) ReclaimAllLeases(ctx context.Context) (int, error) {
	now := q.clock.Now()
	const reason = "control plane restored; leases from the previous node are void"
	reclaimed := 0
	err := q.inTx(ctx, func(tx *sql.Tx) ([]events.Event, error) {
		ids, err := allLeasedIDs(ctx, tx)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return nil, nil
		}
		out := make([]events.Event, 0, len(ids))
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `
				UPDATE jobs SET state = 'pending', lease_owner = NULL, lease_expires_at = NULL,
					last_error = ?, updated_at = ?
				WHERE id = ? AND state = 'leased'`, reason, format(now), id); err != nil {
				return nil, fmt.Errorf("jobs: reclaiming %s: %w", id, err)
			}
			job, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+claimableSelectCols+` FROM jobs WHERE id = ?`, id))
			if err != nil {
				return nil, err
			}
			e, err := q.events.EmitTx(ctx, tx, events.TypeJobFailed, "job", id,
				transitionPayload(job, map[string]any{"terminal": false, "reclaimed": true}))
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		reclaimed = len(ids)
		return out, nil
	})
	if err != nil {
		return 0, err
	}
	return reclaimed, nil
}

// allLeasedIDs collects every leased job, for [Queue.ReclaimAllLeases].
func allLeasedIDs(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM jobs WHERE state = 'leased'`)
	if err != nil {
		return nil, fmt.Errorf("jobs: finding leased jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// expiredLeaseIDs collects the jobs whose leases have run out. It is its own
// function so the rows can be closed with defer on every path, including the
// scan error one.
func expiredLeaseIDs(ctx context.Context, tx *sql.Tx, now string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM jobs WHERE state = 'leased' AND lease_expires_at <= ?`, now)
	if err != nil {
		return nil, fmt.Errorf("jobs: finding expired leases: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("jobs: reading expired leases: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jobs: reading expired leases: %w", err)
	}
	return ids, nil
}

// Get returns one job.
func (q *Queue) Get(ctx context.Context, id string) (Job, error) {
	return scanJob(q.reader.QueryRowContext(ctx, `SELECT `+claimableSelectCols+` FROM jobs WHERE id = ?`, id))
}

// Retry moves a dead or failed job back to pending, resetting its attempts.
// This is an operator action: something was wrong and has been fixed.
func (q *Queue) Retry(ctx context.Context, id string) error {
	now := q.clock.Now()
	return q.inTx(ctx, func(tx *sql.Tx) ([]events.Event, error) {
		res, err := tx.ExecContext(ctx, `
			UPDATE jobs SET state = 'pending', attempts = 0, run_after = ?,
				lease_owner = NULL, lease_expires_at = NULL, finished_at = NULL, updated_at = ?
			WHERE id = ? AND state IN ('dead', 'failed', 'succeeded')`,
			format(now), format(now), id)
		if err != nil {
			return nil, fmt.Errorf("jobs: retrying %s: %w", id, err)
		}
		if err := expectOneRow(res, id); err != nil {
			return nil, err
		}
		job, err := scanJob(tx.QueryRowContext(ctx, `SELECT `+claimableSelectCols+` FROM jobs WHERE id = ?`, id))
		if err != nil {
			return nil, err
		}
		e, err := q.events.EmitTx(ctx, tx, events.TypeJobEnqueued, "job", id,
			transitionPayload(job, map[string]any{"retried": true}))
		if err != nil {
			return nil, err
		}
		return []events.Event{e}, nil
	})
}

// Stats counts jobs by state, for operational visibility (§60).
func (q *Queue) Stats(ctx context.Context) (map[State]int, error) {
	rows, err := q.reader.QueryContext(ctx, `SELECT state, count(*) FROM jobs GROUP BY state`)
	if err != nil {
		return nil, fmt.Errorf("jobs: counting: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[State]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, fmt.Errorf("jobs: counting: %w", err)
		}
		out[State(state)] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jobs: counting: %w", err)
	}
	return out, nil
}

// --- helpers ---

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (Job, error) {
	var (
		j                                          Job
		payload                                    string
		runAfter, createdAt, updatedAt, finishedAt string
		leaseExpires                               string
		state                                      string
	)
	err := row.Scan(&j.ID, &j.Type, &payload, &state, &j.Priority, &j.DedupeKey,
		&j.RequiredCapability, &runAfter, &j.Attempts, &j.MaxAttempts,
		&j.LeaseOwner, &leaseExpires, &j.LastError, &createdAt, &updatedAt, &finishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	j.State = State(state)
	j.Payload = json.RawMessage(payload)
	j.RunAfter = parse(runAfter)
	j.LeaseExpiresAt = parse(leaseExpires)
	j.CreatedAt = parse(createdAt)
	j.UpdatedAt = parse(updatedAt)
	j.FinishedAt = parse(finishedAt)
	return j, nil
}

func expectOneRow(res sql.Result, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("jobs: %s: %w", id, err)
	}
	if n == 0 {
		// Either the job is gone or this owner no longer holds it. Both mean
		// the caller must stop; conflating them is safe because the required
		// action is identical.
		return fmt.Errorf("%w: %s", ErrLeaseLost, id)
	}
	return nil
}

func inClause(values []string) (string, []any) {
	if len(values) == 0 {
		return "''", nil
	}
	placeholders := make([]byte, 0, len(values)*2)
	args := make([]any, 0, len(values))
	for i, v := range values {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args = append(args, v)
	}
	return string(placeholders), args
}

func format(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeFormat)
}

func parse(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(timeFormat, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
