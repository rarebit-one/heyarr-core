package deviceauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// Errors RekeyUser refuses with, beside ErrUnknownUser, ErrMalformedKey and
// ErrUserExists.
var (
	// ErrAmbiguousUser means a principal reference matched more than one user
	// identity, so the operator must name it more precisely.
	ErrAmbiguousUser = errors.New("deviceauth: principal matches more than one user identity")
	// ErrSameKey means the replacement key is the key already pinned: a rekey
	// to the same key would only discard the identity's op log.
	ErrSameKey = errors.New("deviceauth: the new public key is the key already pinned")
)

// Rekey is the outcome of RekeyUser: the identity as it now stands, the keys it
// replaced, and how many of the old key's membership ops were dropped.
type Rekey struct {
	User                     User
	OldPublicKey             string
	OldRecoveryEncryptionKey string
	OpsDeleted               int
}

// RekeyUser replaces a pinned user identity's signing key and recovery
// encryption key IN PLACE (void-which-binds ADR-0022, C2 step 6). It is the
// gen1 to gen2 cutover's one write to user_identities: the principal and its
// row keep their ids, so everything keyed by principal survives, and RevokeUser's
// principal cascade is never involved.
//
// principal is a principal id, a user identity id, or a principal name, and
// must resolve to exactly one user identity. publicKey must be a well-formed
// Ed25519 key not pinned to any other identity and different from the current
// one; recoveryKey must be a well-formed X25519 public key (required here: the
// cutover re-registers the recovery recipient along with the signing key).
//
// The membership ops recorded for the identity are deleted in the same
// transaction. Every one of them was signed under the old key (RecordOps
// refuses an op naming a different identity), and the op log is read by the
// pinned key, so left in place they would be evaluated as the new key's log.
// Device rows are untouched: gen1 devices are revoked individually by the
// operator, and gen2 devices enrol fresh under the new key.
func (s *Store) RekeyUser(ctx context.Context, principal, publicKey, recoveryKey string) (Rekey, error) {
	pub, err := identity.ParsePublicKey(publicKey)
	if err != nil {
		return Rekey{}, fmt.Errorf("%w: %s", ErrMalformedKey, err.Error())
	}
	rendered := identity.FormatPublicKey(pub)
	recovery, err := parseRecoveryEncryptionKey(recoveryKey)
	if err != nil {
		return Rekey{}, err
	}
	if recovery == "" {
		return Rekey{}, fmt.Errorf("%w: a recovery encryption key is required", ErrMalformedKey)
	}

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return Rekey{}, fmt.Errorf("deviceauth: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	user, err := resolveUserTx(ctx, tx, principal)
	if err != nil {
		return Rekey{}, err
	}
	if user.PublicKey == rendered {
		return Rekey{}, fmt.Errorf("%w: %s", ErrSameKey, rendered)
	}
	var other string
	err = tx.QueryRowContext(ctx, `SELECT id FROM user_identities WHERE public_key = ?`, rendered).Scan(&other)
	switch {
	case err == nil:
		return Rekey{}, fmt.Errorf("%w: %s is pinned to user identity %s", ErrUserExists, rendered, other)
	case !errors.Is(err, sql.ErrNoRows):
		return Rekey{}, fmt.Errorf("deviceauth: checking for an existing user: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE user_identities SET public_key = ?, recovery_encryption_key = ? WHERE id = ?`,
		rendered, recovery, user.ID); err != nil {
		return Rekey{}, fmt.Errorf("deviceauth: rekeying user: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM membership_ops WHERE user_id = ?`, user.ID)
	if err != nil {
		return Rekey{}, fmt.Errorf("deviceauth: dropping the old key's membership ops: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return Rekey{}, fmt.Errorf("deviceauth: counting dropped membership ops: %w", err)
	}

	ev, err := s.events.EmitTx(ctx, tx, events.TypeUserRekeyed, "user_identity", user.ID,
		map[string]any{
			"principal_id":                user.PrincipalID,
			"old_public_key":              user.PublicKey,
			"public_key":                  rendered,
			"old_recovery_encryption_key": user.RecoveryEncryptionKey,
			"recovery_encryption_key":     recovery,
			"ops_deleted":                 deleted,
		})
	if err != nil {
		return Rekey{}, fmt.Errorf("deviceauth: recording rekey: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Rekey{}, fmt.Errorf("deviceauth: committing: %w", err)
	}
	s.events.Publish(ev)

	out := Rekey{
		User:                     user,
		OldPublicKey:             user.PublicKey,
		OldRecoveryEncryptionKey: user.RecoveryEncryptionKey,
		OpsDeleted:               int(deleted),
	}
	out.User.PublicKey = rendered
	out.User.RecoveryEncryptionKey = recovery
	return out, nil
}

// resolveUserTx finds the one user identity a principal reference names: a
// principal id, a user identity id, or a principal name.
func resolveUserTx(ctx context.Context, tx *sql.Tx, principal string) (User, error) {
	if principal == "" {
		return User{}, fmt.Errorf("%w: no principal named", ErrUnknownUser)
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT u.id, u.principal_id, u.public_key, u.recovery_encryption_key, u.enrolled_at, p.name
		 FROM user_identities u JOIN principals p ON p.id = u.principal_id
		 WHERE p.id = ? OR u.id = ? OR p.name = ?`, principal, principal, principal)
	if err != nil {
		return User{}, fmt.Errorf("deviceauth: resolving principal: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var found []User
	for rows.Next() {
		var u User
		var enrolled string
		if err := rows.Scan(&u.ID, &u.PrincipalID, &u.PublicKey, &u.RecoveryEncryptionKey, &enrolled, &u.Name); err != nil {
			return User{}, fmt.Errorf("deviceauth: reading user: %w", err)
		}
		if u.EnrolledAt, err = time.Parse(timeFormat, enrolled); err != nil {
			return User{}, fmt.Errorf("deviceauth: user %s has an unparseable enrolled_at: %w", u.ID, err)
		}
		found = append(found, u)
	}
	if err := rows.Err(); err != nil {
		return User{}, fmt.Errorf("deviceauth: resolving principal: %w", err)
	}
	switch len(found) {
	case 0:
		return User{}, fmt.Errorf("%w: no user identity for principal %q", ErrUnknownUser, principal)
	case 1:
		return found[0], nil
	default:
		return User{}, fmt.Errorf("%w: %q matches %d", ErrAmbiguousUser, principal, len(found))
	}
}
