// Package health answers "is that peer up?" for read routing and replication
// (spec §31, §32, §35, M4-10).
//
// §31 says "every HEALTHY Full Peer can serve content" and §32 lists health
// among the six inputs to read routing. Nothing modelled it: there was one
// peer, so the question never came up. With two, routing cannot make its first
// decision without an answer.
//
// Three decisions shape this package. Each of them avoids a specific failure,
// and each of them is the opposite of what the shortest implementation does.
//
// # 1. Health is observed, not declared
//
// A peer that SAYS it is healthy is reporting an intention. A peer that
// answered a request thirty seconds ago is reporting a fact, and the fact is
// what routing needs. So liveness is derived from interactions that were going
// to happen anyway — an inventory report, a served blob read, a snapshot pull —
// each of which calls Answered. There is no "I am healthy" endpoint for a peer
// to assert into, and adding one would let a node that can accept a POST but
// not serve a byte range advertise itself as a read source.
//
// The probe (see Prober) exists only for the idle case: nothing has talked to
// that peer recently, so nothing has learned anything about it recently.
//
// # 2. Unhealthy is a timeout, not an error
//
// This is the one the intuitive implementation gets backwards, so it is worth
// stating flatly: a peer that returned a 500 is HEALTHY, and a peer that
// returned nothing at all for longer than the window is UNHEALTHY whether or
// not anything ever failed.
//
// The reason is what each signal actually proves. A 500 proves a process is
// running, listening, routing and answering — everything reachability means —
// and says only that one request went wrong, which on a busy homelab peer
// mid-scan is ordinary. Silence proves nothing is answering, which is the
// entire question. A design keyed on error responses declares a busy peer dead
// and a powered-off peer alive, and it does both at exactly the moment the
// distinction matters.
//
// Concretely: Answered is called for ANY response, whatever its status, and
// health decays only through the passage of time in Sweep. Nothing in this
// package inspects a status code, and nothing should learn to.
//
// # 3. Health is advisory for reads and blocking for writes
//
// See Sources and Destinations. That asymmetry is the substance of this issue
// and the comment on Destinations is the load-bearing one.
//
// # Where the answer lives
//
// The peers table, in the two columns migration 00020 reserved: health and
// last_seen_at. The stored column is the answer everyone reads — the API, the
// CLI, Sources — rather than each caller re-deriving it from last_seen_at
// against its own clock. One stored value means the state an operator is shown
// and the state a peer.health_changed event announced can never disagree, and
// disagreeing about liveness is how an operator stops believing either. What
// the caller gets for free from that choice is bounded staleness rather than no
// staleness: the sweep runs on the reconciliation beat, so the stored value
// trails reality by at most one beat. last_seen_at is surfaced next to it
// precisely so that a human can see how much to trust it.
package health

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// State is a peer's reachability, as stored in peers.health. The three values
// are the CHECK constraint in migration 00020; anything else is a schema error
// rather than a value this package can produce.
type State string

// The reachability states (§35, migration 00020).
const (
	// StateUnknown is a peer nothing has ever heard from. It is the column
	// default, and it is deliberately NOT a synonym for reachable: a peer that
	// has never been probed has not been shown to be up, and routing a read to
	// it on the strength of an assumption is the failure the default exists to
	// make impossible.
	StateUnknown State = "unknown"
	// StateReachable is a peer that answered inside the window.
	StateReachable State = "reachable"
	// StateUnreachable is a peer that has answered nothing for longer than the
	// window. It is a statement about silence, not about errors.
	StateUnreachable State = "unreachable"
)

