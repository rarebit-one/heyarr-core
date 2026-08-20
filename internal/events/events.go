// Package events is the append-only event log and its fan-out (spec §76).
//
// Events ship in Milestone 1 deliberately (ADR-0009). §61 names "polling as the
// only integration model" as an *arr failure, but the practical argument is
// narrower: retrofitting events means auditing every mutation site, and that
// audit gets more expensive with every milestone.
package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Event namespaces from §76. Milestone 1 uses this subset.
const (
	TypeBlobCreated       = "blob.created"
	TypeReplicaPresent    = "replica.present"
	TypeReplicaCorrupt    = "replica.corrupt"
	TypeIngestCompleted   = "ingest.completed"
	TypeAssetCreated      = "content.asset.created"
	TypeAssetMissing      = "content.asset.missing"
	TypeJobEnqueued       = "job.enqueued"
	TypeJobSucceeded      = "job.succeeded"
	TypeJobFailed         = "job.failed"
	TypeScanProgress      = "system.scan.progress"
	TypeSystemStarted     = "system.started"
	TypeSystemStopped     = "system.stopped"
	TypePeerRegistered    = "peer.registered"
	TypeDesiredSatisfied  = "desired.satisfied"
	TypePrivateStateHeads = "private_state.heads"
)

const timeFormat = time.RFC3339Nano

