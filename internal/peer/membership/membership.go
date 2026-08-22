// Package membership is the peer fabric's trust root (§26, §7, ADR-0012).
//
// # Membership is the only trust root, and it is a lookup rather than a list
//
// ADR-0012: peers authenticate with mTLS over self-signed certificates "whose
// public keys are pinned by the controller-issued peer membership record. No
// CA, no PKI". And: "Revocation is removing a membership record."
//
// That second sentence is a design constraint wearing the clothes of a
// convenience. If revocation is deletion, deletion has to actually sever
// access — and the only way it does is if membership is consulted at the
// moment of use. A set loaded at process start, a map warmed on first request,
// a cache with a TTL: each of them means a peer whose row is gone is still
// reading bytes, for a window whose length nobody chose deliberately. There is
// no revocation list in this design to fall back on, so the freshness of this
// lookup IS the revocation mechanism.
//
// Lookup therefore runs a query. Every time. It is one indexed equality lookup
// on a unique index over a table with as many rows as the operator has
// machines, and the alternative is a security control that works until the
// convenient moment it does not. If this ever needs to be faster, the answer
// is a faster query, not a longer-lived answer.
//
// M4-05 owns the mechanism that presents a peer's key — the mTLS handshake and
// the certificate whose public key is compared against these records. This
// package owns the question that mechanism asks, and it owns it separately so
// that the answer cannot quietly become "whatever we knew at startup".
//
// # Enrolment is operator-mediated, and a peer is its key
//
// There is no broadcast, no join token and no trust on first use. An operator
// reads the other node's public key out of `heyarr peers` at that site and
// registers it here; the other operator does the same in the other direction.
// Two nodes, two commands.
//
// A peer is registered BY its public key. The endpoint is where to reach it
// and may change freely — Register accepts a new endpoint for a key it already
// holds, and the peer's id, its enrolment and everything referring to it are
// untouched. Registering by hostname and learning the key afterwards is the
// shape that produces trust on first use, so there is no way to spell it here:
// PublicKey is required and a Registration without one is refused before any
// row is read.
package membership

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// Refusals. Each one is a distinct error because each one calls for a
// different action from the operator who hit it, and a test asserting "some
// error" would pass on the wrong one.
var (
	// ErrKeyRegistered is a public key already pinned to a different peer.
	// Two peers behind one key is one presented certificate authorising two
	// identities, which is the trust root failing open.
	ErrKeyRegistered = errors.New("membership: this public key is already registered to another peer")
	// ErrNameTaken is a name already used by a peer with a different key.
	ErrNameTaken = errors.New("membership: a different peer is already registered under this name")
	// ErrSelfRemoval is an attempt to revoke this node's own membership. It
	// would leave a deployment unable to name the peer that holds its bytes.
	ErrSelfRemoval = errors.New("membership: this node cannot remove its own membership record")
	// ErrSelfExists is an attempt to register a second peer as this node.
	ErrSelfExists = errors.New("membership: this instance already has a self peer")
	// ErrUnknownPeer is a reference naming no peer.
	ErrUnknownPeer = errors.New("membership: no peer is registered under that name or id")
	// ErrNotAMember is a presented public key that no membership record pins.
	// It is what a removed peer gets on its next request, on the connection it
	// was already using.
	ErrNotAMember = errors.New("membership: the presented public key is not a member of this fabric")
)

// ErrMalformedKey is re-exported from identity so a caller validating an
// enrolment does not have to import two packages to tell a typo from a
// collision.
var ErrMalformedKey = identity.ErrMalformedPublicKey

