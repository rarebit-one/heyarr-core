package store

// memberwrite.go answers ADR-0128's question: may this device write to this
// space as one of its members? It is asked for a Device credential that
// carries only the read floor (ADR-0067), and it lifts that device to write
// on ONE space, per request, never on its identity.
//
// The answer follows the space key. A device may write where it holds a copy
// of the space's CURRENT key (it can seal what it pushes, and a rotation that
// left it out has already shut it out), and where the membership gate of
// ADR-0108 would admit its user today. A removed member's device that still
// holds a stale-but-current copy, because no rotation has run yet, is refused
// by the tombstone.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
)

// MemberHoldsCurrentKey reports whether recipient — the encryption key of one
// of principalID's devices — holds a copy of spaceID's current key, and the
// space's membership admits principalID (ADR-0128). An unknown space is false,
// not an error: a device that cannot write to it is refused the same way
// whether or not it exists.
func (s *Store) MemberHoldsCurrentKey(ctx context.Context, spaceID, principalID, recipient string) (bool, error) {
	if spaceID == "" || principalID == "" || recipient == "" {
		return false, nil
	}
	tx, err := s.reader.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("personalstate/store: beginning read transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ok, err := memberHoldsCurrentKeyTx(ctx, tx, spaceID, principalID, recipient)
	if err != nil {
		return false, err
	}
	return ok, tx.Commit()
}

// MemberHoldsAnyCurrentKey reports whether MemberHoldsCurrentKey holds for at
// least one space. It is the vault routes' question: a blob and a pin carry no
// space (ADR-0096, Invariant 6), so "a space this device may write" is the
// strongest check they can make — the same one ADR-0104 makes for an executor.
func (s *Store) MemberHoldsAnyCurrentKey(ctx context.Context, principalID, recipient string) (bool, error) {
	if principalID == "" || recipient == "" {
		return false, nil
	}
	tx, err := s.reader.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("personalstate/store: beginning read transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	held, err := currentWrapSpacesTx(ctx, tx, recipient)
	if err != nil {
		return false, err
	}
	for _, spaceID := range held {
		ok, err := memberAdmitsTx(ctx, tx, spaceID, principalID)
		if err != nil {
			return false, err
		}
		if ok {
			return true, tx.Commit()
		}
	}
	return false, tx.Commit()
}

// currentWrapSpacesTx is the spaces where recipient holds a copy at the
// space's current epoch.
func currentWrapSpacesTx(ctx context.Context, tx *sql.Tx, recipient string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT w.space_id FROM wrapped_keys w
		WHERE w.recipient = ?
		  AND w.epoch = (SELECT COALESCE(MAX(h.epoch), 0) FROM space_key_history h WHERE h.space_id = w.space_id)
		ORDER BY w.space_id`, recipient)
	if err != nil {
		return nil, fmt.Errorf("personalstate/store: listing a recipient's current wraps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var held []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("personalstate/store: reading a recipient's current wraps: %w", err)
		}
		held = append(held, id)
	}
	return held, rows.Err()
}

func memberHoldsCurrentKeyTx(ctx context.Context, tx *sql.Tx, spaceID, principalID, recipient string) (bool, error) {
	if err := spaceExistsTx(ctx, tx, spaceID); err != nil {
		if errors.Is(err, ErrUnknownSpace) {
			return false, nil
		}
		return false, err
	}
	epoch, err := keyEpochTx(ctx, tx, spaceID)
	if err != nil {
		return false, err
	}
	var one int
	err = tx.QueryRowContext(ctx,
		`SELECT 1 FROM wrapped_keys WHERE space_id = ? AND recipient = ? AND epoch = ?`,
		spaceID, recipient, epoch).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("personalstate/store: reading a current wrap: %w", err)
	}
	return memberAdmitsTx(ctx, tx, spaceID, principalID)
}

// memberAdmitsTx is the membership half of the rule, the same gate ADR-0108
// puts on a wrap: a family space admits every household user; a removed
// member is refused; the owner and a current member are admitted; and a space
// with neither an owner nor membership rows — a legacy household space created
// by a bearer token — is gated by the key alone, exactly as its wraps are. An
// owned space with no rows yet admits only its owner: write is narrower than
// the wrap gate there, never wider.
func memberAdmitsTx(ctx context.Context, tx *sql.Tx, spaceID, principalID string) (bool, error) {
	var (
		kind  string
		owner sql.NullString
	)
	if err := tx.QueryRowContext(ctx, `SELECT kind, owner_principal_id FROM encrypted_spaces WHERE id = ?`, spaceID).
		Scan(&kind, &owner); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("personalstate/store: reading a space: %w", err)
	}
	if spaces.Kind(kind) == spaces.KindFamily {
		return true, nil
	}
	var rows int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM space_members WHERE space_id = ?`, spaceID).
		Scan(&rows); err != nil {
		return false, fmt.Errorf("personalstate/store: counting members: %w", err)
	}
	m, found, err := memberRowTx(ctx, tx, spaceID, principalID)
	if err != nil {
		return false, err
	}
	switch {
	case found && !m.Current():
		return false, nil
	case found, owner.Valid && owner.String == principalID:
		return true, nil
	default:
		return rows == 0 && !owner.Valid, nil
	}
}
