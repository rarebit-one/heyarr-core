// Package leases issues and honours the Milestone 8 cross-site access lease
// (§54, ADR-0048, #285): a capability grant this peer signs with its own
// identity, so a peer that has this one in its membership can honour it without
// reaching back — the property ADR-0038 could not otherwise give a capability
// that spans sites.
//
// A lease IS an internal/grant.Grant. This package adds the two things a grant
// alone does not have: an issuer that signs with the peer's Ed25519 identity
// (ADR-0012), and a durable record so a lease can be listed, revoked and (in
// #285) cached to sibling peers ahead of an outage. Honouring a PRESENTED token
// needs only the token and the pinned issuer key — a peer that lost this table
// still honours a lease it holds, which is the degraded-read property (§53).
//
// Revocation is asymmetric on purpose, and it is the whole ADR-0048 argument:
// the ISSUER refuses a lease it has revoked (it holds the row and checks it);
// a peer honouring a sibling's cached lease across a partition cannot, and
// honours it until it expires. The 24h cap (grant.MaxTTL) is what bounds that.
package leases

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/grant"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

const timeFormat = time.RFC3339Nano

// Clock is injected so expiry is a unit fact, not a sleep (ADR-0017).
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// Errors the store refuses with.
var (
	// ErrLeaseRevoked is a lease the issuer has revoked. Distinct from
	// grant.ErrExpired: revoked is a decision, expired is the clock, and a
	// sibling honouring across a partition sees only the second.
	ErrLeaseRevoked = errors.New("leases: lease is revoked")
	// ErrUnknownLease names a lease this store does not hold.
	ErrUnknownLease = errors.New("leases: no such lease")
	// ErrNoSigner is a store asked to issue without an identity key.
	ErrNoSigner = errors.New("leases: an issuer signing key is required to issue")
)

// A Lease is an issued grant and this issuer's record of it.
type Lease struct {
	ID           string
	Principal    string
	Resource     string
	Capabilities []grant.Capability
	Issuer       string // the issuing peer's rendered public key
	Token        string // the signed grant.Grant token — the authority
	IssuedAt     time.Time
	ExpiresAt    time.Time
	RevokedAt    *time.Time
}

// Store is the access_leases table plus this peer's issuer identity.
type Store struct {
	writer   *sql.DB
	reader   *sql.DB
	clock    Clock
	events   *events.Log
	signer   ed25519.PrivateKey
	issuerID string
	siblings SiblingKeys
}

// Options configure a Store.
type Options struct {
	// Writer is the single-writer pool (ADR-0003).
	Writer *sql.DB
	// Reader serves honour-time reads off the write path.
	Reader *sql.DB
	// Events records issuance and revocation (invariant 7). Required.
	Events *events.Log
	// Signer is this peer's Ed25519 identity key (identity.Signer). Required to
	// ISSUE; a store built without it can still honour and revoke leases others
	// issued, which is what a read-only replica does.
	Signer ed25519.PrivateKey
	// Siblings supplies the peer identities a lease may ALSO be verified against
	// (ADR-0012 membership). It is what makes a lease minted at site A honourable
	// at site B: B trusts A because B has A pinned. Nil means "this peer's own
	// leases only" — correct for a single-peer deployment, and the #304 shape
	// before this widening. The lookup is at honour time, so enrolling or
	// revoking a peer takes effect on the next honoured lease, not at restart.
	Siblings SiblingKeys
	Clock    Clock
}

// SiblingKeys supplies the pinned public keys of enrolled peers, keyed by their
// rendered form ("ed25519:<hex>"). A membership store satisfies it through a
// thin adapter; a test satisfies it with a map. It is deliberately narrow — the
// leases package needs the trust set, not the membership model.
type SiblingKeys interface {
	PeerKeys(ctx context.Context) (map[string]ed25519.PublicKey, error)
}