// Transitions carried in a peer.registered payload.
//
// One event type for the whole membership machine, with the transition in the
// payload (§76, M4-15). A separate "peer.endpoint_changed" would mean a
// subscriber reconciling "who is in this fabric and where" had to watch two
// types to learn one fact.
const (
	// TransitionEnrolled is a peer admitted for the first time.
	TransitionEnrolled = events.PeerTransitionEnrolled
	// TransitionEndpointChanged is an existing member reachable somewhere
	// else. Its identity — the key, the id — is unchanged.
	TransitionEndpointChanged = events.PeerTransitionEndpointChanged
	// TransitionUnchanged is a re-registration that asserted exactly what was
	// already recorded. It is NOT in the event vocabulary and never appears in
	// a payload: invariant 7 is about state transitions, and this is not one.
	// It exists so a caller can tell "moved" from "already said that".
	TransitionUnchanged = "unchanged"
	// TransitionRemoved is membership revoked, carried in peer.removed.
	TransitionRemoved = events.PeerTransitionRemoved
)

// Member is one membership record.
//
// There is no private key field and there never can be: the private half of a
// peer's identity lives at that peer, at 0600, and this table holds the public
// half precisely because it is the half that is safe to copy.
type Member struct {
	PeerID     string
	Name       string
	Site       string
	Mode       string
	Endpoint   string
	PublicKey  ed25519.PublicKey
	IsSelf     bool
	EnrolledAt time.Time
	CreatedAt  time.Time
}

// Registration is what an operator asserts about another node.
type Registration struct {
	Name string
	Site string
	// Mode defaults to "full" (§9).
	Mode string
	// Endpoint is where to reach the peer. It is not identity: it may be
	// changed later by registering the same key again.
	Endpoint string
	// PublicKey is who the peer is. Required.
	PublicKey []byte
	// IsSelf registers this node. Nothing outside first-start setup sets it,
	// and a second one is refused — see ErrSelfExists. It exists as a field
	// rather than being unspellable so that the refusal is a real query
	// against a real database rather than a constant this package returns.
	IsSelf bool
}

// Result is a registration's outcome and which transition it was.
type Result struct {
	Member     Member
	Transition string
}

// Clock is injected so enrolment timestamps are testable (ADR-0017).
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// Options configure a Store.
type Options struct {
	DB     *sqlite.DB
	Events *events.Log
	Clock  Clock
	Logger *slog.Logger
	// NewID mints peer identifiers. Injected for deterministic tests; UUIDv7
	// otherwise (ADR-0017).
	NewID func() string
}

// Store reads and writes membership records.
type Store struct {
	db     *sqlite.DB
	events *events.Log
	clock  Clock
	log    *slog.Logger
	newID  func() string
}

const timestampFormat = time.RFC3339Nano

// columns is the projection every read here shares, so a column added to one
// scanner and not the other is a compile error rather than a wrong answer.
const columns = `id, name, site, mode, endpoint, public_key, is_self, enrolled_at, created_at`

// New constructs a Store.
func New(opts Options) (*Store, error) {
	if opts.DB == nil {
		return nil, errors.New("membership: a database is required")
	}
	if opts.Events == nil {
		return nil, errors.New("membership: an event log is required — every state transition emits an event (ADR-0009)")
	}
	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	newID := opts.NewID
	if newID == nil {
		newID = func() string { return uuid.Must(uuid.NewV7()).String() }
	}
	return &Store{db: opts.DB, events: opts.Events, clock: clock, log: log.With("component", "membership"), newID: newID}, nil
}

// Lookup answers "is this key a member, and which one" — the question the peer
// read path asks on every request.
//
// It queries. See the package doc: the freshness of this answer is the
// revocation mechanism, so there is nothing here to memoise and nothing to
// warm. A caller that wants this cheaper wants a better index.
func (s *Store) Lookup(ctx context.Context, pub []byte) (Member, error) {
	if len(pub) != ed25519.PublicKeySize {
		// A presented key of the wrong length is not a member and is also not
		// a database question. Answering it without a query is not a cache:
		// no row can ever match, whatever the table contains.
		return Member{}, fmt.Errorf("%w: the presented key is %d bytes, and an ed25519 public key is %d",
			ErrNotAMember, len(pub), ed25519.PublicKeySize)
	}
	row := s.db.Reader().QueryRowContext(ctx,
		`SELECT `+columns+` FROM peers WHERE public_key = ?`, pub)
	m, err := scan(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Member{}, fmt.Errorf("%w: %s", ErrNotAMember, identity.FormatPublicKey(pub))
	case err != nil:
		return Member{}, fmt.Errorf("membership: looking up a peer by public key: %w", err)
	}
	return m, nil
}

