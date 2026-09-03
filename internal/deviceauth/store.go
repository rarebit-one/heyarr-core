// Package deviceauth is the control-plane side of Milestone 8 device identity
// (§40, ADR-0048, ADR-0032). It is what internal/auth deliberately is not: an
// identity system. Where internal/auth issues opaque bearer tokens, this pins
// user identities and the device keys they vouch for, so a device can prove it
// is a user's — on either peer, with no token issued.
//
// A user identity is a principal (kind 'user') with a pinned Ed25519 public key,
// the ADR-0012 trust-root shape applied to a user rather than a peer. A device
// is enrolled UNDER a user by that user's signed cert (internal/enrolment).
// Authentication (verifier.go) then needs both: a cert that verifies against the
// pinned user key, AND a device row that is enrolled and not revoked — so a
// leaked cert for a device the server never accepted authenticates nobody, and a
// single device can be revoked without unpinning its user.
package deviceauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/enrolment"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

const timeFormat = time.RFC3339Nano

// Clock is injected so expiry and enrolment times are unit facts, not sleeps
// (ADR-0017).
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// Errors the store refuses with. Distinct because they call for different
// actions and the verifier maps each to a named refusal reason.
var (
	// ErrUnknownUser means no user identity is pinned for a public key.
	ErrUnknownUser = errors.New("deviceauth: user is not enrolled")
	// ErrUnknownDevice means no device is enrolled for a device key.
	ErrUnknownDevice = errors.New("deviceauth: device is not enrolled")
	// ErrDeviceRevoked means the device exists but has been revoked.
	ErrDeviceRevoked = errors.New("deviceauth: device is revoked")
	// ErrUserExists means a user identity is already pinned for this key.
	ErrUserExists = errors.New("deviceauth: user identity already enrolled")
	// ErrDeviceExists means this device key is already enrolled.
	ErrDeviceExists = errors.New("deviceauth: device already enrolled")
	// ErrMalformedKey is a public key that is not a rendered Ed25519 key.
	ErrMalformedKey = errors.New("deviceauth: malformed public key")
	// ErrCertMismatch is an enrolment cert whose user is not the one being
	// enrolled under, or whose device key is empty.
	ErrCertMismatch = errors.New("deviceauth: cert does not match the enrolment")
)

// User is a pinned user identity.
type User struct {
	ID          string
	PrincipalID string
	PublicKey   string // rendered "ed25519:<hex>"
	Name        string
	EnrolledAt  time.Time
}

// Device is a device key a user has vouched for.
type Device struct {
	ID        string
	UserID    string
	DeviceKey string // rendered "ed25519:<hex>"
	// EncryptionKey is the device's X25519 encryption key ("x25519:<hex>"), the
	// key space keys are wrapped for (§41, ADR-0049). It is pinned here so a
	// wrapper on either peer can learn an enrolled device's encryption key from
	// the cert the user signed, rather than trusting an unauthenticated claim.
	// Empty for a device enrolled by a v1 cert (no encryption key bound).
	EncryptionKey string
	Name          string
	Cert          string // the admitting membership op token (a user-signed cert, or a member-signed add — ADR-0068)
	EnrolledAt    time.Time
	ExpiresAt     time.Time
	RevokedAt     *time.Time
}

// Admission parses the device's admitting op (Cert) — who admitted it, citing
// what, and the op's hash. It re-verifies the op's own signature, nothing
// about authority: the view is the evaluation's answer, this is its provenance.
func (d Device) Admission() (enrolment.Op, error) {
	return enrolment.VerifyOp(d.Cert)
}

// Active reports whether the device may authenticate at t.
func (d Device) Active(t time.Time) error {
	if d.RevokedAt != nil {
		return ErrDeviceRevoked
	}
	return nil
}

// Store is the user_identities + device_identities tables.
type Store struct {
	writer *sql.DB
	reader *sql.DB
	clock  Clock
	events *events.Log
}

// Options configure a Store.
type Options struct {
	// Writer is the single-writer pool (ADR-0003).
	Writer *sql.DB
	// Reader serves the per-request auth lookup, off the write path.
	Reader *sql.DB
	// Events records every enrolment and revocation (invariant 7). Required.
	Events *events.Log
	Clock  Clock
}

