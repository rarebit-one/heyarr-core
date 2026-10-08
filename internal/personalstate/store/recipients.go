package store

// recipients.go holds service recipients (ADR-0104): an executor's X25519 public
// key, registered by a management-authorised device as a wrap recipient for one
// executor principal. It is the second way a key becomes a pinned wrap target
// under enrol-before-wrap (ADR-0049), beside an enrolled device and a recovery
// key, and it is narrower than both: the key is a valid target only on a space
// where its principal holds an active grant.
//
// The store keeps the public key as an opaque recipient string, exactly as it
// keeps every wrapped_keys recipient. It cannot unwrap anything for it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/events"
)

// Service-recipient errors.
var (
	// ErrRecipientTaken is a registration of a key that is already a live
	// service recipient of another principal.
	ErrRecipientTaken = errors.New("personalstate/store: that key is already registered as another executor's recipient")
	// ErrUnknownRecipient is a removal of a registration that does not exist or
	// was already removed.
	ErrUnknownRecipient = errors.New("personalstate/store: no such service recipient")
	// ErrNotServiceRecipient is a grant carrying a wrap for a key that is not a
	// live service recipient of the principal being granted. That is
	// enrol-before-wrap (ADR-0049) for service recipients: no wrap for a key an
	// owner's device did not register for that executor.
	ErrNotServiceRecipient = errors.New("personalstate/store: the wrap's recipient is not a registered service recipient of that executor")
	// ErrServiceRecipientUngranted is a wrap for a live service recipient whose
	// executor holds no active grant on the space at the moment of the write.
	// Checked inside the transaction that stores the wrap, so a revoke or an
	// expiry landing after an earlier check can never be undone by the write.
	ErrServiceRecipientUngranted = errors.New("personalstate/store: the recipient is a service recipient whose executor holds no active grant on the space")
	// ErrRecipientIsMemberKey is a registration of a key that is a member's: a
	// non-revoked device's encryption key or a user's recovery key. A key is a
	// member's or an executor's, never both (deviceauth refuses the reverse).
	ErrRecipientIsMemberKey = errors.New("personalstate/store: that key is an enrolled device's or a recovery key, not an executor's")
)

// ServiceRecipient is one registered executor key.
type ServiceRecipient struct {
	ID                    string
	PrincipalID           string
	Recipient             string
	Label                 string
	Fingerprint           string
	RegisteredByPrincipal string
	RegisteredByDevice    string
	CreatedAt             time.Time
}

// GrantWrap is a copy of a space's current key, wrapped for a service recipient,
// that a grant records together with the grant itself.
type GrantWrap struct {
	Recipient string
	Wrapped   []byte
	Epoch     int
}