// IsMember is Lookup reduced to the boolean the request guard needs.
//
// It exists so that the HTTP layer can consult membership without importing
// the record type or learning how to tell "not a member" from "the database is
// down" — those are 403 and 500 and getting them the wrong way round either
// leaks membership or lets a removed peer keep reading.
func (s *Store) IsMember(ctx context.Context, pub []byte) (bool, error) {
	_, err := s.Lookup(ctx, pub)
	switch {
	case errors.Is(err, ErrNotAMember):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// Get resolves a peer by id or by name, in that order.
//
// An operator holds whichever of the two they were shown, and requiring them
// to know which is which is how `peers remove` gets run against the wrong
// thing at the wrong moment.
func (s *Store) Get(ctx context.Context, ref string) (Member, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Member{}, fmt.Errorf("%w: %q", ErrUnknownPeer, ref)
	}
	row := s.db.Reader().QueryRowContext(ctx,
		`SELECT `+columns+` FROM peers WHERE id = ? OR name = ? ORDER BY (id = ?) DESC LIMIT 1`,
		ref, ref, ref)
	m, err := scan(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Member{}, fmt.Errorf("%w: %q", ErrUnknownPeer, ref)
	case err != nil:
		return Member{}, fmt.Errorf("membership: reading a peer: %w", err)
	}
	return m, nil
}

// List returns every membership record, self first and then by name.
func (s *Store) List(ctx context.Context) ([]Member, error) {
	rows, err := s.db.Reader().QueryContext(ctx,
		`SELECT `+columns+` FROM peers ORDER BY is_self DESC, name ASC`)
	if err != nil {
		return nil, fmt.Errorf("membership: listing peers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Member
	for rows.Next() {
		m, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("membership: listing peers: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("membership: listing peers: %w", err)
	}
	return out, nil
}

// Register admits a peer, or moves an existing member's endpoint.
//
// The key decides which. A key nobody holds is an enrolment; a key this
// instance already pins is the same peer, and what may differ is where it is
// reachable. A key held under a different name is refused rather than adopted:
// silently re-pointing a pinned key at a new name is how an operator who
// pasted the wrong site's key finds out months later.
func (s *Store) Register(ctx context.Context, reg Registration) (Result, error) {
	name := strings.TrimSpace(reg.Name)
	if name == "" {
		return Result{}, errors.New("membership: a peer name is required")
	}
	if len(reg.PublicKey) == 0 {
		// The refusal that keeps trust-on-first-use unspellable. A peer with
		// no key is a hostname somebody hopes is the right machine.
		return Result{}, fmt.Errorf("%w: a peer is registered by its public key, and none was given — "+
			"read it from `heyarr peers` at the other site (ADR-0012)", ErrMalformedKey)
	}
	if len(reg.PublicKey) != ed25519.PublicKeySize {
		return Result{}, fmt.Errorf("%w: the key is %d bytes, and an ed25519 public key is %d",
			ErrMalformedKey, len(reg.PublicKey), ed25519.PublicKeySize)
	}
	mode := strings.TrimSpace(reg.Mode)
	if mode == "" {
		mode = "full"
	}
	switch mode {
	case "full", "partial", "cache", "archive", "compute":
	default:
		return Result{}, fmt.Errorf("membership: %q is not a peer mode; it must be one of "+
			"full, partial, cache, archive, compute (§9)", reg.Mode)
	}

	now := s.clock.Now().UTC()
	var (
		result Result
		ev     events.Event
		emit   bool
	)
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		existing, err := scan(tx.QueryRowContext(ctx,
			`SELECT `+columns+` FROM peers WHERE public_key = ?`, reg.PublicKey))
		switch {
		case err == nil:
			result, ev, err = s.reregister(ctx, tx, existing, name, reg, mode)
			emit = result.Transition != TransitionUnchanged
			return err
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("membership: reading the peer holding this key: %w", err)
		}

		// The key is new. The name must be too.
		var takenBy string
		err = tx.QueryRowContext(ctx, `SELECT id FROM peers WHERE name = ?`, name).Scan(&takenBy)
		switch {
		case err == nil:
			return fmt.Errorf("%w: %q is peer %s, and it holds a different public key. "+
				"Remove it first, or enrol this key under another name", ErrNameTaken, name, takenBy)
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("membership: checking the peer name: %w", err)
		}

		if reg.IsSelf {
			var selfID, selfName string
			err = tx.QueryRowContext(ctx, `SELECT id, name FROM peers WHERE is_self = 1`).Scan(&selfID, &selfName)
			switch {
			case err == nil:
				return fmt.Errorf("%w: peer %s (%q) is this node. Two rows claiming to be self is "+
					"unrecoverable once replication has run (ADR-0010)", ErrSelfExists, selfID, selfName)
			case !errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("membership: checking for an existing self peer: %w", err)
			}
		}

		m := Member{
			PeerID:     s.newID(),
			Name:       name,
			Site:       strings.TrimSpace(reg.Site),
			Mode:       mode,
			Endpoint:   strings.TrimSpace(reg.Endpoint),
			PublicKey:  ed25519.PublicKey(append([]byte(nil), reg.PublicKey...)),
			IsSelf:     reg.IsSelf,
			EnrolledAt: now,
			CreatedAt:  now,
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO peers (id, name, site, mode, endpoint, public_key, is_self, enrolled_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.PeerID, m.Name, m.Site, m.Mode, nullable(m.Endpoint), []byte(m.PublicKey),
			boolToInt(m.IsSelf), now.Format(timestampFormat), now.Format(timestampFormat)); err != nil {
			return fmt.Errorf("membership: enrolling peer %q: %w", name, err)
		}
		result = Result{Member: m, Transition: TransitionEnrolled}
		emit = true
		ev, err = s.events.EmitTx(ctx, tx, events.TypePeerRegistered, "peer", m.PeerID,
			payload(m, TransitionEnrolled))
		return err
	})
	if err != nil {
		return Result{}, err
	}
	if emit {
		s.events.Publish(ev)
		s.log.Info("peer membership changed",
			"peer_id", result.Member.PeerID, "name", result.Member.Name,
			"transition", result.Transition, "endpoint", result.Member.Endpoint)
	}
	return result, nil
}

// Remove revokes membership by deleting the record (ADR-0012).
//
// Deletion rather than a revoked_at flag, and that is the ADR's decision
// rather than an economy: there is no revocation list in this design, so a row
// that still exists is a peer that is still trusted. The replicas rows for the
// peer go with it through ON DELETE CASCADE, which is correct — a peer this
// system will not talk to is not a peer whose copy counts towards placement.
func (s *Store) Remove(ctx context.Context, ref string) (Member, error) {
	var (
		removed Member
		ev      events.Event
	)
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		m, err := scan(tx.QueryRowContext(ctx,
			`SELECT `+columns+` FROM peers WHERE id = ? OR name = ? ORDER BY (id = ?) DESC LIMIT 1`,
			ref, ref, ref))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: %q", ErrUnknownPeer, ref)
		case err != nil:
			return fmt.Errorf("membership: reading the peer to remove: %w", err)
		}
		if m.IsSelf {
			return fmt.Errorf("%w: %s (%q) is this node. Removing it would leave the instance "+
				"unable to name the peer that holds its own bytes; decommission the node instead",
				ErrSelfRemoval, m.PeerID, m.Name)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM peers WHERE id = ?`, m.PeerID); err != nil {
			return fmt.Errorf("membership: removing peer %s: %w", m.PeerID, err)
		}
		removed = m
		ev, err = s.events.EmitTx(ctx, tx, events.TypePeerRemoved, "peer", m.PeerID,
			payload(m, TransitionRemoved))
		return err
	})
	if err != nil {
		return Member{}, err
	}
	s.events.Publish(ev)
	s.log.Info("peer membership revoked",
		"peer_id", removed.PeerID, "name", removed.Name, "transition", TransitionRemoved)
	return removed, nil
}

// reregister is the branch taken when the presented key is already pinned.
//
// It returns the event rather than publishing it: publishing inside the
// transaction would fan a membership change out to subscribers that a rollback
// then un-happened, and a subscriber that acted on it would be enforcing a
// membership record the database does not have.
func (s *Store) reregister(ctx context.Context, tx *sql.Tx, existing Member, name string, reg Registration, mode string) (Result, events.Event, error) {
	if existing.Name != name {
		return Result{}, events.Event{}, fmt.Errorf("%w: it is peer %s (%q), and this registration calls it %q. "+
			"A peer is its key, not its name — remove %q first if it really is being renamed",
			ErrKeyRegistered, existing.PeerID, existing.Name, name, existing.Name)
	}
	endpoint := strings.TrimSpace(reg.Endpoint)
	site := strings.TrimSpace(reg.Site)
	if endpoint == existing.Endpoint && site == existing.Site && mode == existing.Mode {
		return Result{Member: existing, Transition: TransitionUnchanged}, events.Event{}, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE peers SET endpoint = ?, site = ?, mode = ? WHERE id = ?`,
		nullable(endpoint), site, mode, existing.PeerID); err != nil {
		return Result{}, events.Event{}, fmt.Errorf("membership: updating peer %s: %w", existing.PeerID, err)
	}
	updated := existing
	updated.Endpoint = endpoint
	updated.Site = site
	updated.Mode = mode
	ev, err := s.events.EmitTx(ctx, tx, events.TypePeerRegistered, "peer", updated.PeerID,
		payload(updated, TransitionEndpointChanged))
	if err != nil {
		return Result{}, events.Event{}, err
	}
	return Result{Member: updated, Transition: TransitionEndpointChanged}, ev, nil
}

// payload is the one place a peer membership event's body is built, so the
// enrolment, the endpoint move and the removal cannot describe the same peer
// with three different sets of fields.
//
// The public key is in it. That is deliberate and safe: it is the public half,
// it is what a subscriber reconciling the fabric needs in order to know WHICH
// key was pinned or revoked, and it is rendered the same way the API and the
// CLI render it so the three can be compared by eye.
func payload(m Member, transition string) map[string]any {
	return map[string]any{
		"transition": transition,
		"peer_id":    m.PeerID,
		"name":       m.Name,
		"site":       m.Site,
		"mode":       m.Mode,
		"endpoint":   m.Endpoint,
		"public_key": identity.FormatPublicKey(m.PublicKey),
		"is_self":    m.IsSelf,
	}
}

func scan(row interface{ Scan(...any) error }) (Member, error) {
	var (
		m          Member
		endpoint   sql.NullString
		enrolledAt sql.NullString
		pub        []byte
		isSelf     int
		createdAt  string
	)
	if err := row.Scan(&m.PeerID, &m.Name, &m.Site, &m.Mode, &endpoint, &pub, &isSelf,
		&enrolledAt, &createdAt); err != nil {
		return Member{}, err
	}
	m.Endpoint = endpoint.String
	if len(pub) > 0 {
		m.PublicKey = ed25519.PublicKey(pub)
	}
	m.IsSelf = isSelf == 1
	m.CreatedAt = parseTime(createdAt)
	if enrolledAt.Valid {
		m.EnrolledAt = parseTime(enrolledAt.String)
	} else {
		m.EnrolledAt = m.CreatedAt
	}
	return m, nil
}

func parseTime(s string) time.Time {
	t, err := time.Parse(timestampFormat, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