// DefaultWindow is how long a peer may be silent before it is unreachable.
//
// Fifteen minutes, chosen against what each direction costs. Too short and an
// ordinary reboot, a NAS spinning up its array, or a peer busy hashing a large
// ingest flaps to unreachable and back — and every flap is an event, a routing
// change, and an operator's trust in the field. Too long and a site that went
// down at breakfast is still being offered as a read source at lunch, which is
// a stalled stream rather than a fallback to the other peer.
//
// Fifteen minutes is comfortably longer than any restart and comfortably
// shorter than any outage worth calling one. It is three sweeps of the
// reconciliation beat, so a peer that is genuinely gone is probed three times
// before it is called gone.
//
// It is a field on Options rather than a key in the config file. The
// reconciliation beat's interval made the same call for the same reason: a
// knob that exists only because it was easy to add is a knob somebody
// eventually sets to something harmful, and nothing yet suggests an operator
// wants a different number. What Options buys is the thing that is actually
// needed today — a test that can choose a window and an injected clock instead
// of sleeping.
const DefaultWindow = 15 * time.Minute

// Peer is one peer's reachability, and when that was last established.
//
// LastSeenAt travels with State everywhere, including onto the wire and into
// the CLI table, and that is not decoration. "Unhealthy" on its own is a status
// nobody can act on: it does not say whether to reboot something or to wait
// twenty seconds. It is the same argument PlacementVerdict.Missing was built
// on — the actionable half of a verdict is what it names, not the verdict.
type Peer struct {
	PeerID     string
	Name       string
	Endpoint   string
	IsSelf     bool
	State      State
	LastSeenAt time.Time
}

// Seen reports whether this peer has ever been heard from.
func (p Peer) Seen() bool { return !p.LastSeenAt.IsZero() }

// Clock is injected so the window is testable without sleeping (ADR-0017).
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// Prober asks a peer whether anything is there, for the case where nothing
// else has asked recently.
//
// The contract is the whole of decision 2, so it is stated as a rule rather
// than as a description: Probe returns nil if the peer ANSWERED, whatever it
// answered. A 500, a 404, a 503 from a peer mid-migration — all of them are
// nil, because all of them prove a process is listening. Probe returns an
// error only when nothing came back: connection refused, no route, TLS
// failure, timeout.
//
// An implementation that returns an error for a non-2xx status is a bug, not a
// stricter check. It makes a peer that is up and busy indistinguishable from a
// peer that is off.
type Prober interface {
	Probe(ctx context.Context, peer Peer) error
}

// Options configure a Tracker.
type Options struct {
	DB     *sqlite.DB
	Events *events.Log
	Clock  Clock
	// Window is the silence after which a peer is unreachable. Zero means
	// DefaultWindow.
	Window time.Duration
	// Prober is how an idle peer is checked. Nil means no probing, which is a
	// supported configuration: liveness still flows from ordinary successful
	// interactions, and a deployment whose peers talk constantly needs nothing
	// else.
	Prober Prober
	Logger *slog.Logger
}

// Tracker records liveness and moves peers across the reachability edge.
type Tracker struct {
	db     *sqlite.DB
	events *events.Log
	clock  Clock
	window time.Duration
	prober Prober
	log    *slog.Logger
}

const timestampFormat = time.RFC3339Nano

// columns is the projection both reads share, so a column added to one scanner
// and not the other is a compile error rather than a wrong answer.
const columns = `id, name, endpoint, is_self, health, last_seen_at`

// New constructs a Tracker.
func New(opts Options) (*Tracker, error) {
	if opts.DB == nil {
		return nil, errors.New("health: a database is required")
	}
	if opts.Events == nil {
		return nil, errors.New("health: an event log is required — every state transition emits an event (ADR-0009)")
	}
	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}
	window := opts.Window
	if window <= 0 {
		window = DefaultWindow
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Tracker{
		db:     opts.DB,
		events: opts.Events,
		clock:  clock,
		window: window,
		prober: opts.Prober,
		log:    log.With("component", "peer-health"),
	}, nil
}

// Window is the silence this tracker treats as unreachable.
func (t *Tracker) Window() time.Duration { return t.window }