// New constructs a Store.
func New(opts Options) (*Store, error) {
	if opts.Writer == nil {
		return nil, errors.New("deviceauth: a writer database is required")
	}
	if opts.Events == nil {
		return nil, errors.New("deviceauth: an event log is required (invariant 7)")
	}
	reader := opts.Reader
	if reader == nil {
		reader = opts.Writer
	}
	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}
	return &Store{writer: opts.Writer, reader: reader, clock: clock, events: opts.Events}, nil
}

// Now reports the store's current time.
func (s *Store) Now() time.Time { return s.clock.Now() }

// EnrolUser pins a user public key, creating its principal (kind 'user') in the
// same transaction. This is the ADR-0032 gate: nothing a user signs is honoured
// until the user is pinned here, out of band.
func (s *Store) EnrolUser(ctx context.Context, publicKey, name string) (User, error) {
	pub, err := identity.ParsePublicKey(publicKey)
	if err != nil {
		return User{}, fmt.Errorf("%w: %s", ErrMalformedKey, err.Error())
	}
	rendered := identity.FormatPublicKey(pub)
	if name == "" {
		name = "user-" + rendered[len(rendered)-8:]
	}
	now := s.clock.Now().UTC()

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("deviceauth: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existing string
	err = tx.QueryRowContext(ctx, `SELECT id FROM user_identities WHERE public_key = ?`, rendered).Scan(&existing)
	switch {
	case err == nil:
		return User{}, fmt.Errorf("%w: %s", ErrUserExists, rendered)
	case !errors.Is(err, sql.ErrNoRows):
		return User{}, fmt.Errorf("deviceauth: checking for an existing user: %w", err)
	}

	principalID := uuid.Must(uuid.NewV7()).String()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO principals (id, kind, name, created_at) VALUES (?, 'user', ?, ?)`,
		principalID, name, now.Format(timeFormat)); err != nil {
		return User{}, fmt.Errorf("deviceauth: creating principal: %w", err)
	}
	id := uuid.Must(uuid.NewV7()).String()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_identities (id, principal_id, public_key, enrolled_at) VALUES (?, ?, ?, ?)`,
		id, principalID, rendered, now.Format(timeFormat)); err != nil {
		return User{}, fmt.Errorf("deviceauth: pinning user: %w", err)
	}
	ev, err := s.events.EmitTx(ctx, tx, events.TypeUserEnrolled, "user_identity", id,
		map[string]any{"public_key": rendered, "name": name})
	if err != nil {
		return User{}, fmt.Errorf("deviceauth: recording enrolment: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("deviceauth: committing: %w", err)
	}
	s.events.Publish(ev)
	return User{ID: id, PrincipalID: principalID, PublicKey: rendered, Name: name, EnrolledAt: now}, nil
}

// EnrolDevice records a device under the user its admitting op names — the
// admin path. The op (a user-signed cert, or since ADR-0068 an add signed by
// any current member) is evaluated against the pinned identity's op log
// exactly as authentication evaluates it, so a device cannot be enrolled under
// a user nobody in that identity signed for, and an unpinned user is refused
// ErrUnknownUser (enrol the user before its devices). A device the view
// already holds is ErrDeviceExists.
func (s *Store) EnrolDevice(ctx context.Context, opToken, name string) (Device, error) {
	now := s.clock.Now().UTC()

	op, err := enrolment.VerifyOp(opToken)
	if err != nil {
		return Device{}, fmt.Errorf("%w: %s", ErrCertMismatch, err.Error())
	}
	if _, err := s.LookupDevice(ctx, op.Device); err == nil {
		return Device{}, fmt.Errorf("%w: %s", ErrDeviceExists, op.Device)
	} else if !errors.Is(err, ErrUnknownDevice) {
		return Device{}, err
	}
	// The evaluation records the op and materialises the row (RecordOps).
	user, auth, err := s.verifyMembership(ctx, opToken, nil, now)
	switch {
	case errors.Is(err, ErrUnknownUser):
		return Device{}, err
	case err != nil:
		return Device{}, fmt.Errorf("%w: %s", ErrCertMismatch, err.Error())
	}
	device, err := s.memberDevice(ctx, user, auth.DeviceKey)
	if err != nil {
		return Device{}, err
	}
	if err := s.setDeviceName(ctx, device.ID, name); err != nil {
		return Device{}, err
	}
	device.Name = name
	return device, nil
}

// setDeviceName names a device row. A name is display state, not identity: it
// carries no event of its own (the materialisation already emitted enrolment).
func (s *Store) setDeviceName(ctx context.Context, id, name string) error {
	if _, err := s.writer.ExecContext(ctx, `UPDATE device_identities SET name = ? WHERE id = ?`, name, id); err != nil {
		return fmt.Errorf("deviceauth: naming device: %w", err)
	}
	return nil
}

// LookupUser returns the pinned user for a rendered public key. It joins the
// principal so an authenticated identity carries a name for the log, in one
// round trip on the read pool — this is the per-request hot path.
func (s *Store) LookupUser(ctx context.Context, publicKey string) (User, error) {
	row := s.reader.QueryRowContext(ctx,
		`SELECT u.id, u.principal_id, u.public_key, u.enrolled_at, p.name
		 FROM user_identities u JOIN principals p ON p.id = u.principal_id
		 WHERE u.public_key = ?`, publicKey)
	var u User
	var enrolled string
	err := row.Scan(&u.ID, &u.PrincipalID, &u.PublicKey, &enrolled, &u.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("%w: %s", ErrUnknownUser, publicKey)
	}
	if err != nil {
		return User{}, fmt.Errorf("deviceauth: reading user: %w", err)
	}
	if u.EnrolledAt, err = time.Parse(timeFormat, enrolled); err != nil {
		return User{}, fmt.Errorf("deviceauth: user %s has an unparseable enrolled_at: %w", u.ID, err)
	}
	return u, nil
}

// ListUsers returns every pinned user identity, most-recently-enrolled first.
// It is what the admin surface lists so an operator can see which users a peer
// trusts and copy a key out to revoke — the read counterpart of EnrolUser.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT u.id, u.principal_id, u.public_key, u.enrolled_at, p.name
		 FROM user_identities u JOIN principals p ON p.id = u.principal_id
		 ORDER BY u.enrolled_at DESC, u.id DESC`)
	if err != nil {
		return nil, fmt.Errorf("deviceauth: listing users: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []User{}
	for rows.Next() {
		var u User
		var enrolled string
		if err := rows.Scan(&u.ID, &u.PrincipalID, &u.PublicKey, &enrolled, &u.Name); err != nil {
			return nil, fmt.Errorf("deviceauth: reading user: %w", err)
		}
		if u.EnrolledAt, err = time.Parse(timeFormat, enrolled); err != nil {
			return nil, fmt.Errorf("deviceauth: user %s has an unparseable enrolled_at: %w", u.ID, err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// LookupDevice returns the enrolled device for a rendered device key.
func (s *Store) LookupDevice(ctx context.Context, deviceKey string) (Device, error) {
	row := s.reader.QueryRowContext(ctx,
		`SELECT id, user_id, device_key, encryption_key, name, cert, enrolled_at, expires_at, revoked_at
		 FROM device_identities WHERE device_key = ?`, deviceKey)
	return scanDevice(row)
}

// PrincipalID returns the principal a user authenticates as. It is a thin read
// used by the verifier to build the authenticated identity.
func (s *Store) PrincipalID(ctx context.Context, userID string) (string, error) {
	var pid string
	err := s.reader.QueryRowContext(ctx, `SELECT principal_id FROM user_identities WHERE id = ?`, userID).Scan(&pid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUnknownUser
	}
	if err != nil {
		return "", fmt.Errorf("deviceauth: reading principal: %w", err)
	}
	return pid, nil
}

// RevokeUser unpins a user identity, deleting it and cascading to its devices —
// revocation is removing the pin, the same shape as peer removal (ADR-0012).
func (s *Store) RevokeUser(ctx context.Context, publicKey string) (User, error) {
	user, err := s.LookupUser(ctx, publicKey)
	if err != nil {
		return User{}, err
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("deviceauth: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Deleting the principal cascades to user_identities and its device rows
	// (ON DELETE CASCADE), so a revoked user leaves nothing behind that could
	// still authenticate.
	if _, err := tx.ExecContext(ctx, `DELETE FROM principals WHERE id = ?`, user.PrincipalID); err != nil {
		return User{}, fmt.Errorf("deviceauth: revoking user: %w", err)
	}
	ev, err := s.events.EmitTx(ctx, tx, events.TypeUserRevoked, "user_identity", user.ID,
		map[string]any{"public_key": user.PublicKey})
	if err != nil {
		return User{}, fmt.Errorf("deviceauth: recording revocation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("deviceauth: committing: %w", err)
	}
	s.events.Publish(ev)
	return user, nil
}

// RevokeDevice tombstones a single device, leaving its row so a re-presented
// cert for it is refused rather than silently re-enrolled. Idempotent: revoking
// an already-revoked device returns it unchanged.
func (s *Store) RevokeDevice(ctx context.Context, deviceKey string) (Device, error) {
	device, err := s.LookupDevice(ctx, deviceKey)
	if err != nil {
		return Device{}, err
	}
	if device.RevokedAt != nil {
		return device, nil
	}
	now := s.clock.Now().UTC()
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return Device{}, fmt.Errorf("deviceauth: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`UPDATE device_identities SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		now.Format(timeFormat), device.ID); err != nil {
		return Device{}, fmt.Errorf("deviceauth: revoking device: %w", err)
	}
	ev, err := s.events.EmitTx(ctx, tx, events.TypeDeviceRevoked, "device_identity", device.ID,
		map[string]any{"device_key": device.DeviceKey, "user_id": device.UserID})
	if err != nil {
		return Device{}, fmt.Errorf("deviceauth: recording revocation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Device{}, fmt.Errorf("deviceauth: committing: %w", err)
	}
	s.events.Publish(ev)
	device.RevokedAt = &now
	return device, nil
}

