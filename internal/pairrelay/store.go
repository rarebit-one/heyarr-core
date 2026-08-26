// Package pairrelay is the device-pairing relay (§40, ADR-0022, ADR-0038): the
// dumb store-and-forward two devices exchange through so an old device can
// authorise a new one without the server being trusted.
//
// It is the transport behind internal/pairflow, and the whole of its design is
// what it does NOT do. It keeps a value per (session, slot), hands it back, and
// forgets it after a short life. It never inspects a value, never joins two
// sessions, never signs anything and never learns a key: everything that crosses
// it is public (two commitments, two public keys, a salt, and a signed
// enrolment cert), so a relay that read every byte would still learn nothing it
// could pair with. That is ADR-0038's untrusted relay made literal — if this
// package ever needed to understand a value to be safe, the pairing design would
// have drifted back into trusting the server.
//
// # Why in-memory, and why bounded
//
// A pairing lasts seconds and involves two parties a tap apart, so the relay is
// ephemeral by nature — a durable table would outlive every value it held. It is
// bounded on every axis a public, unauthenticated endpoint must be: a cap on the
// number of live sessions, a cap on the bytes per slot, a fixed allow-list of
// slot names, and a TTL that reaps a session whether or not it completed. Those
// bounds are the entire security surface of serving it without a credential:
// nothing here grants authority, so the only abuse is memory, and the bounds
// close it.
package pairrelay

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// The limits that make an unauthenticated, public endpoint safe to serve. They
// are deliberately generous for a real pairing and hostile to abuse.
const (
	// MaxSessions is how many live pairing sessions the relay holds at once.
	// Reaping expired sessions happens first, so this bounds concurrent, live
	// pairings, not the rate.
	MaxSessions = 256
	// MaxSlotBytes caps a single slot. The largest value is an enrolment cert
	// (two base64url segments), comfortably under a kilobyte; 4 KiB is slack.
	MaxSlotBytes = 4 << 10
	// SessionTTL is how long a session lives from its first write. A pairing
	// completes in seconds; ten minutes tolerates a human reading a code across
	// two screens without leaving state around to accumulate.
	SessionTTL = 10 * time.Minute
)

// AllowedSlots is the fixed set of slot names the relay will store, mirroring
// internal/pairflow's wire contract. A closed set means an attacker cannot use
// the relay as a general-purpose key-value store, and a typo is a 400 rather
// than a silently orphaned value.
var AllowedSlots = map[string]bool{
	"initiator_commit": true,
	"responder_commit": true,
	"initiator_reveal": true,
	"responder_reveal": true,
	"cert":             true,
	"abort":            true,
}

// The errors the store refuses with, mapped to HTTP status by the handler.
var (
	// ErrUnknownSlot is a slot name outside AllowedSlots.
	ErrUnknownSlot = errors.New("pairrelay: unknown slot")
	// ErrTooLarge is a value past MaxSlotBytes.
	ErrTooLarge = errors.New("pairrelay: value too large")
	// ErrSlotConflict is a second write to a slot with different bytes. Slots
	// are write-once: a relay that let a slot be overwritten would let a late
	// value clobber an honest one, which is a tampering vector even though the
	// values are public.
	ErrSlotConflict = errors.New("pairrelay: slot already holds a different value")
	// ErrTooManySessions is the MaxSessions cap, after reaping expired ones.
	ErrTooManySessions = errors.New("pairrelay: too many live pairing sessions")
	// ErrNotFound is a slot (or session) with nothing in it yet.
	ErrNotFound = errors.New("pairrelay: no value")
	// ErrBadSession is a malformed session identifier.
	ErrBadSession = errors.New("pairrelay: malformed session id")
)

// Clock is the injected time source (ADR-0017), so TTL expiry is a unit fact.
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

type session struct {
	slots   map[string][]byte
	created time.Time
}

// Store is the in-memory session table.
type Store struct {
	mu       sync.Mutex
	sessions map[string]*session
	clock    Clock
	ttl      time.Duration
	maxSess  int
	maxBytes int
}

// Options configure a Store. Zero values take the package defaults, so the
// production caller passes none and a test can shrink the TTL.
type Options struct {
	Clock       Clock
	TTL         time.Duration
	MaxSessions int
	MaxBytes    int
}

// New constructs a Store.
func New(opts Options) *Store {
	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = SessionTTL
	}
	maxSess := opts.MaxSessions
	if maxSess <= 0 {
		maxSess = MaxSessions
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = MaxSlotBytes
	}
	return &Store{
		sessions: map[string]*session{},
		clock:    clock, ttl: ttl, maxSess: maxSess, maxBytes: maxBytes,
	}
}

// Put stores data at (id, slot), write-once. It creates the session on first
// write, reaping expired sessions first so a long-idle relay does not refuse a
// new pairing for want of room. A repeat of the same bytes is idempotent; a
// different value to a written slot is ErrSlotConflict.
func (s *Store) Put(id, slot string, data []byte) error {
	if err := validateSession(id); err != nil {
		return err
	}
	if !AllowedSlots[slot] {
		return fmt.Errorf("%w: %q", ErrUnknownSlot, slot)
	}
	if len(data) > s.maxBytes {
		return fmt.Errorf("%w: %d bytes, max %d", ErrTooLarge, len(data), s.maxBytes)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reap()

	sess := s.sessions[id]
	if sess == nil {
		if len(s.sessions) >= s.maxSess {
			return ErrTooManySessions
		}
		sess = &session{slots: map[string][]byte{}, created: s.clock.Now().UTC()}
		s.sessions[id] = sess
	}
	if existing, ok := sess.slots[slot]; ok {
		if !bytesEqual(existing, data) {
			return ErrSlotConflict
		}
		return nil // idempotent
	}
	sess.slots[slot] = append([]byte(nil), data...)
	return nil
}

// Get returns the value at (id, slot), or ErrNotFound. An expired session reads
// as absent — the value is gone whether or not the reaper has run yet.
func (s *Store) Get(id, slot string) ([]byte, error) {
	if err := validateSession(id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reap()
	sess := s.sessions[id]
	if sess == nil {
		return nil, ErrNotFound
	}
	v, ok := sess.slots[slot]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

// SessionCount reports how many live sessions the store holds, for tests and the
// bound assertion. It reaps first so the number is the live one.
func (s *Store) SessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reap()
	return len(s.sessions)
}

// reap drops every session past its TTL. Called under the lock on every access,
// so the table cannot grow unboundedly between requests.
func (s *Store) reap() {
	now := s.clock.Now().UTC()
	for id, sess := range s.sessions {
		if now.Sub(sess.created) > s.ttl {
			delete(s.sessions, id)
		}
	}
}

// validateSession keeps a session id a clean, bounded path segment: a non-empty
// run of URL-safe characters. It refuses a "/" or an over-long id before either
// reaches the table, so the id cannot smuggle a second path segment or bloat a
// map key.
func validateSession(id string) error {
	if id == "" || len(id) > 128 {
		return fmt.Errorf("%w: length %d", ErrBadSession, len(id))
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return fmt.Errorf("%w: %q has a disallowed character", ErrBadSession, id)
		}
	}
	return nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