// Answered records that a peer produced a response, and is the primary way
// liveness is learned (decision 1).
//
// Every ordinary successful interaction with a peer calls this — an inventory
// report received, a blob range served, a snapshot pulled — so that in a busy
// deployment the probe below never has anything to do. "Successful" here means
// the peer answered, NOT that the answer was a 2xx: see the package doc, and
// see Prober.
//
// It is idempotent and cheap: one UPDATE, and an event only on the edge.
func (t *Tracker) Answered(ctx context.Context, peerID string) error {
	now := t.clock.Now().UTC()
	var (
		ev   events.Event
		emit bool
	)
	err := t.db.InTx(ctx, func(tx *sql.Tx) error {
		p, err := scan(tx.QueryRowContext(ctx, `SELECT `+columns+` FROM peers WHERE id = ?`, peerID))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// A removed peer is not an error worth failing an otherwise
			// successful interaction over. Revocation is deletion (ADR-0012),
			// so the row going away mid-request is a supported race, and the
			// interaction it happened during was already refused by the
			// membership guard.
			return nil
		case err != nil:
			return fmt.Errorf("health: reading peer %s: %w", peerID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE peers SET last_seen_at = ?, health = ? WHERE id = ?`,
			now.Format(timestampFormat), string(StateReachable), peerID); err != nil {
			return fmt.Errorf("health: recording that peer %s answered: %w", peerID, err)
		}
		if p.State == StateReachable {
			// Not an edge. A peer that answers every thirty seconds must not
			// emit every thirty seconds — that is a heartbeat in the event
			// log, which is the blob.verified mistake events.go:26 refused.
			return nil
		}
		emit = true
		ev, err = t.emitTx(ctx, tx, p, StateReachable, now)
		return err
	})
	if err != nil {
		return err
	}
	if emit {
		t.events.Publish(ev)
	}
	return nil
}

// livenessResolution is how much a peer's last_seen_at may lag behind reality
// before an inbound interaction is worth a write.
//
// Seen is called on EVERY request a peer makes, and a peer mid-transfer makes
// a great many. Recording each one would put a write into the single-writer
// control plane (ADR-0003) on a request path that is otherwise one indexed
// read, in order to move a timestamp by milliseconds — and the timestamp is
// consumed by a window measured in minutes.
//
// A tenth of the window is fine-grained enough that the answer is never wrong
// in a way anything can act on: the worst case is a peer whose last_seen_at is
// one tenth of a window stale, which cannot flip it across the edge. This is
// NOT a cache of the membership answer — that one is looked up on every
// request without exception, because its freshness is the revocation
// mechanism. This throttles a write about liveness, which is a fact that
// decays over fifteen minutes.
func (t *Tracker) livenessResolution() time.Duration { return t.window / 10 }

// Seen records that a peer TALKED TO US, keyed by the public key it presented.
//
// This is decision 1 in its most literal form: an inbound peer request is a
// peer proving it is up, on a connection it opened, as a side effect of work it
// was doing anyway. No probe can be better evidence than that, and in a busy
// fabric this is where almost all liveness comes from — the probe only covers
// the case where nothing has happened.
//
// It is deliberately called AFTER the membership guard has admitted the
// request. A key that is not a member is not a peer whose liveness this system
// has any business recording.
func (t *Tracker) Seen(ctx context.Context, publicKey []byte) error {
	p, err := scan(t.db.Reader().QueryRowContext(ctx,
		`SELECT `+columns+` FROM peers WHERE public_key = ?`, publicKey))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Not a member. Nothing to record, and not an error: revocation is
		// deletion, so a row disappearing between the guard and here is the
		// supported race rather than a failure.
		return nil
	case err != nil:
		return fmt.Errorf("health: looking up the peer that made this request: %w", err)
	}
	if p.State == StateReachable && p.Seen() &&
		t.clock.Now().UTC().Sub(p.LastSeenAt) < t.livenessResolution() {
		return nil
	}
	return t.Answered(ctx, p.PeerID)
}

// Sweep is the timeout half of the model, and the only thing that can call a
// peer unreachable.
//
// For each peer it probes if nothing has been heard recently, and then compares
// the state the clock implies against the state that is stored. A difference is
// an edge: the column moves and exactly one peer.health_changed is emitted. No
// difference is silence, however many times it is swept — which is what makes
// this edge-triggered rather than a heartbeat.
//
// It returns how many peers it moved in each direction, for the beat to log.
func (t *Tracker) Sweep(ctx context.Context) (Summary, error) {
	peers, err := t.List(ctx)
	if err != nil {
		return Summary{}, err
	}
	var sum Summary
	for _, p := range peers {
		sum.Swept++
		if p.IsSelf {
			// This node. The process running this sweep IS the evidence, and
			// probing our own listener would only prove that the loopback
			// interface works. Recording it keeps the self row's last_seen_at
			// meaningful next to every other row's rather than permanently
			// null, which reads as "never heard from" for the one peer we are
			// certain about.
			if err := t.Answered(ctx, p.PeerID); err != nil {
				return sum, err
			}
			continue
		}

		if t.shouldProbe(p) {
			sum.Probed++
			if err := t.prober.Probe(ctx, p); err != nil {
				// Deliberately not recorded anywhere. A failed probe is
				// evidence of nothing beyond the silence that last_seen_at
				// already measures, and a "consecutive failures" counter here
				// would be a second, competing definition of unhealthy — one
				// that a busy peer refusing a connection for a moment can
				// trip, and that a powered-off peer with no probe attempt
				// cannot.
				t.log.Debug("a peer did not answer a probe",
					"peer_id", p.PeerID, "name", p.Name, "endpoint", p.Endpoint, "error", err)
			} else if err := t.Answered(ctx, p.PeerID); err != nil {
				return sum, err
			} else {
				// Answered already moved the column and emitted the edge if
				// there was one; the sweep only has to count it.
				if p.State != StateReachable {
					sum.BecameReachable++
				}
				p.State = StateReachable
				p.LastSeenAt = t.clock.Now().UTC()
			}
		}

		want := Derive(p.LastSeenAt, t.clock.Now().UTC(), t.window)
		if want == p.State {
			continue
		}
		if err := t.transition(ctx, p, want); err != nil {
			return sum, err
		}
		switch want {
		case StateReachable:
			sum.BecameReachable++
		case StateUnreachable:
			sum.BecameUnreachable++
		case StateUnknown:
			// Unreachable to unknown cannot happen: last_seen_at is never
			// cleared. Named so the switch is exhaustive.
		}
	}
	return sum, nil
}

// Summary is one sweep's outcome, for the beat's log line.
type Summary struct {
	Swept             int
	Probed            int
	BecameReachable   int
	BecameUnreachable int
}

// shouldProbe decides whether this peer needs asking.
//
// Only when nothing else has: a peer heard from inside half the window is
// already proving itself through ordinary work, and probing it would be a
// request Heyarr made up. Half rather than the whole window so that a peer
// about to time out gets asked before it does, rather than being declared
// unreachable and then immediately probed.
func (t *Tracker) shouldProbe(p Peer) bool {
	if t.prober == nil || p.Endpoint == "" {
		return false
	}
	if !p.Seen() {
		return true
	}
	return t.clock.Now().UTC().Sub(p.LastSeenAt) >= t.window/2
}

// Derive is the whole of the health rule, as one pure function.
//
// It is silence against a window, and nothing else. There is no error count in
// it and no failure threshold, because both would be a second definition of
// unhealthy that disagrees with this one — see the package doc.
func Derive(lastSeen, now time.Time, window time.Duration) State {
	if lastSeen.IsZero() {
		return StateUnknown
	}
	if now.Sub(lastSeen) > window {
		return StateUnreachable
	}
	return StateReachable
}

// transition moves the stored column and emits the one event that says so.
//
// The row is re-read inside the transaction rather than trusting the caller's
// copy, so that a peer removed between the sweep's read and its write is a
// no-op instead of resurrecting a revoked membership row's health.
func (t *Tracker) transition(ctx context.Context, p Peer, to State) error {
	now := t.clock.Now().UTC()
	var (
		ev   events.Event
		emit bool
	)
	err := t.db.InTx(ctx, func(tx *sql.Tx) error {
		current, err := scan(tx.QueryRowContext(ctx, `SELECT `+columns+` FROM peers WHERE id = ?`, p.PeerID))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("health: reading peer %s: %w", p.PeerID, err)
		case current.State == to:
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE peers SET health = ? WHERE id = ?`, string(to), p.PeerID); err != nil {
			return fmt.Errorf("health: recording peer %s as %s: %w", p.PeerID, to, err)
		}
		emit = true
		ev, err = t.emitTx(ctx, tx, current, to, now)
		return err
	})
	if err != nil {
		return err
	}
	if emit {
		t.events.Publish(ev)
		t.log.Info("peer health changed",
			"peer_id", p.PeerID, "name", p.Name, "from", string(p.State), "to", string(to))
	}
	return nil
}

