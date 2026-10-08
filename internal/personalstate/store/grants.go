package store

// grants.go is the per-space access list (ADR-0104): the FETCH gate of
// ADR-0049's two orthogonal gates, applied to restricted principals. A grant
// lets an executor fetch (and, with write, push) a space's ciphertext; it never
// lets it read plaintext — that still needs a wrapped key it separately holds.
// Grants are this node's fact and do not replicate: an executor talks to the
// node that granted it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
)

// The capability sets a grant may carry, as stored.
const (
	// CapsRead fetches the grantee's own wrap, the key history, changes and
	// snapshots.
	CapsRead = "read"
	// CapsReadWrite may also push changes and snapshots.
	CapsReadWrite = "read,write"
)

// Grant errors.
var (
	// ErrInvalidCaps is a grant naming a capability set other than read or
	// read,write.
	ErrInvalidCaps = errors.New("personalstate/store: a grant's caps must be read or read,write")
	// ErrNotSpaceOwner is a grant or revoke by a principal that does not own the
	// space. A space with no recorded owner (a legacy household space) has no
	// such restriction.
	ErrNotSpaceOwner = errors.New("personalstate/store: only the space's owner may grant or revoke access to it")
	// ErrNoGrant is a revoke of a grant that is not active.
	ErrNoGrant = errors.New("personalstate/store: no active grant for that principal on that space")
	// ErrInvalidExpiry is a grant whose expiry is not in the future: it would be
	// inactive the moment it was written.
	ErrInvalidExpiry = errors.New("personalstate/store: a grant's expiry must be in the future")
)

// SpaceGrant is one principal's access to one space.
type SpaceGrant struct {
	SpaceID            string
	PrincipalID        string
	Caps               string
	GrantedByPrincipal string
	GrantedByDevice    string
	GrantedAt          time.Time
	ExpiresAt          *time.Time
}

// Writes reports whether the grant lets its holder push changes and snapshots.
func (g SpaceGrant) Writes() bool { return g.Caps == CapsReadWrite }

