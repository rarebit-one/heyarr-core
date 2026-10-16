package store

// members.go is shared-space membership (ADR-0108, §47): which users a space
// is shared with, and so whose enrolled devices its key may be wrapped for.
// Membership is the second thing enrol-before-wrap (ADR-0049) consults, after
// the device pin: a wrap for a device whose user is not a current member of a
// personal, shared or research space is refused (see the API's
// recipientsAllowed). A family space is every household member's by
// definition, so its rows inform and never narrow the gate.
//
// Who may change membership is ADR-0104's owner rule, reused: the recorded
// owner's device, any management-authorised device on a legacy space with no
// owner, and any such device on a family space.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
)

// MemberRole is a member's role in a space.
type MemberRole string

// The roles a membership row carries.
const (
	// RoleOwner is the user who created the space from a device (ADR-0104).
	// There is at most one, it is written by the space creation, and it cannot
	// be removed.
	RoleOwner MemberRole = "owner"
	// RoleMember is a user the owner shared the space with.
	RoleMember MemberRole = "member"
)

// Membership errors.
var (
	// ErrNotMember is a removal of a user who is not a current member.
	ErrNotMember = errors.New("personalstate/store: that user is not a member of the space")
	// ErrOwnerNotRemovable is a removal of the space's owner: a space is never
	// left without the member who owns it.
	ErrOwnerNotRemovable = errors.New("personalstate/store: the owner of a space cannot be removed from it")
	// ErrEmptyMember is a membership change naming no user.
	ErrEmptyMember = errors.New("personalstate/store: a member principal is required")
)

// SpaceMember is one user's membership of one space, current or removed.
type SpaceMember struct {
	SpaceID          string
	PrincipalID      string
	Role             MemberRole
	AddedAt          time.Time
	AddedByPrincipal string
	AddedByDevice    string
	// RemovedAt is set on a tombstone: the user was a member until then, and
	// no new wrap for their devices is admitted while it is set.
	RemovedAt *time.Time
}

// Current reports whether the membership is in force.
func (m SpaceMember) Current() bool { return m.RemovedAt == nil }

// Membership is a space's member set as the wrap gate reads it.
type Membership struct {
	Kind spaces.Kind
	// Current and Removed are principal ids: the users whose devices may be
	// wrapped for, and the users whose devices may not (a tombstone).
	Current map[string]bool
	Removed map[string]bool
}

// Gated reports whether membership narrows the wrap gate for this space: it
// has at least one membership row and is not a family space. A space with no
// rows — one created before membership existed, or by a bearer token — is
// gated only by the device pin, exactly as before ADR-0108.
func (m Membership) Gated() bool {
	return m.Kind != spaces.KindFamily && (len(m.Current) > 0 || len(m.Removed) > 0)
}

