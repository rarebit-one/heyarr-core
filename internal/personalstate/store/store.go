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

// A WrappedKey is one recipient's sealed copy of a space key, as the peer holds
// it. Wrapped is opaque: it is encryption.Seal output (e_pub ‖ nonce ‖
// ciphertext), and this package never looks inside it.
type WrappedKey struct {
	ID        string
	SpaceID   string
	Recipient string // "x25519:<hex>" — a device or the recovery encryption key
	Wrapped   []byte
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

// PutWrappedKey stores (or replaces) the sealed copy of a space's key for one
// recipient. The wrapped bytes are opaque — the peer holds them and cannot open
// them. Replacing an existing copy for the same (space, recipient) is how a
// re-wrap after revocation (§41, ADR-0022) lands: a recipient has exactly one
// current wrapped copy. The space must exist.
func (s *Store) PutWrappedKey(ctx context.Context, spaceID, recipient string, wrapped []byte) (WrappedKey, error) {
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

	id := uuid.Must(uuid.NewV7()).String()
	// Upsert on (space_id, recipient): a re-wrap replaces the recipient's copy in
	// place, keeping the row's identity stable is unimportant — the current bytes
	// are — so the new id wins on conflict too.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO wrapped_keys (id, space_id, recipient, wrapped, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (space_id, recipient)
		 DO UPDATE SET id = excluded.id, wrapped = excluded.wrapped, created_at = excluded.created_at`,
		id, spaceID, recipient, wrapped, now.Format(timeFormat)); err != nil {
		return WrappedKey{}, fmt.Errorf("personalstate/store: storing wrapped key: %w", err)
	}
	ev, err := s.events.EmitTx(ctx, tx, events.TypeSpaceKeyWrapped, "encrypted_space", spaceID,
		map[string]any{"recipient": recipient})
	if err != nil {
		return WrappedKey{}, fmt.Errorf("personalstate/store: recording wrap: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return WrappedKey{}, fmt.Errorf("personalstate/store: committing: %w", err)
	}
	s.events.Publish(ev)
	return WrappedKey{ID: id, SpaceID: spaceID, Recipient: recipient, Wrapped: wrapped, CreatedAt: now}, nil
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
	rows, err := s.reader.QueryContext(ctx,
		`SELECT id, space_id, recipient, wrapped, created_at
		 FROM wrapped_keys WHERE space_id = ? ORDER BY recipient`, spaceID)
	if err != nil {
		return nil, fmt.Errorf("personalstate/store: listing wrapped keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []WrappedKey{}
	for rows.Next() {
		var w WrappedKey
		var created string
		if err := rows.Scan(&w.ID, &w.SpaceID, &w.Recipient, &w.Wrapped, &created); err != nil {
			return nil, fmt.Errorf("personalstate/store: reading wrapped key: %w", err)
		}
		if w.CreatedAt, err = time.Parse(timeFormat, created); err != nil {
			return nil, fmt.Errorf("personalstate/store: wrapped key %s has an unparseable created_at: %w", w.ID, err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
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