// GrantAccess records (or renews) principalID's access to spaceID, granted by
// the user principal by through the device key device (ADR-0104). The space
// must exist, and by must own it unless it has no recorded owner. A re-grant
// replaces the caps and expiry and clears a revocation. It emits
// TypeSpaceAccessGranted in the same transaction (Invariant 7).
//
// wraps are copies of the space's current key wrapped for the executor's
// service recipients, recorded in the SAME transaction as the grant, so the
// fetch gate and the decryption gate open together or not at all. Each must be
// for a live service recipient of principalID (ErrNotServiceRecipient) and seal
// the current key epoch (ErrStaleKeyEpoch, ErrFutureKeyEpoch). A grant with no
// wraps opens the fetch gate alone, as before.
func (s *Store) GrantAccess(ctx context.Context, spaceID, principalID, caps, by, device string, expiresAt *time.Time, wraps ...GrantWrap) (SpaceGrant, error) {
	if caps != CapsRead && caps != CapsReadWrite {
		return SpaceGrant{}, ErrInvalidCaps
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return SpaceGrant{}, fmt.Errorf("personalstate/store: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// The clock is read only once the single writer connection is held, so a
	// call that queued behind another writer cannot admit a grant (and its
	// wraps) that expired while it waited. This same instant decides wrap
	// eligibility below.
	now := s.clock.Now().UTC()
	if expiresAt != nil && !expiresAt.After(now) {
		return SpaceGrant{}, ErrInvalidExpiry
	}

	if err := ownerMayAct(ctx, tx, spaceID, by); err != nil {
		return SpaceGrant{}, err
	}
	var expires any
	if expiresAt != nil {
		expires = expiresAt.UTC().Format(timeFormat)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO space_grants (space_id, principal_id, caps, granted_by_principal, granted_by_device, granted_at, expires_at, revoked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL)
		ON CONFLICT (space_id, principal_id) DO UPDATE SET
			caps = excluded.caps, granted_by_principal = excluded.granted_by_principal,
			granted_by_device = excluded.granted_by_device, granted_at = excluded.granted_at,
			expires_at = excluded.expires_at, revoked_at = NULL`,
		spaceID, principalID, caps, by, device, now.Format(timeFormat), expires); err != nil {
		return SpaceGrant{}, fmt.Errorf("personalstate/store: recording a grant: %w", err)
	}
	wrapEvs, err := s.putGrantWrapsTx(ctx, tx, spaceID, principalID, wraps, now)
	if err != nil {
		return SpaceGrant{}, err
	}
	payload := map[string]any{"principal_id": principalID, "caps": caps, "granted_by_device": device}
	if expiresAt != nil {
		payload["expires_at"] = expiresAt.UTC().Format(timeFormat)
	}
	if len(wraps) > 0 {
		recips := make([]string, 0, len(wraps))
		for _, w := range wraps {
			recips = append(recips, w.Recipient)
		}
		payload["wrapped_for"] = recips
	}
	ev, err := s.events.EmitTx(ctx, tx, events.TypeSpaceAccessGranted, "encrypted_space", spaceID, payload)
	if err != nil {
		return SpaceGrant{}, fmt.Errorf("personalstate/store: recording a grant: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SpaceGrant{}, fmt.Errorf("personalstate/store: committing: %w", err)
	}
	s.events.Publish(append(wrapEvs, ev)...)
	return SpaceGrant{
		SpaceID: spaceID, PrincipalID: principalID, Caps: caps,
		GrantedByPrincipal: by, GrantedByDevice: device, GrantedAt: now, ExpiresAt: expiresAt,
	}, nil
}

// RevokeAccess withdraws principalID's access to spaceID (ADR-0104), under the
// same ownership rule as GrantAccess, and in the same transaction deletes every
// copy of the space's key wrapped for that executor's service recipients. Both
// gates close together. It returns the recipients whose copy went.
//
// Revoking when there is neither an active grant nor a copy to delete is
// ErrNoGrant, so a script cannot mistake "already gone" for "revoked just now".
// An expired grant is not active, but its executor's copies, and a lingering
// copy with no grant (a wrap a peer replicated back, ADR-0103), are still
// deleted. It emits TypeSpaceAccessRevoked when it revoked an active grant, and
// TypeSpaceKeyRevoked per deleted copy, in the same transaction.
//
// This is not forward secrecy: the executor keeps any key it already
// unwrapped. Only a rotation that leaves it out does that (ADR-0103).
func (s *Store) RevokeAccess(ctx context.Context, spaceID, principalID, by, device string) ([]string, error) {
	now := s.clock.Now().UTC()
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("personalstate/store: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := ownerMayAct(ctx, tx, spaceID, by); err != nil {
		return nil, err
	}
	// An expired grant is already inactive (ActiveGrant treats it so), so it is
	// not revoked again and earns no revocation event. Checked against the
	// store's clock in Go, because expires_at is RFC3339Nano text and does not
	// compare reliably as a string. Its executor's copies are still deleted.
	active := true
	var expires sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT expires_at FROM space_grants WHERE space_id = ? AND principal_id = ? AND revoked_at IS NULL`,
		spaceID, principalID).Scan(&expires)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		active = false
	case err != nil:
		return nil, fmt.Errorf("personalstate/store: reading a grant: %w", err)
	case expires.Valid:
		exp, perr := time.Parse(timeFormat, expires.String)
		if perr != nil {
			return nil, fmt.Errorf("personalstate/store: a grant has an unparseable expires_at: %w", perr)
		}
		active = now.Before(exp)
	}
	if active {
		if _, err := tx.ExecContext(ctx,
			`UPDATE space_grants SET revoked_at = ? WHERE space_id = ? AND principal_id = ? AND revoked_at IS NULL`,
			now.Format(timeFormat), spaceID, principalID); err != nil {
			return nil, fmt.Errorf("personalstate/store: revoking a grant: %w", err)
		}
	}
	// Every key the executor ever registered, live or removed: a removed one's
	// copies went with it, so this only ever finds copies of live ones, or of a
	// key a peer pushed back.
	keys, err := recipientSet(ctx, tx, `SELECT recipient FROM service_recipients WHERE principal_id = ?`, principalID)
	if err != nil {
		return nil, err
	}
	list := make([]string, 0, len(keys))
	for k := range keys {
		list = append(list, k)
	}
	slices.Sort(list)
	hit, evs, err := s.deleteWrapsForTx(ctx, tx, spaceID, list)
	if err != nil {
		return nil, err
	}
	if !active && len(hit) == 0 {
		return nil, ErrNoGrant
	}
	dropped := make([]string, 0, len(hit))
	for _, g := range hit {
		dropped = append(dropped, g.recipient)
	}
	if active {
		ev, err := s.events.EmitTx(ctx, tx, events.TypeSpaceAccessRevoked, "encrypted_space", spaceID,
			map[string]any{"principal_id": principalID, "revoked_by_device": device, "wraps_deleted": len(dropped)})
		if err != nil {
			return nil, fmt.Errorf("personalstate/store: revoking a grant: %w", err)
		}
		evs = append(evs, ev)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("personalstate/store: committing: %w", err)
	}
	s.events.Publish(evs...)
	return dropped, nil
}

// ownerMayAct checks, inside tx, that spaceID exists and that by owns it or it
// has no recorded owner.
func ownerMayAct(ctx context.Context, tx *sql.Tx, spaceID, by string) error {
	var owner sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT owner_principal_id FROM encrypted_spaces WHERE id = ?`, spaceID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnknownSpace
	}
	if err != nil {
		return fmt.Errorf("personalstate/store: reading a space's owner: %w", err)
	}
	if owner.Valid && owner.String != by {
		return ErrNotSpaceOwner
	}
	return nil
}

// ActiveGrant returns principalID's grant on spaceID if it is active now — not
// revoked and not expired. ok is false otherwise, including for an unknown
// space, so a caller cannot tell "no such space" from "not yours".
func (s *Store) ActiveGrant(ctx context.Context, spaceID, principalID string) (SpaceGrant, bool, error) {
	var (
		g                SpaceGrant
		grantedAt        string
		expires, revoked sql.NullString
	)
	err := s.reader.QueryRowContext(ctx, `
		SELECT space_id, principal_id, caps, granted_by_principal, granted_by_device, granted_at, expires_at, revoked_at
		FROM space_grants WHERE space_id = ? AND principal_id = ?`, spaceID, principalID).
		Scan(&g.SpaceID, &g.PrincipalID, &g.Caps, &g.GrantedByPrincipal, &g.GrantedByDevice, &grantedAt, &expires, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return SpaceGrant{}, false, nil
	}
	if err != nil {
		return SpaceGrant{}, false, fmt.Errorf("personalstate/store: reading a grant: %w", err)
	}
	if revoked.Valid {
		return SpaceGrant{}, false, nil
	}
	if g.GrantedAt, err = time.Parse(timeFormat, grantedAt); err != nil {
		return SpaceGrant{}, false, fmt.Errorf("personalstate/store: a grant has an unparseable granted_at: %w", err)
	}
	if expires.Valid {
		exp, err := time.Parse(timeFormat, expires.String)
		if err != nil {
			return SpaceGrant{}, false, fmt.Errorf("personalstate/store: a grant has an unparseable expires_at: %w", err)
		}
		if !s.clock.Now().Before(exp) {
			return SpaceGrant{}, false, nil
		}
		g.ExpiresAt = &exp
	}
	return g, true, nil
}

// GrantedSpaceIDs is the set of spaces principalID holds an active grant on —
// what a restricted caller's space listing is filtered to.
func (s *Store) GrantedSpaceIDs(ctx context.Context, principalID string) (map[string]bool, error) {
	return s.grantedSpaces(ctx, principalID, false)
}

// HasAnyGrant reports whether principalID holds an active grant of any kind on
// at least one space — the floor for a restricted caller's blob reads.
func (s *Store) HasAnyGrant(ctx context.Context, principalID string) (bool, error) {
	set, err := s.grantedSpaces(ctx, principalID, false)
	return len(set) > 0, err
}

// HasAnyWriteGrant reports whether principalID holds an active write grant on
// at least one space. It satisfies httpapi.SpaceWriteGrants.
func (s *Store) HasAnyWriteGrant(ctx context.Context, principalID string) (bool, error) {
	set, err := s.grantedSpaces(ctx, principalID, true)
	return len(set) > 0, err
}

func (s *Store) grantedSpaces(ctx context.Context, principalID string, writeOnly bool) (map[string]bool, error) {
	q := `SELECT space_id, expires_at FROM space_grants WHERE principal_id = ? AND revoked_at IS NULL`
	if writeOnly {
		q += ` AND caps = '` + CapsReadWrite + `'`
	}
	rows, err := s.reader.QueryContext(ctx, q, principalID)
	if err != nil {
		return nil, fmt.Errorf("personalstate/store: listing grants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	now := s.clock.Now()
	out := map[string]bool{}
	for rows.Next() {
		var (
			id      string
			expires sql.NullString
		)
		if err := rows.Scan(&id, &expires); err != nil {
			return nil, fmt.Errorf("personalstate/store: listing grants: %w", err)
		}
		if expires.Valid {
			exp, err := time.Parse(timeFormat, expires.String)
			if err != nil {
				return nil, fmt.Errorf("personalstate/store: a grant has an unparseable expires_at: %w", err)
			}
			if !now.Before(exp) {
				continue
			}
		}
		out[id] = true
	}
	return out, rows.Err()
}