// New constructs a Store.
func New(opts Options) (*Store, error) {
	if opts.Writer == nil {
		return nil, errors.New("leases: a writer database is required")
	}
	if opts.Events == nil {
		return nil, errors.New("leases: an event log is required (invariant 7)")
	}
	reader := opts.Reader
	if reader == nil {
		reader = opts.Writer
	}
	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}
	s := &Store{writer: opts.Writer, reader: reader, clock: clock, events: opts.Events, signer: opts.Signer, siblings: opts.Siblings}
	if len(opts.Signer) == ed25519.PrivateKeySize {
		s.issuerID = identity.FormatPublicKey(opts.Signer.Public().(ed25519.PublicKey))
	}
	return s, nil
}

// Issue mints, signs, stores and records a lease for a principal to exercise
// capabilities on a resource, for ttl. The TTL cap (grant.MaxTTL, 24h) is
// enforced by grant.Sign, so an over-long lease cannot be minted.
func (s *Store) Issue(ctx context.Context, principal, resource string, caps []grant.Capability, ttl time.Duration) (Lease, error) {
	if len(s.signer) != ed25519.PrivateKeySize {
		return Lease{}, ErrNoSigner
	}
	now := s.clock.Now().UTC()
	g := grant.Grant{
		Issuer:       s.issuerID,
		Principal:    principal,
		Resource:     resource,
		Capabilities: caps,
		IssuedAt:     now,
		ExpiresAt:    now.Add(ttl),
	}
	token, err := g.Sign(s.signer)
	if err != nil {
		return Lease{}, fmt.Errorf("leases: signing: %w", err)
	}

	id := uuid.Must(uuid.NewV7()).String()
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, fmt.Errorf("leases: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO access_leases (id, principal, resource, capabilities, issuer, token, issued_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, principal, resource, joinCaps(caps), s.issuerID, token,
		now.Format(timeFormat), g.ExpiresAt.UTC().Format(timeFormat)); err != nil {
		return Lease{}, fmt.Errorf("leases: storing lease: %w", err)
	}
	ev, err := s.events.EmitTx(ctx, tx, events.TypeLeaseIssued, "lease", id,
		map[string]any{"principal": principal, "resource": resource, "capabilities": joinCaps(caps), "expires_at": g.ExpiresAt.UTC()})
	if err != nil {
		return Lease{}, fmt.Errorf("leases: recording issuance: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Lease{}, fmt.Errorf("leases: committing: %w", err)
	}
	s.events.Publish(ev)
	return Lease{
		ID: id, Principal: principal, Resource: resource, Capabilities: caps,
		Issuer: s.issuerID, Token: token, IssuedAt: now, ExpiresAt: g.ExpiresAt.UTC(),
	}, nil
}

// Honour verifies a presented lease token against the request and the trust
// store, and refuses one this issuer has revoked.
//
// The trust store is this peer's OWN key plus every enrolled sibling's (Options.
// Siblings) — which is the whole cross-site property: a lease minted at site A
// verifies at site B because B has A pinned, and B reaches NOBODY to check it.
// The signature does all the work the network would otherwise have to; that is
// what makes a lease honourable "whether or not the two peers can reach each
// other" (ADR-0048).
//
// Revocation is asymmetric, by design. A lease THIS store issued and has since
// revoked is refused here (the reachable-issuer path holds the row). A sibling's
// cached lease has no row here, so this peer honours it on its signature until
// it expires — the stated consequence that the 24h cap (grant.MaxTTL) bounds,
// not a bug.
func (s *Store) Honour(ctx context.Context, token string, req grant.Request, now time.Time) (grant.Grant, error) {
	trust, err := s.trustStore(ctx)
	if err != nil {
		return grant.Grant{}, err
	}
	g, err := grant.Verify(token, trust, req, now)
	if err != nil {
		return grant.Grant{}, err
	}
	revoked, err := s.tokenRevoked(ctx, token)
	if err != nil {
		return grant.Grant{}, err
	}
	if revoked {
		return grant.Grant{}, ErrLeaseRevoked
	}
	return g, nil
}

// trustStore is this peer's own key plus every enrolled sibling's. It is built
// per honour so enrolling or revoking a peer takes effect on the next request,
// the same "read the trust root every time" property peer membership already
// has — a revoked peer's leases stop verifying at once, not at the next restart.
func (s *Store) trustStore(ctx context.Context) (grant.Keys, error) {
	trust := grant.Keys{}
	if s.issuerID != "" {
		trust[s.issuerID] = s.signer.Public().(ed25519.PublicKey)
	}
	if s.siblings != nil {
		keys, err := s.siblings.PeerKeys(ctx)
		if err != nil {
			return nil, fmt.Errorf("leases: reading sibling keys: %w", err)
		}
		for id, pub := range keys {
			// self already present; a sibling never overrides it.
			if _, ok := trust[id]; !ok {
				trust[id] = pub
			}
		}
	}
	return trust, nil
}

// Revoke tombstones a lease by id. Idempotent.
func (s *Store) Revoke(ctx context.Context, id string) (Lease, error) {
	lease, err := s.get(ctx, id)
	if err != nil {
		return Lease{}, err
	}
	if lease.RevokedAt != nil {
		return lease, nil
	}
	now := s.clock.Now().UTC()
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, fmt.Errorf("leases: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE access_leases SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		now.Format(timeFormat), id); err != nil {
		return Lease{}, fmt.Errorf("leases: revoking: %w", err)
	}
	ev, err := s.events.EmitTx(ctx, tx, events.TypeLeaseRevoked, "lease", id,
		map[string]any{"principal": lease.Principal, "resource": lease.Resource})
	if err != nil {
		return Lease{}, fmt.Errorf("leases: recording revocation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Lease{}, fmt.Errorf("leases: committing: %w", err)
	}
	s.events.Publish(ev)
	lease.RevokedAt = &now
	return lease, nil
}

// List returns all leases, most-recently-issued first.
func (s *Store) List(ctx context.Context) ([]Lease, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT id, principal, resource, capabilities, issuer, token, issued_at, expires_at, revoked_at
		 FROM access_leases ORDER BY issued_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("leases: listing: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Lease{}
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) get(ctx context.Context, id string) (Lease, error) {
	row := s.reader.QueryRowContext(ctx,
		`SELECT id, principal, resource, capabilities, issuer, token, issued_at, expires_at, revoked_at
		 FROM access_leases WHERE id = ?`, id)
	return scanLease(row)
}

func (s *Store) tokenRevoked(ctx context.Context, token string) (bool, error) {
	var revoked sql.NullString
	err := s.reader.QueryRowContext(ctx,
		`SELECT revoked_at FROM access_leases WHERE token = ?`, token).Scan(&revoked)
	if errors.Is(err, sql.ErrNoRows) {
		// Not a lease this store issued — a sibling's cached lease. It is
		// honoured on its signature alone; this issuer has no revocation say
		// over it, which is the cross-site model.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("leases: checking revocation: %w", err)
	}
	return revoked.Valid && revoked.String != "", nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanLease(row rowScanner) (Lease, error) {
	var l Lease
	var caps, issued, expires string
	var revoked sql.NullString
	err := row.Scan(&l.ID, &l.Principal, &l.Resource, &caps, &l.Issuer, &l.Token, &issued, &expires, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, ErrUnknownLease
	}
	if err != nil {
		return Lease{}, fmt.Errorf("leases: reading lease: %w", err)
	}
	l.Capabilities = splitCaps(caps)
	if l.IssuedAt, err = time.Parse(timeFormat, issued); err != nil {
		return Lease{}, fmt.Errorf("leases: lease %s has an unparseable issued_at: %w", l.ID, err)
	}
	if l.ExpiresAt, err = time.Parse(timeFormat, expires); err != nil {
		return Lease{}, fmt.Errorf("leases: lease %s has an unparseable expires_at: %w", l.ID, err)
	}
	if revoked.Valid && revoked.String != "" {
		t, err := time.Parse(timeFormat, revoked.String)
		if err != nil {
			return Lease{}, fmt.Errorf("leases: lease %s has an unparseable revoked_at: %w", l.ID, err)
		}
		l.RevokedAt = &t
	}
	return l, nil
}

func joinCaps(caps []grant.Capability) string {
	parts := make([]string, len(caps))
	for i, c := range caps {
		parts[i] = string(c)
	}
	return strings.Join(parts, ",")
}

func splitCaps(s string) []grant.Capability {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]grant.Capability, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, grant.Capability(p))
		}
	}
	return out
}
