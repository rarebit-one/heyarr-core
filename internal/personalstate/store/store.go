// Package store is the peer-side storage of encrypted personal state (§38, §41,
// §79, ADR-0049): the encrypted spaces a peer holds and the wrapped copies of
// their keys. It is the storage half of Invariant 6 — the peer keeps ciphertext
// and opaque metadata and CANNOT read any of it.
//
// The load-bearing thing this package does NOT do is unwrap. It stores a wrapped
// key as opaque bytes and hands them back as opaque bytes; it imports neither the
// unwrap path nor any X25519 private key, so "the peer reads a space key" is not
// merely refused at runtime — it is unspellable here, because the capability is
// absent from the package. A space key becomes plaintext only on an authorised
// device that holds the matching device key (a separate, client-side concern).
//
// It follows internal/deviceauth: one single-writer database (ADR-0003), reads on
// a reader pool, an injected clock (ADR-0017), and a state transition is an event
// (Invariant 7) — but every event here is opaque metadata (a space exists, a key
// was wrapped for a recipient), never a name or a key (§38).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
)

const timeFormat = time.RFC3339Nano

// Clock is the injected time source (ADR-0017).
type Clock interface{ Now() time.Time }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// Errors this store refuses with.
var (
	// ErrUnknownSpace is an operation naming a space this peer does not hold.
	ErrUnknownSpace = errors.New("personalstate/store: no such space")
	// ErrEmptyWrapped is a wrapped key with no bytes — a wrap that sealed nothing
	// is not something to store, because a recipient could never open it.
	ErrEmptyWrapped = errors.New("personalstate/store: the wrapped key is empty")
	// ErrEmptyRecipient is a wrapped key stored for no recipient.
	ErrEmptyRecipient = errors.New("personalstate/store: a recipient is required")
)

// Key-epoch errors (ADR-0103).
var (
	// ErrStaleKeyEpoch is a wrapped copy sealing an epoch older than the space's
	// current one: the key was rotated since. Storing it would hand a recipient a
	// superseded key, or let replication resurrect a copy a rotation dropped.
	ErrStaleKeyEpoch = errors.New("personalstate/store: the wrapped key seals a superseded key epoch")
	// ErrFutureKeyEpoch is a wrapped copy sealing an epoch newer than the space's
	// current one: the history row that moves the space there has not landed.
	ErrFutureKeyEpoch = errors.New("personalstate/store: the wrapped key seals a key epoch this peer has not reached")
	// ErrKeyEpochConflict is a rotation whose expected epoch is not the space's
	// current one — another rotation landed first. Re-open the space and retry.
	ErrKeyEpochConflict = errors.New("personalstate/store: the space key was rotated concurrently")
	// ErrKeyHistoryConflict is a history row for an epoch already held with
	// DIFFERENT bytes: two rotations forked the chain. Never overwritten.
	ErrKeyHistoryConflict = errors.New("personalstate/store: a different key-history row is already held for that epoch")
	// ErrInvalidKeyEpoch is a history row for epoch < 1 (epoch 0 has no
	// predecessor) or a rotation with a negative expected epoch.
	ErrInvalidKeyEpoch = errors.New("personalstate/store: invalid key epoch")
	// ErrEmptySealedPrev is a history row with no bytes.
	ErrEmptySealedPrev = errors.New("personalstate/store: the sealed previous key is empty")
	// ErrRotationDropsPreserved is a rotation that leaves out a recipient it must
	// keep — a recovery key holding a copy of the current key (ADR-0022).
	ErrRotationDropsPreserved = errors.New("personalstate/store: a rotation must re-wrap the recovery key")
	// ErrRotationRecipientsChanged is a rotation whose recipient selection no
	// longer matches the space (#703): a recipient holding a copy of the current
	// key is neither re-wrapped nor revoked (it was added since the device read
	// the set), or a wrap or a revocation names a recipient that holds no current
	// copy (it was removed since, or never had one). The device re-reads the
	// recipients and rotates again.
	ErrRotationRecipientsChanged = errors.New("personalstate/store: the space's recipients changed since the rotation read them")
	// ErrRotationRevokesRewrapped is a rotation that both re-wraps and revokes the
	// same recipient — a malformed request, not a race.
	ErrRotationRevokesRewrapped = errors.New("personalstate/store: a rotation cannot both re-wrap and revoke a recipient")
	// ErrNoRotationWraps is a rotation that re-wraps the new key for nobody — the
	// space would become unreadable to everyone.
	ErrNoRotationWraps = errors.New("personalstate/store: a rotation needs at least one wrapped key")
)