// RegisterServiceRecipient records recipient as a wrap target for the executor
// principalID, registered by the user principal by through device. Registering
// a key already live for the same principal returns that registration with
// created false and changes nothing, so a retried command is harmless. A key
// live for another principal is ErrRecipientTaken. It emits
// TypeServiceRecipientRegistered in the same transaction.
//
// The caller has already checked that recipient is well formed and is not an
// enrolled device or recovery key; the store sees only opaque strings.
func (s *Store) RegisterServiceRecipient(ctx context.Context, principalID, recipient, label, fingerprint, by, device string) (ServiceRecipient, bool, error) {
	if recipient == "" {
		return ServiceRecipient{}, false, ErrEmptyRecipient
	}
	now := s.clock.Now().UTC()
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return ServiceRecipient{}, false, fmt.Errorf("personalstate/store: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	held, err := scanServiceRecipient(tx.QueryRowContext(ctx, serviceRecipientSelect+
		` WHERE recipient = ? AND revoked_at IS NULL`, recipient))
	switch {
	case err == nil:
		if held.PrincipalID != principalID {
			return ServiceRecipient{}, false, ErrRecipientTaken
		}
		return held, false, tx.Commit()
	case !errors.Is(err, ErrUnknownRecipient):
		return ServiceRecipient{}, false, err
	}

	if err := refuseMemberKeyTx(ctx, tx, recipient); err != nil {
		return ServiceRecipient{}, false, err
	}
	sr := ServiceRecipient{
		ID: uuid.Must(uuid.NewV7()).String(), PrincipalID: principalID, Recipient: recipient,
		Label: label, Fingerprint: fingerprint, RegisteredByPrincipal: by, RegisteredByDevice: device,
		CreatedAt: now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO service_recipients (id, principal_id, recipient, label, fingerprint,
			registered_by_principal, registered_by_device, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sr.ID, sr.PrincipalID, sr.Recipient, sr.Label, sr.Fingerprint, by, device, now.Format(timeFormat)); err != nil {
		return ServiceRecipient{}, false, fmt.Errorf("personalstate/store: registering a service recipient: %w", err)
	}
	ev, err := s.events.EmitTx(ctx, tx, events.TypeServiceRecipientRegistered, "service_recipient", sr.ID,
		map[string]any{
			"principal_id": principalID, "recipient": recipient, "fingerprint": fingerprint,
			"registered_by_device": device,
		})
	if err != nil {
		return ServiceRecipient{}, false, fmt.Errorf("personalstate/store: registering a service recipient: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ServiceRecipient{}, false, fmt.Errorf("personalstate/store: committing: %w", err)
	}
	s.events.Publish(ev)
	return sr, true, nil
}

// RemoveServiceRecipient withdraws a registration (by id or by recipient key)
// and, in the same transaction, deletes every wrapped copy of a space key held
// for that key on this node. Every such copy is the executor's: a wrap names
// its recipient only by key, and a live service-recipient key is never also a
// non-revoked device's or a recovery key (refused in both directions, inside
// each side's transaction), so no member's wrap can share the key. It returns the registration and the spaces whose
// copy went. Removing a key therefore removes what it could decrypt here from
// now on. Copies already replicated to a peer are removed there only by a
// rotation, as for any revoked device (ADR-0103).
func (s *Store) RemoveServiceRecipient(ctx context.Context, ref, device string) (ServiceRecipient, []string, error) {
	now := s.clock.Now().UTC()
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return ServiceRecipient{}, nil, fmt.Errorf("personalstate/store: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	sr, err := scanServiceRecipient(tx.QueryRowContext(ctx, serviceRecipientSelect+
		` WHERE (id = ? OR recipient = ?) AND revoked_at IS NULL`, ref, ref))
	if err != nil {
		return ServiceRecipient{}, nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE service_recipients SET revoked_at = ? WHERE id = ?`,
		now.Format(timeFormat), sr.ID); err != nil {
		return ServiceRecipient{}, nil, fmt.Errorf("personalstate/store: removing a service recipient: %w", err)
	}
	gone, evs, err := s.deleteWrapsForTx(ctx, tx, "", []string{sr.Recipient})
	if err != nil {
		return ServiceRecipient{}, nil, err
	}
	spaceIDs := make([]string, 0, len(gone))
	for _, g := range gone {
		spaceIDs = append(spaceIDs, g.spaceID)
	}
	ev, err := s.events.EmitTx(ctx, tx, events.TypeServiceRecipientRemoved, "service_recipient", sr.ID,
		map[string]any{
			"principal_id": sr.PrincipalID, "recipient": sr.Recipient,
			"removed_by_device": device, "wraps_deleted": len(spaceIDs),
		})
	if err != nil {
		return ServiceRecipient{}, nil, fmt.Errorf("personalstate/store: removing a service recipient: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ServiceRecipient{}, nil, fmt.Errorf("personalstate/store: committing: %w", err)
	}
	s.events.Publish(append(evs, ev)...)
	return sr, spaceIDs, nil
}

// ServiceRecipients lists the live registrations, oldest first.
func (s *Store) ServiceRecipients(ctx context.Context) ([]ServiceRecipient, error) {
	rows, err := s.reader.QueryContext(ctx, serviceRecipientSelect+
		` WHERE revoked_at IS NULL ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("personalstate/store: listing service recipients: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []ServiceRecipient{}
	for rows.Next() {
		sr, err := scanServiceRecipient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sr)
	}
	return out, rows.Err()
}

// RecipientsOf is the set of live service-recipient keys of principalID: the
// wraps a restricted caller is shown of a space's key (ADR-0104). It satisfies
// the personal-state API's PrincipalRecipients.
func (s *Store) RecipientsOf(ctx context.Context, principalID string) (map[string]bool, error) {
	return recipientSet(ctx, s.reader,
		`SELECT recipient FROM service_recipients WHERE principal_id = ? AND revoked_at IS NULL`, principalID)
}

// GrantedServiceRecipients is the set of live service-recipient keys whose
// principal holds an active grant on spaceID: the service recipients a space
// key may be wrapped for there, on a re-wrap or a rotation. A key whose
// executor lost its grant (revoked or expired) drops out, so a rotation must
// revoke it explicitly rather than hand it the next key.
func (s *Store) GrantedServiceRecipients(ctx context.Context, spaceID string) (map[string]bool, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT r.recipient, g.expires_at
		FROM service_recipients r
		JOIN space_grants g ON g.principal_id = r.principal_id
		WHERE g.space_id = ? AND g.revoked_at IS NULL AND r.revoked_at IS NULL`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("personalstate/store: listing a space's service recipients: %w", err)
	}
	defer func() { _ = rows.Close() }()
	now := s.clock.Now()
	out := map[string]bool{}
	for rows.Next() {
		var (
			r       string
			expires sql.NullString
		)
		if err := rows.Scan(&r, &expires); err != nil {
			return nil, fmt.Errorf("personalstate/store: listing a space's service recipients: %w", err)
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
		out[r] = true
	}
	return out, rows.Err()
}

// refuseMemberKeyTx is ErrRecipientIsMemberKey when key is a non-revoked
// device's encryption key or any user's recovery key.
func refuseMemberKeyTx(ctx context.Context, tx *sql.Tx, key string) error {
	var n int
	if err := tx.QueryRowContext(ctx, `
		SELECT (SELECT count(*) FROM device_identities WHERE encryption_key = ? AND revoked_at IS NULL)
		     + (SELECT count(*) FROM user_identities WHERE recovery_encryption_key = ?)`, key, key).Scan(&n); err != nil {
		return fmt.Errorf("personalstate/store: checking member keys: %w", err)
	}
	if n > 0 {
		return ErrRecipientIsMemberKey
	}
	return nil
}

// serviceWrapAllowedTx is the in-transaction half of enrol-before-wrap for
// service recipients: when recipient is a live service recipient, its executor
// must hold an active, unexpired grant on spaceID at now, or the write is
// ErrServiceRecipientUngranted. Any other recipient passes here (a device or a
// recovery key is the API's authorizer's to check).
func serviceWrapAllowedTx(ctx context.Context, tx *sql.Tx, spaceID, recipient string, now time.Time) error {
	var principal string
	err := tx.QueryRowContext(ctx,
		`SELECT principal_id FROM service_recipients WHERE recipient = ? AND revoked_at IS NULL`, recipient).Scan(&principal)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("personalstate/store: reading a service recipient: %w", err)
	}
	var expires sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT expires_at FROM space_grants WHERE space_id = ? AND principal_id = ? AND revoked_at IS NULL`,
		spaceID, principal).Scan(&expires)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s", ErrServiceRecipientUngranted, recipient)
	case err != nil:
		return fmt.Errorf("personalstate/store: reading a grant: %w", err)
	}
	if expires.Valid {
		exp, err := time.Parse(timeFormat, expires.String)
		if err != nil {
			return fmt.Errorf("personalstate/store: a grant has an unparseable expires_at: %w", err)
		}
		if !now.Before(exp) {
			return fmt.Errorf("%w: %s", ErrServiceRecipientUngranted, recipient)
		}
	}
	return nil
}