// Event is one recorded state transition.
type Event struct {
	Seq         int64           `json:"seq"`
	ID          string          `json:"id"`
	Type        string          `json:"type"`
	SubjectType string          `json:"subject_type,omitempty"`
	SubjectID   string          `json:"subject_id,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

// Clock is injected so ordering assertions do not depend on wall time.
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// Log appends events durably and fans them out to live subscribers.
//
// Durability first, then fan-out: a subscriber must never see an event that is
// not in the log, because it may act on it and then the log is the record of
// what happened.
type Log struct {
	writer *sql.DB
	reader *sql.DB
	clock  Clock
	log    *slog.Logger

	mu     sync.RWMutex
	subs   map[int64]*Subscription
	nextID int64
}

// Options configure a Log.
type Options struct {
	Writer *sql.DB
	Reader *sql.DB
	Clock  Clock
	Logger *slog.Logger
}

// New constructs a Log.
func New(opts Options) (*Log, error) {
	if opts.Writer == nil {
		return nil, errors.New("events: a writer database is required")
	}
	reader := opts.Reader
	if reader == nil {
		reader = opts.Writer
	}
	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Log{
		writer: opts.Writer,
		reader: reader,
		clock:  clock,
		log:    logger,
		subs:   map[int64]*Subscription{},
	}, nil
}

// rowQuerier is the one method appending an event needs, so the same code can
// run on the writer pool or inside a caller's transaction.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Emit appends an event durably and then publishes it to live subscribers.
//
// It writes on the writer pool, which holds exactly one connection (ADR-0003).
// Calling it from inside a transaction started on that same pool would
// therefore wait for a connection the caller is holding, and block until the
// context expires. Emit from inside a transaction with EmitTx.
func (l *Log) Emit(ctx context.Context, eventType, subjectType, subjectID string, payload any) (Event, error) {
	e, err := l.append(ctx, l.writer, eventType, subjectType, subjectID, payload)
	if err != nil {
		return Event{}, err
	}
	l.publish(e)
	return e, nil
}

// EmitTx appends an event inside the caller's transaction and does NOT publish
// it. Publish the returned events with Publish once the transaction commits.
//
// Two things make this the only correct way to record a state transition that
// is part of a larger write:
//
//   - The writer pool is a single connection (ADR-0003). An Emit nested inside
//     an InTx on that pool waits for a connection its own caller is holding,
//     and the symptom is a hang until the context expires rather than an error.
//   - A subscriber must never see an event whose transaction later rolls back.
//     It may act on it, and the log is the record of what happened (§76).
//
// The database still assigns seq, so ordering remains the database's job.
func (l *Log) EmitTx(ctx context.Context, tx *sql.Tx, eventType, subjectType, subjectID string, payload any) (Event, error) {
	if tx == nil {
		return Event{}, errors.New("events: a transaction is required — use Emit outside one")
	}
	return l.append(ctx, tx, eventType, subjectType, subjectID, payload)
}

// Publish fans out events that are already durable. It is the second half of
// EmitTx and must be called only after the transaction has committed.
func (l *Log) Publish(evs ...Event) {
	for _, e := range evs {
		l.publish(e)
	}
}

func (l *Log) append(ctx context.Context, q rowQuerier, eventType, subjectType, subjectID string, payload any) (Event, error) {
	if eventType == "" {
		return Event{}, errors.New("events: type must be set")
	}
	encoded := json.RawMessage("{}")
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return Event{}, fmt.Errorf("events: encoding %s payload: %w", eventType, err)
		}
		encoded = b
	}

	e := Event{
		ID:          uuid.Must(uuid.NewV7()).String(),
		Type:        eventType,
		SubjectType: subjectType,
		SubjectID:   subjectID,
		Payload:     encoded,
		CreatedAt:   l.clock.Now(),
	}

	// The database assigns seq, so ordering is the database's job rather than a
	// counter two writers could race on.
	row := q.QueryRowContext(ctx, `
		INSERT INTO events (id, type, subject_type, subject_id, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?) RETURNING seq`,
		e.ID, e.Type, e.SubjectType, e.SubjectID, string(e.Payload), e.CreatedAt.Format(timeFormat))
	if err := row.Scan(&e.Seq); err != nil {
		return Event{}, fmt.Errorf("events: appending %s: %w", eventType, err)
	}
	return e, nil
}

// publish delivers to live subscribers. It never blocks the caller: a slow
// subscriber is dropped rather than backpressured, because the alternative is
// one stalled SSE client wedging every write in the system.
func (l *Log) publish(e Event) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, sub := range l.subs {
		if !sub.matches(e.Type) {
			continue
		}
		select {
		case sub.ch <- e:
		default:
			sub.markDropped()
			l.log.Warn("event subscriber is too slow; dropping",
				"subscriber", sub.id, "type", e.Type, "seq", e.Seq)
		}
	}
}

// Since returns events after seq, oldest first, up to limit.
//
// This is what makes reconnection gapless: a client that saw seq N asks for
// everything after N, then switches to the live stream.
func (l *Log) Since(ctx context.Context, after int64, types []string, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 1000
	}
	query := `SELECT seq, id, type, subject_type, subject_id, payload, created_at
		FROM events WHERE seq > ?`
	args := []any{after}
	if len(types) > 0 {
		clause, typeArgs := typeFilter(types)
		if clause != "" {
			// #nosec G202 -- clause is assembled only from the literals
			// "type LIKE ?" and "type = ?"; every caller-supplied value goes
			// through a bind parameter in typeArgs. TestTypeFilterOnlyEmitsBoundClauses
			// pins that, so this stays true if typeFilter changes.
			query += " AND (" + clause + ")"
			args = append(args, typeArgs...)
		}
	}
	query += " ORDER BY seq ASC LIMIT ?"
	args = append(args, limit)

	rows, err := l.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("events: reading since %d: %w", after, err)
	}
	defer func() { _ = rows.Close() }()

	var out []Event
	for rows.Next() {
		var e Event
		var payload, createdAt string
		if err := rows.Scan(&e.Seq, &e.ID, &e.Type, &e.SubjectType, &e.SubjectID, &payload, &createdAt); err != nil {
			return nil, fmt.Errorf("events: reading since %d: %w", after, err)
		}
		e.Payload = json.RawMessage(payload)
		if t, err := time.Parse(timeFormat, createdAt); err == nil {
			e.CreatedAt = t
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("events: reading since %d: %w", after, err)
	}
	return out, nil
}

// Latest returns the highest sequence number recorded, or 0 for an empty log.
func (l *Log) Latest(ctx context.Context) (int64, error) {
	var seq sql.NullInt64
	if err := l.reader.QueryRowContext(ctx, `SELECT max(seq) FROM events`).Scan(&seq); err != nil {
		return 0, fmt.Errorf("events: reading the latest sequence: %w", err)
	}
	return seq.Int64, nil
}

// Subscription is a live feed of events.
type Subscription struct {
	id      int64
	ch      chan Event
	types   []string
	log     *Log
	once    sync.Once
	dropped atomic64
}

type atomic64 struct {
	mu sync.Mutex
	n  int64
}

func (a *atomic64) add() {
	a.mu.Lock()
	a.n++
	a.mu.Unlock()
}

func (a *atomic64) load() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.n
}

// Subscribe returns a live feed. Types may contain exact names or prefix
// patterns ending in `*`, matching §76's namespaces.
//
// The buffer is bounded on purpose. An unbounded queue turns a stalled client
// into unbounded memory growth, which fails the whole process instead of the
// one connection that is actually broken.
func (l *Log) Subscribe(buffer int, types ...string) *Subscription {
	if buffer <= 0 {
		buffer = 256
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextID++
	sub := &Subscription{
		id:    l.nextID,
		ch:    make(chan Event, buffer),
		types: types,
		log:   l,
	}
	l.subs[sub.id] = sub
	return sub
}

// Events is the channel to range over.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Dropped reports how many events this subscriber was too slow to take. A
// non-zero count means the client's view has gaps and it should reconnect with
// ?after= rather than trusting the stream.
func (s *Subscription) Dropped() int64 { return s.dropped.load() }

// Close unsubscribes. Safe to call more than once.
func (s *Subscription) Close() {
	s.once.Do(func() {
		s.log.mu.Lock()
		delete(s.log.subs, s.id)
		s.log.mu.Unlock()
		close(s.ch)
	})
}

func (s *Subscription) markDropped() { s.dropped.add() }

func (s *Subscription) matches(eventType string) bool {
	if len(s.types) == 0 {
		return true
	}
	for _, pattern := range s.types {
		if matchType(pattern, eventType) {
			return true
		}
	}
	return false
}

// matchType supports exact names and trailing-* prefixes, which is what §76's
// namespaces (`blob.*`, `job.*`) need and nothing more.
func matchType(pattern, eventType string) bool {
	if pattern == "" || pattern == "*" {
		return true
	}
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(eventType, prefix)
	}
	return pattern == eventType
}

// SubscriberCount reports how many live subscriptions exist.
func (l *Log) SubscriberCount() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.subs)
}

func typeFilter(types []string) (string, []any) {
	var clauses []string
	var args []any
	for _, t := range types {
		if t == "" || t == "*" {
			return "", nil
		}
		if prefix, ok := strings.CutSuffix(t, "*"); ok {
			clauses = append(clauses, "type LIKE ?")
			args = append(args, prefix+"%")
			continue
		}
		clauses = append(clauses, "type = ?")
		args = append(args, t)
	}
	return strings.Join(clauses, " OR "), args
}