// A WrappedKey is one recipient's sealed copy of a space key, as the peer holds
// it. Wrapped is opaque: it is encryption.Seal output (e_pub ‖ nonce ‖
// ciphertext), and this package never looks inside it.
type WrappedKey struct {
	ID        string
	SpaceID   string
	Recipient string // "x25519:<hex>" — a device or the recovery encryption key
	Wrapped   []byte
	// Epoch is the key epoch this copy seals (ADR-0103): 0 for the space's
	// original key, N after the Nth rotation. Only copies at the space's current
	// epoch are ever held — a rotation drops the rest.
	Epoch     int
	CreatedAt time.Time
}

// Options configure a Store.
type Options struct {
	// Writer is the single-writer pool (ADR-0003). Required.
	Writer *sql.DB
	// Reader is the read pool; defaults to Writer when nil.
	Reader *sql.DB
	Events *events.Log
	Clock  Clock
}

// Store is the encrypted_spaces + wrapped_keys tables.
type Store struct {
	writer *sql.DB
	reader *sql.DB
	clock  Clock
	events *events.Log
}

// New opens a store over an already-migrated database.
func New(opts Options) (*Store, error) {
	if opts.Writer == nil {
		return nil, errors.New("personalstate/store: a writer database is required")
	}
	if opts.Events == nil {
		return nil, errors.New("personalstate/store: an event log is required")
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

// CreateSpace mints an encrypted space of the given kind and records it. The id
// is an opaque UUIDv7 minted by spaces.NewSpace — never derived from the kind or
// a name (§38) — and an unknown kind is refused before anything is written.
func (s *Store) CreateSpace(ctx context.Context, kind spaces.Kind) (spaces.EncryptedSpace, error) {
	now := s.clock.Now().UTC()
	sp, err := spaces.NewSpace(kind, now)
	if err != nil {
		return spaces.EncryptedSpace{}, err
	}

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return spaces.EncryptedSpace{}, fmt.Errorf("personalstate/store: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO encrypted_spaces (id, kind, created_at) VALUES (?, ?, ?)`,
		sp.ID, string(sp.Kind), sp.CreatedAt.UTC().Format(timeFormat)); err != nil {
		return spaces.EncryptedSpace{}, fmt.Errorf("personalstate/store: creating space: %w", err)
	}
	ev, err := s.events.EmitTx(ctx, tx, events.TypeSpaceCreated, "encrypted_space", sp.ID,
		map[string]any{"kind": string(sp.Kind)})
	if err != nil {
		return spaces.EncryptedSpace{}, fmt.Errorf("personalstate/store: recording space: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return spaces.EncryptedSpace{}, fmt.Errorf("personalstate/store: committing: %w", err)
	}
	s.events.Publish(ev)
	return sp, nil
}

// PutSpace records a space MINTED BY A CLIENT (client.Manager.Create mints the
// id and the space key together, client-side, and hands the peer only the opaque
// space and the wrapped copies). It is the push counterpart of CreateSpace: where
// CreateSpace mints the id server-side, this persists an id the device already
// minted, because the space key held in the device's memory is keyed by THAT id
// and a server-minted one would not match. The id is opaque to the peer (a
// UUIDv7, ADR-0017) — the peer validates only that the kind is a known §39
// category (structural, a peer may see it) and never derives meaning from the id.
//
// It is idempotent (Invariant 9): re-pushing a space the peer already holds is a
// no-op that emits no second event, so a device that retries a create after a
// dropped response does not double-record it. Re-pushing an id under a DIFFERENT
// kind is refused — a kind is a wire value, and silently adopting a new one would
// let a relay re-categorise a space out from under its owner. created_at is the
// peer's own clock, not the client's: the peer timestamps what it received.
func (s *Store) PutSpace(ctx context.Context, id string, kind spaces.Kind) (spaces.EncryptedSpace, error) {
	return s.PutSpaceOwned(ctx, id, kind, "")
}

// PutSpaceOwned is PutSpace recording the user principal that created the space
// (ADR-0104) — the one principal, besides any management-authorised device on a
// legacy space, whose device may grant an executor access to it. owner is
// recorded only when the space is first stored: a re-push never changes it, so a
// second device cannot claim a space another created. An empty owner records
// NULL, which is a legacy household space (and what replication writes: the
// owner is this node's fact, not the space's).
func (s *Store) PutSpaceOwned(ctx context.Context, id string, kind spaces.Kind, owner string) (spaces.EncryptedSpace, error) {
	if err := kind.Validate(); err != nil {
		return spaces.EncryptedSpace{}, err
	}
	if _, err := uuid.Parse(id); err != nil {
		// The peer treats the id as opaque, but a well-formed opaque handle is a
		// UUID here (spaces.NewSpace mints one) — refusing junk keeps a malformed
		// id from becoming a row nothing can ever reference correctly.
		return spaces.EncryptedSpace{}, fmt.Errorf("personalstate/store: a space id must be a uuid: %q: %w", id, err)
	}
	now := s.clock.Now().UTC()

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return spaces.EncryptedSpace{}, fmt.Errorf("personalstate/store: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// An existing space with a different kind is a conflict, not an overwrite.
	var existingKind string
	err = tx.QueryRowContext(ctx, `SELECT kind FROM encrypted_spaces WHERE id = ?`, id).Scan(&existingKind)
	switch {
	case err == nil:
		if existingKind != string(kind) {
			return spaces.EncryptedSpace{}, fmt.Errorf(
				"personalstate/store: space %s already exists as kind %q, refusing to re-record it as %q",
				id, existingKind, string(kind))
		}
		// Already held, same kind — idempotent no-op, no second event.
		return s.Space(ctx, id)
	case !errors.Is(err, sql.ErrNoRows):
		return spaces.EncryptedSpace{}, fmt.Errorf("personalstate/store: checking space: %w", err)
	}

	var ownerCol any
	if owner != "" {
		ownerCol = owner
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO encrypted_spaces (id, kind, created_at, owner_principal_id) VALUES (?, ?, ?, ?)`,
		id, string(kind), now.Format(timeFormat), ownerCol); err != nil {
		return spaces.EncryptedSpace{}, fmt.Errorf("personalstate/store: recording space: %w", err)
	}
	ev, err := s.events.EmitTx(ctx, tx, events.TypeSpaceCreated, "encrypted_space", id,
		map[string]any{"kind": string(kind)})
	if err != nil {
		return spaces.EncryptedSpace{}, fmt.Errorf("personalstate/store: recording space: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return spaces.EncryptedSpace{}, fmt.Errorf("personalstate/store: committing: %w", err)
	}
	s.events.Publish(ev)
	return spaces.EncryptedSpace{ID: id, Kind: kind, CreatedAt: now}, nil
}

// PutWrappedKey stores (or replaces) the sealed copy of a space's key for one
// recipient. The wrapped bytes are opaque — the peer holds them and cannot open
// them. Replacing an existing copy for the same (space, recipient) is how adding
// a recipient (or a re-wrap) lands: a recipient has exactly one current wrapped
// copy. The space must exist.
//
// epoch is the key epoch the copy seals (ADR-0103), and it must be the space's
// CURRENT epoch: an older one is ErrStaleKeyEpoch — the key was rotated since, so
// the copy would hand a recipient a superseded key (or, arriving by replication,
// resurrect a copy a rotation deliberately dropped); a newer one is
// ErrFutureKeyEpoch — the history row that moves the space to that epoch has to
// land first. Checked inside the write transaction, so a racing rotation cannot
// slip between the check and the upsert.
func (s *Store) PutWrappedKey(ctx context.Context, spaceID, recipient string, wrapped []byte, epoch int) (WrappedKey, error) {
	if recipient == "" {
		return WrappedKey{}, ErrEmptyRecipient
	}
	if len(wrapped) == 0 {
		return WrappedKey{}, ErrEmptyWrapped
	}
	now := s.clock.Now().UTC()

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return WrappedKey{}, fmt.Errorf("personalstate/store: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var exists string
	err = tx.QueryRowContext(ctx, `SELECT id FROM encrypted_spaces WHERE id = ?`, spaceID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return WrappedKey{}, fmt.Errorf("%w: %s", ErrUnknownSpace, spaceID)
	}
	if err != nil {
		return WrappedKey{}, fmt.Errorf("personalstate/store: checking space: %w", err)
	}

	current, err := keyEpochTx(ctx, tx, spaceID)
	if err != nil {
		return WrappedKey{}, err
	}
	switch {
	case epoch < current:
		return WrappedKey{}, fmt.Errorf("%w: space %s is at epoch %d, the copy seals epoch %d",
			ErrStaleKeyEpoch, spaceID, current, epoch)
	case epoch > current:
		return WrappedKey{}, fmt.Errorf("%w: space %s is at epoch %d, the copy seals epoch %d",
			ErrFutureKeyEpoch, spaceID, current, epoch)
	}

	ev, w, err := s.upsertWrapTx(ctx, tx, spaceID, recipient, wrapped, epoch, now)
	if err != nil {
		return WrappedKey{}, err
	}
	if err := tx.Commit(); err != nil {
		return WrappedKey{}, fmt.Errorf("personalstate/store: committing: %w", err)
	}
	s.events.Publish(ev)
	return w, nil
}

// upsertWrapTx writes one recipient's copy at an epoch inside tx and records it.
// Upsert on (space_id, recipient): a re-wrap replaces the recipient's copy in
// place; the row's identity is unimportant — the current bytes are — so the new
// id wins on conflict too.
func (s *Store) upsertWrapTx(ctx context.Context, tx *sql.Tx, spaceID, recipient string, wrapped []byte, epoch int, now time.Time) (events.Event, WrappedKey, error) {
	id := uuid.Must(uuid.NewV7()).String()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO wrapped_keys (id, space_id, recipient, wrapped, epoch, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (space_id, recipient)
		 DO UPDATE SET id = excluded.id, wrapped = excluded.wrapped, epoch = excluded.epoch,
		               created_at = excluded.created_at`,
		id, spaceID, recipient, wrapped, epoch, now.Format(timeFormat)); err != nil {
		return events.Event{}, WrappedKey{}, fmt.Errorf("personalstate/store: storing wrapped key: %w", err)
	}
	ev, err := s.events.EmitTx(ctx, tx, events.TypeSpaceKeyWrapped, "encrypted_space", spaceID,
		map[string]any{"recipient": recipient, "epoch": epoch})
	if err != nil {
		return events.Event{}, WrappedKey{}, fmt.Errorf("personalstate/store: recording wrap: %w", err)
	}
	return ev, WrappedKey{ID: id, SpaceID: spaceID, Recipient: recipient, Wrapped: wrapped, Epoch: epoch, CreatedAt: now}, nil
}

// Space returns one encrypted space by id.
func (s *Store) Space(ctx context.Context, id string) (spaces.EncryptedSpace, error) {
	row := s.reader.QueryRowContext(ctx,
		`SELECT id, kind, created_at FROM encrypted_spaces WHERE id = ?`, id)
	return scanSpace(row)
}

// ListSpaces returns every space this peer holds, most-recently-created first.
func (s *Store) ListSpaces(ctx context.Context) ([]spaces.EncryptedSpace, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT id, kind, created_at FROM encrypted_spaces ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("personalstate/store: listing spaces: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []spaces.EncryptedSpace{}
	for rows.Next() {
		sp, err := scanSpace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// WrappedKeysFor returns the wrapped copies of a space's key — what a device
// fetches to find the one sealed for it. The space must exist.
func (s *Store) WrappedKeysFor(ctx context.Context, spaceID string) ([]WrappedKey, error) {
	if _, err := s.Space(ctx, spaceID); err != nil {
		return nil, err
	}
	return wrappedKeysTx(ctx, s.reader, spaceID)
}

// queryer is what both a pool and a transaction offer for reads.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// wrappedKeysTx lists a space's wrapped copies through q (a pool or a tx).
func wrappedKeysTx(ctx context.Context, q queryer, spaceID string) ([]WrappedKey, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, space_id, recipient, wrapped, epoch, created_at
		 FROM wrapped_keys WHERE space_id = ? ORDER BY recipient`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("personalstate/store: listing wrapped keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []WrappedKey{}
	for rows.Next() {
		var w WrappedKey
		var created string
		if err := rows.Scan(&w.ID, &w.SpaceID, &w.Recipient, &w.Wrapped, &w.Epoch, &created); err != nil {
			return nil, fmt.Errorf("personalstate/store: reading wrapped key: %w", err)
		}
		if w.CreatedAt, err = time.Parse(timeFormat, created); err != nil {
			return nil, fmt.Errorf("personalstate/store: wrapped key %s has an unparseable created_at: %w", w.ID, err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// DeleteWrappedKey removes a recipient's wrapped copy of a space's key — the
// storage side of device revocation (§41, ADR-0022). After a rotation re-wraps
// the space for the REMAINING devices, the revoked device's copy is deleted here
// so a peer no longer serves it; the revoked device keeps only what it already
// downloaded (revocation is forward-looking, not retroactive). Deleting a copy
// that is not present is a no-op — revoking an already-revoked device is
// idempotent. The space must exist.
func (s *Store) DeleteWrappedKey(ctx context.Context, spaceID, recipient string) error {
	if _, err := s.Space(ctx, spaceID); err != nil {
		return err
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("personalstate/store: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`DELETE FROM wrapped_keys WHERE space_id = ? AND recipient = ?`, spaceID, recipient)
	if err != nil {
		return fmt.Errorf("personalstate/store: deleting wrapped key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Nothing to revoke — idempotent, no event.
		return tx.Commit()
	}
	ev, err := s.events.EmitTx(ctx, tx, events.TypeSpaceKeyRevoked, "encrypted_space", spaceID,
		map[string]any{"recipient": recipient})
	if err != nil {
		return fmt.Errorf("personalstate/store: recording revocation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("personalstate/store: committing: %w", err)
	}
	s.events.Publish(ev)
	return nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanSpace(row rowScanner) (spaces.EncryptedSpace, error) {
	var sp spaces.EncryptedSpace
	var kind, created string
	err := row.Scan(&sp.ID, &kind, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return spaces.EncryptedSpace{}, ErrUnknownSpace
	}
	if err != nil {
		return spaces.EncryptedSpace{}, fmt.Errorf("personalstate/store: reading space: %w", err)
	}
	sp.Kind = spaces.Kind(kind)
	if sp.CreatedAt, err = time.Parse(timeFormat, created); err != nil {
		return spaces.EncryptedSpace{}, fmt.Errorf("personalstate/store: space %s has an unparseable created_at: %w", sp.ID, err)
	}
	return sp, nil
}