// putGrantWrapsTx stores, inside a grant's transaction, each wrap the grant
// carries: every recipient must be a live service recipient of principalID, and
// every copy must seal the space's current key epoch (ADR-0103).
func (s *Store) putGrantWrapsTx(ctx context.Context, tx *sql.Tx, spaceID, principalID string, wraps []GrantWrap, now time.Time) ([]events.Event, error) {
	if len(wraps) == 0 {
		return nil, nil
	}
	own, err := recipientSet(ctx, tx,
		`SELECT recipient FROM service_recipients WHERE principal_id = ? AND revoked_at IS NULL`, principalID)
	if err != nil {
		return nil, err
	}
	current, err := keyEpochTx(ctx, tx, spaceID)
	if err != nil {
		return nil, err
	}
	evs := make([]events.Event, 0, len(wraps))
	for _, w := range wraps {
		if w.Recipient == "" {
			return nil, ErrEmptyRecipient
		}
		if len(w.Wrapped) == 0 {
			return nil, ErrEmptyWrapped
		}
		if !own[w.Recipient] {
			return nil, fmt.Errorf("%w: %s", ErrNotServiceRecipient, w.Recipient)
		}
		if err := checkWrapEpoch(spaceID, current, w.Epoch); err != nil {
			return nil, err
		}
		// The grant was written earlier in this transaction; a wrap lands only
		// if it is active now.
		if err := serviceWrapAllowedTx(ctx, tx, spaceID, w.Recipient, now); err != nil {
			return nil, err
		}
		ev, _, err := s.upsertWrapTx(ctx, tx, spaceID, w.Recipient, w.Wrapped, w.Epoch, now)
		if err != nil {
			return nil, err
		}
		evs = append(evs, ev)
	}
	return evs, nil
}