// AddMember records (or re-admits) principalID as a member of spaceID, by the
// user principal by through the device key device (ADR-0108). The space must
// exist; for every kind but family, by must own it unless it has no recorded
// owner (ADR-0104's rule, ownerMayAct). Adding the owner, or a user who is
// already a current member, returns the existing row and emits nothing. A
// re-add of a removed member clears the tombstone. It emits
// TypeSpaceMemberAdded in the same transaction (Invariant 7).
//
// Nothing is wrapped here: the caller's device wraps the current key for the
// member's enrolled devices through the re-wrap route, which from now on admits
// them. The server never holds a key to wrap (Invariant 6).
func (s *Store) AddMember(ctx context.Context, spaceID, principalID, by, device string) (SpaceMember, error) {
	if principalID == "" {
		return SpaceMember{}, ErrEmptyMember
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return SpaceMember{}, fmt.Errorf("personalstate/store: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := s.clock.Now().UTC()

	kind, owner, err := s.memberChangeAllowed(ctx, tx, spaceID, by)
	if err != nil {
		return SpaceMember{}, err
	}
	if err := s.ensureOwnerRowTx(ctx, tx, spaceID, owner, now); err != nil {
		return SpaceMember{}, err
	}
	existing, found, err := memberRowTx(ctx, tx, spaceID, principalID)
	if err != nil {
		return SpaceMember{}, err
	}
	if found && existing.Current() {
		// Already in — the owner, or a member added before. Idempotent, no
		// second event.
		return existing, nil
	}
	m := SpaceMember{
		SpaceID: spaceID, PrincipalID: principalID, Role: RoleMember,
		AddedAt: now, AddedByPrincipal: by, AddedByDevice: device,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO space_members (space_id, principal_id, role, added_at, added_by_principal, added_by_device, removed_at)
		VALUES (?, ?, ?, ?, ?, ?, NULL)
		ON CONFLICT (space_id, principal_id) DO UPDATE SET
			role = excluded.role, added_at = excluded.added_at,
			added_by_principal = excluded.added_by_principal,
			added_by_device = excluded.added_by_device, removed_at = NULL`,
		spaceID, principalID, string(RoleMember), now.Format(timeFormat), by, device); err != nil {
		return SpaceMember{}, fmt.Errorf("personalstate/store: recording a member: %w", err)
	}
	ev, err := s.events.EmitTx(ctx, tx, events.TypeSpaceMemberAdded, "encrypted_space", spaceID,
		map[string]any{
			"principal_id": principalID, "role": string(RoleMember), "kind": string(kind),
			"added_by_device": device, "readmitted": found,
		})
	if err != nil {
		return SpaceMember{}, fmt.Errorf("personalstate/store: recording a member: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SpaceMember{}, fmt.Errorf("personalstate/store: committing: %w", err)
	}
	s.events.Publish(ev)
	return m, nil
}

// RemoveMember tombstones principalID's membership of spaceID, under the same
// rule as AddMember (ADR-0108). The owner cannot be removed
// (ErrOwnerNotRemovable); a user who is not a current member is ErrNotMember,
// so a script cannot mistake "already gone" for "removed just now". It emits
// TypeSpaceMemberRemoved in the same transaction.
//
// This closes the gate on NEW wraps for the user's devices, including on a
// rotation, which must name them in revoke. It is not itself forward secrecy:
// the devices keep the copies they hold until a rotation leaves them out
// (ADR-0103), and that rotation is the owner's device's to run.
func (s *Store) RemoveMember(ctx context.Context, spaceID, principalID, by, device string) (SpaceMember, error) {
	if principalID == "" {
		return SpaceMember{}, ErrEmptyMember
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return SpaceMember{}, fmt.Errorf("personalstate/store: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := s.clock.Now().UTC()

	kind, owner, err := s.memberChangeAllowed(ctx, tx, spaceID, by)
	if err != nil {
		return SpaceMember{}, err
	}
	if owner != "" && principalID == owner {
		return SpaceMember{}, ErrOwnerNotRemovable
	}
	existing, found, err := memberRowTx(ctx, tx, spaceID, principalID)
	if err != nil {
		return SpaceMember{}, err
	}
	if !found || !existing.Current() {
		return SpaceMember{}, ErrNotMember
	}
	if existing.Role == RoleOwner {
		return SpaceMember{}, ErrOwnerNotRemovable
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE space_members SET removed_at = ? WHERE space_id = ? AND principal_id = ?`,
		now.Format(timeFormat), spaceID, principalID); err != nil {
		return SpaceMember{}, fmt.Errorf("personalstate/store: removing a member: %w", err)
	}
	ev, err := s.events.EmitTx(ctx, tx, events.TypeSpaceMemberRemoved, "encrypted_space", spaceID,
		map[string]any{"principal_id": principalID, "kind": string(kind), "removed_by_device": device})
	if err != nil {
		return SpaceMember{}, fmt.Errorf("personalstate/store: removing a member: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return SpaceMember{}, fmt.Errorf("personalstate/store: committing: %w", err)
	}
	s.events.Publish(ev)
	existing.RemovedAt = &now
	return existing, nil
}

// Members lists a space's membership rows, current and removed, oldest first.
// The space must exist. A removed row is returned with RemovedAt set: it is
// what the owner's device reads to know which recipients a rotation leaves out.
func (s *Store) Members(ctx context.Context, spaceID string) ([]SpaceMember, error) {
	if _, err := s.Space(ctx, spaceID); err != nil {
		return nil, err
	}
	rows, err := s.reader.QueryContext(ctx, `
		SELECT space_id, principal_id, role, added_at, added_by_principal, added_by_device, removed_at
		FROM space_members WHERE space_id = ?
		ORDER BY CASE role WHEN 'owner' THEN 0 ELSE 1 END, added_at, principal_id`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("personalstate/store: listing members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []SpaceMember{}
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SpaceMembership is the member set as the wrap gate reads it: the space's
// kind and the current and removed principal ids. The space must exist.
func (s *Store) SpaceMembership(ctx context.Context, spaceID string) (Membership, error) {
	sp, err := s.Space(ctx, spaceID)
	if err != nil {
		return Membership{}, err
	}
	rows, err := s.reader.QueryContext(ctx,
		`SELECT principal_id, removed_at FROM space_members WHERE space_id = ?`, spaceID)
	if err != nil {
		return Membership{}, fmt.Errorf("personalstate/store: reading membership: %w", err)
	}
	defer func() { _ = rows.Close() }()
	m := Membership{Kind: sp.Kind, Current: map[string]bool{}, Removed: map[string]bool{}}
	for rows.Next() {
		var (
			id      string
			removed sql.NullString
		)
		if err := rows.Scan(&id, &removed); err != nil {
			return Membership{}, fmt.Errorf("personalstate/store: reading membership: %w", err)
		}
		if removed.Valid {
			m.Removed[id] = true
		} else {
			m.Current[id] = true
		}
	}
	return m, rows.Err()
}

// memberChangeAllowed checks, inside tx, that spaceID exists and that by may
// change its membership: on a family space anyone may (ADR-0108's owner-less
// rule), on every other kind by must own it or it must have no recorded owner
// (ADR-0104). It returns the kind and the recorded owner ("" for none).
func (s *Store) memberChangeAllowed(ctx context.Context, tx *sql.Tx, spaceID, by string) (spaces.Kind, string, error) {
	var (
		kind  string
		owner sql.NullString
	)
	err := tx.QueryRowContext(ctx, `SELECT kind, owner_principal_id FROM encrypted_spaces WHERE id = ?`, spaceID).
		Scan(&kind, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrUnknownSpace
	}
	if err != nil {
		return "", "", fmt.Errorf("personalstate/store: reading a space: %w", err)
	}
	if spaces.Kind(kind) != spaces.KindFamily && owner.Valid && owner.String != by {
		return "", "", ErrNotSpaceOwner
	}
	return spaces.Kind(kind), owner.String, nil
}

// ensureOwnerRowTx writes the owner's membership row when the space has a
// recorded owner and no row yet — a space created before membership existed,
// gated from its first membership change on. A space with no owner gets none.
func (s *Store) ensureOwnerRowTx(ctx context.Context, tx *sql.Tx, spaceID, owner string, now time.Time) error {
	if owner == "" {
		return nil
	}
	_, found, err := memberRowTx(ctx, tx, spaceID, owner)
	if err != nil || found {
		return err
	}
	return insertOwnerRowTx(ctx, tx, spaceID, owner, "", now)
}

// insertOwnerRowTx records owner as the space's owner member.
func insertOwnerRowTx(ctx context.Context, tx *sql.Tx, spaceID, owner, device string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO space_members (space_id, principal_id, role, added_at, added_by_principal, added_by_device, removed_at)
		VALUES (?, ?, ?, ?, ?, ?, NULL)`,
		spaceID, owner, string(RoleOwner), now.Format(timeFormat), owner, device); err != nil {
		return fmt.Errorf("personalstate/store: recording the owner as a member: %w", err)
	}
	return nil
}

func memberRowTx(ctx context.Context, tx *sql.Tx, spaceID, principalID string) (SpaceMember, bool, error) {
	row := tx.QueryRowContext(ctx, `
		SELECT space_id, principal_id, role, added_at, added_by_principal, added_by_device, removed_at
		FROM space_members WHERE space_id = ? AND principal_id = ?`, spaceID, principalID)
	m, err := scanMember(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SpaceMember{}, false, nil
	}
	if err != nil {
		return SpaceMember{}, false, err
	}
	return m, true, nil
}

func scanMember(row interface{ Scan(dest ...any) error }) (SpaceMember, error) {
	var (
		m           SpaceMember
		role, added string
		removed     sql.NullString
	)
	if err := row.Scan(&m.SpaceID, &m.PrincipalID, &role, &added, &m.AddedByPrincipal, &m.AddedByDevice, &removed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SpaceMember{}, err
		}
		return SpaceMember{}, fmt.Errorf("personalstate/store: reading a member: %w", err)
	}
	m.Role = MemberRole(role)
	var err error
	if m.AddedAt, err = time.Parse(timeFormat, added); err != nil {
		return SpaceMember{}, fmt.Errorf("personalstate/store: a member has an unparseable added_at: %w", err)
	}
	if removed.Valid {
		t, err := time.Parse(timeFormat, removed.String)
		if err != nil {
			return SpaceMember{}, fmt.Errorf("personalstate/store: a member has an unparseable removed_at: %w", err)
		}
		m.RemovedAt = &t
	}
	return m, nil
}