// emitTx writes the one event type this package has.
//
// peer.health_changed carries both ends of the edge (see events.go). There is
// deliberately no peer.up / peer.down pair: two types for two edges of one
// machine is two places to forget to emit, and it forces a subscriber that
// cares about reachability at all to subscribe to both and reassemble the
// machine itself.
//
// The event is returned rather than published here, for membership's reason:
// publishing inside the transaction fans a change out to subscribers that a
// rollback then un-happens.
func (t *Tracker) emitTx(ctx context.Context, tx *sql.Tx, from Peer, to State, now time.Time) (events.Event, error) {
	payload := map[string]any{
		"peer_id": from.PeerID,
		"name":    from.Name,
		"from":    string(from.State),
		"to":      string(to),
		// The window is in the payload because the event is otherwise
		// unfalsifiable by a reader: "unreachable" means nothing without the
		// silence it was measured against.
		"window": t.window.String(),
	}
	// last_seen_at is the actionable half — see the comment on Peer. For a
	// transition INTO unreachable it is when the peer was last alive, which is
	// the number an operator uses to decide whether to go and look.
	if from.Seen() {
		payload["last_seen_at"] = from.LastSeenAt.Format(timestampFormat)
	} else {
		payload["last_seen_at"] = nil
	}
	if to == StateReachable {
		payload["last_seen_at"] = now.Format(timestampFormat)
	}
	return t.events.EmitTx(ctx, tx, events.TypePeerHealthChanged, "peer", from.PeerID, payload)
}