// A deletedWrap is one wrapped copy deleteWrapsForTx removed.
type deletedWrap struct{ spaceID, recipient string }

// deleteWrapsForTx deletes the wrapped copies held for recipients — on one space,
// or on every space when spaceID is empty — and returns what it deleted and one
// TypeSpaceKeyRevoked event per deleted copy.
func (s *Store) deleteWrapsForTx(ctx context.Context, tx *sql.Tx, spaceID string, recipients []string) ([]deletedWrap, []events.Event, error) {
	var (
		gone []deletedWrap
		evs  []events.Event
	)
	for _, r := range recipients {
		hit, err := deleteRecipientWrapsTx(ctx, tx, spaceID, r)
		if err != nil {
			return nil, nil, err
		}
		for _, id := range hit {
			ev, err := s.events.EmitTx(ctx, tx, events.TypeSpaceKeyRevoked, "encrypted_space", id,
				map[string]any{"recipient": r})
			if err != nil {
				return nil, nil, fmt.Errorf("personalstate/store: recording revocation: %w", err)
			}
			evs = append(evs, ev)
			gone = append(gone, deletedWrap{spaceID: id, recipient: r})
		}
	}
	return gone, evs, nil
}

// deleteRecipientWrapsTx deletes recipient's copies — on spaceID, or on every
// space when it is empty — and returns the spaces they were on.
func deleteRecipientWrapsTx(ctx context.Context, tx *sql.Tx, spaceID, recipient string) ([]string, error) {
	q, args := `DELETE FROM wrapped_keys WHERE recipient = ? RETURNING space_id`, []any{recipient}
	if spaceID != "" {
		q, args = `DELETE FROM wrapped_keys WHERE recipient = ? AND space_id = ? RETURNING space_id`, []any{recipient, spaceID}
	}
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("personalstate/store: deleting a recipient's wraps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var hit []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("personalstate/store: deleting a recipient's wraps: %w", err)
		}
		hit = append(hit, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("personalstate/store: deleting a recipient's wraps: %w", err)
	}
	return hit, rows.Close()
}

const serviceRecipientSelect = `SELECT id, principal_id, recipient, label, fingerprint,
	registered_by_principal, registered_by_device, created_at FROM service_recipients`

func scanServiceRecipient(row rowScanner) (ServiceRecipient, error) {
	var (
		sr      ServiceRecipient
		created string
	)
	err := row.Scan(&sr.ID, &sr.PrincipalID, &sr.Recipient, &sr.Label, &sr.Fingerprint,
		&sr.RegisteredByPrincipal, &sr.RegisteredByDevice, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return ServiceRecipient{}, ErrUnknownRecipient
	}
	if err != nil {
		return ServiceRecipient{}, fmt.Errorf("personalstate/store: reading a service recipient: %w", err)
	}
	if sr.CreatedAt, err = time.Parse(timeFormat, created); err != nil {
		return ServiceRecipient{}, fmt.Errorf("personalstate/store: service recipient %s has an unparseable created_at: %w", sr.ID, err)
	}
	return sr, nil
}

// recipientSet runs a one-column query of recipient keys into a set.
func recipientSet(ctx context.Context, q queryer, query string, args ...any) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("personalstate/store: listing service recipients: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, fmt.Errorf("personalstate/store: listing service recipients: %w", err)
		}
		out[r] = true
	}
	return out, rows.Err()
}