// ListDevices returns a user's devices, most-recently-enrolled first.
func (s *Store) ListDevices(ctx context.Context, userID string) ([]Device, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT id, user_id, device_key, encryption_key, name, cert, enrolled_at, expires_at, revoked_at
		 FROM device_identities WHERE user_id = ? ORDER BY enrolled_at DESC, id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("deviceauth: listing devices: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Device{}
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// rowScanner is satisfied by both *sql.Row (LookupDevice) and *sql.Rows
// (ListDevices), so one scanner serves both.
type rowScanner interface{ Scan(dest ...any) error }

func scanDevice(row rowScanner) (Device, error) {
	var d Device
	var enrolled, expires string
	var revoked sql.NullString
	err := row.Scan(&d.ID, &d.UserID, &d.DeviceKey, &d.EncryptionKey, &d.Name, &d.Cert, &enrolled, &expires, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrUnknownDevice
	}
	if err != nil {
		return Device{}, fmt.Errorf("deviceauth: reading device: %w", err)
	}
	if d.EnrolledAt, err = time.Parse(timeFormat, enrolled); err != nil {
		return Device{}, fmt.Errorf("deviceauth: device %s has an unparseable enrolled_at: %w", d.ID, err)
	}
	if d.ExpiresAt, err = time.Parse(timeFormat, expires); err != nil {
		return Device{}, fmt.Errorf("deviceauth: device %s has an unparseable expires_at: %w", d.ID, err)
	}
	if revoked.Valid && revoked.String != "" {
		t, err := time.Parse(timeFormat, revoked.String)
		if err != nil {
			return Device{}, fmt.Errorf("deviceauth: device %s has an unparseable revoked_at: %w", d.ID, err)
		}
		d.RevokedAt = &t
	}
	return d, nil
}

// DeviceActive reports whether a device key is still a member here: enrolled,
// and not revoked. It answers nil when it is, and the store's own refusal —
// ErrUnknownDevice or ErrDeviceRevoked — when it is not.
//
// It exists for the web-login path (#420, ADR-0053): a session pins the device
// key that approved it, and until now nothing re-read that pin, so revoking a
// device left its already-minted sessions authenticating until they expired. It
// is the same question Verify asks in passing, exposed on its own so a caller
// that holds only a device key — which is all a session principal carries — can
// ask it without a credential to verify.
func (s *Store) DeviceActive(ctx context.Context, deviceKey string) error {
	device, err := s.LookupDevice(ctx, deviceKey)
	if err != nil {
		return err
	}
	return device.Active(s.clock.Now())
}