// List returns every peer's reachability, self first and then by name — the
// same order membership.List uses, so the two never have to be reconciled by
// eye.
func (t *Tracker) List(ctx context.Context) ([]Peer, error) {
	rows, err := t.db.Reader().QueryContext(ctx,
		`SELECT `+columns+` FROM peers ORDER BY is_self DESC, name ASC`)
	if err != nil {
		return nil, fmt.Errorf("health: listing peers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Peer
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("health: listing peers: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("health: listing peers: %w", err)
	}
	return out, nil
}

// Of returns one peer's reachability.
func (t *Tracker) Of(ctx context.Context, peerID string) (Peer, error) {
	p, err := scan(t.db.Reader().QueryRowContext(ctx,
		`SELECT `+columns+` FROM peers WHERE id = ?`, peerID))
	if err != nil {
		return Peer{}, fmt.Errorf("health: reading the health of peer %s: %w", peerID, err)
	}
	return p, nil
}

func scan(row interface{ Scan(...any) error }) (Peer, error) {
	var (
		p        Peer
		endpoint sql.NullString
		isSelf   int
		state    string
		lastSeen sql.NullString
	)
	if err := row.Scan(&p.PeerID, &p.Name, &endpoint, &isSelf, &state, &lastSeen); err != nil {
		return Peer{}, err
	}
	p.Endpoint = endpoint.String
	p.IsSelf = isSelf == 1
	p.State = State(state)
	if lastSeen.Valid {
		if ts, err := time.Parse(timestampFormat, lastSeen.String); err == nil {
			p.LastSeenAt = ts.UTC()
		}
	}
	return p, nil
}
