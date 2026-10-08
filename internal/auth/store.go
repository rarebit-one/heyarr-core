package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const timeFormat = time.RFC3339Nano

// Clock is injected everywhere time is read, so expiry is an ordinary unit test
// rather than a sleep (ADR-0017).
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// Errors returned by the store.
var (
	// ErrNotFound means no such token or principal.
	ErrNotFound = errors.New("auth: not found")
	// ErrRevoked means the token exists but has been revoked.
	ErrRevoked = errors.New("auth: token revoked")
	// ErrExpired means the token exists but has passed its expiry.
	ErrExpired = errors.New("auth: token expired")
	// ErrBadSecret means the selector resolved but the verifier did not match.
	ErrBadSecret = errors.New("auth: token secret does not match")
	// ErrPrincipalKind means a name is already a principal of a different kind:
	// an executor's name cannot be given an ordinary token, nor a service's name
	// a restricted one (ADR-0104). Silently minting across kinds would be how a
	// confined principal ends up holding an unconfined credential.
	ErrPrincipalKind = errors.New("auth: that name is a principal of a different kind")
)

// The principal kinds (ADR-0011, ADR-0104).
const (
	// KindService is a named service holding ordinary bearer tokens.
	KindService = "service"
	// KindUser is an enrolled person, authenticated by a device credential.
	KindUser = "user"
	// KindExecutor is a service confined to the vault surface and to the
	// encrypted spaces it holds a grant for (ADR-0104). Every token it holds is
	// restricted; it is never a member device and never wraps a key.
	KindExecutor = "executor"
)

// Principal is who a token acts as. Milestone 1 creates only `service`
// principals; the `user` kind exists so Milestone 8 is additive (ADR-0011), and
// `executor` is the restricted kind of ADR-0104.
type Principal struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Token is an API credential as stored. It never carries the secret: after
// creation the secret does not exist anywhere Heyarr can reach.
type Token struct {
	ID          string     `json:"id"`
	PrincipalID string     `json:"principal_id"`
	Name        string     `json:"name"`
	Scopes      []Scope    `json:"scopes"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at"`
	// Restricted confines the token to the vault surface and to the spaces its
	// principal holds a grant for (ADR-0104). False for every ordinary token,
	// and omitted from JSON then, so an existing token's shape is unchanged.
	Restricted bool `json:"restricted,omitempty"`
}

// Active reports whether the token may be used at t.
func (tk Token) Active(t time.Time) error {
	if tk.RevokedAt != nil {
		return ErrRevoked
	}
	if tk.ExpiresAt != nil && !t.Before(*tk.ExpiresAt) {
		return ErrExpired
	}
	return nil
}

// Store is the credential table.
type Store struct {
	writer *sql.DB
	reader *sql.DB
	clock  Clock
}

// StoreOptions configure a Store.
type StoreOptions struct {
	// Writer must be the single-writer pool (ADR-0003).
	Writer *sql.DB
	// Reader serves the per-request lookup, which must never queue behind a
	// write — authentication is on the hot path of every API call.
	Reader *sql.DB
	Clock  Clock
}

// NewStore constructs a Store.
func NewStore(opts StoreOptions) (*Store, error) {
	if opts.Writer == nil {
		return nil, errors.New("auth: a writer database is required")
	}
	reader := opts.Reader
	if reader == nil {
		reader = opts.Writer
	}
	clock := opts.Clock
	if clock == nil {
		clock = systemClock{}
	}
	return &Store{writer: opts.Writer, reader: reader, clock: clock}, nil
}

// Now reports the store's current time.
func (s *Store) Now() time.Time { return s.clock.Now() }

// CreatedToken is the one-time result of minting a credential.
type CreatedToken struct {
	Token Token
	// Secret is the plaintext credential. It is returned exactly once and is
	// not recoverable: only its argon2id hash was stored.
	Secret string
}

// Create mints a token for the named principal, creating the principal if it
// does not exist yet.
//
// Principal-per-name is intentional for Milestone 1: `heyarr token create
// jellyfin` twice gives one principal with two tokens, which is what rotation
// looks like, rather than two identities that happen to share a name. A name
// that is already an executor principal is refused (ErrPrincipalKind): an
// executor holds only restricted tokens (ADR-0104).
func (s *Store) Create(ctx context.Context, name string, scopes []Scope, expiresAt *time.Time) (CreatedToken, error) {
	return s.create(ctx, name, KindService, scopes, expiresAt, false)
}

// ExecutorScopes is what every executor token carries: read and write, never
// admin. The restriction, not the scope, is what confines it (ADR-0104).
var ExecutorScopes = []Scope{ScopeRead, ScopeWrite}

// CreateExecutor mints a RESTRICTED token for the named executor principal,
// creating the principal (kind executor) if it does not exist yet (ADR-0104).
// The token carries read and write, and reaches only the vault surface and the
// encrypted spaces the principal holds a grant for. A name already held by a
// service or user principal is refused (ErrPrincipalKind).
func (s *Store) CreateExecutor(ctx context.Context, name string, expiresAt *time.Time) (CreatedToken, error) {
	return s.create(ctx, name, KindExecutor, ExecutorScopes, expiresAt, true)
}

func (s *Store) create(ctx context.Context, name, kind string, scopes []Scope, expiresAt *time.Time, restricted bool) (CreatedToken, error) {
	if name == "" {
		return CreatedToken{}, errors.New("auth: a token needs a name — it is how you will recognise it in `heyarr token list`")
	}
	if len(scopes) == 0 {
		return CreatedToken{}, errors.New("auth: a token needs at least one scope")
	}
	now := s.clock.Now()

	id, raw, secret, err := NewToken()
	if err != nil {
		return CreatedToken{}, err
	}
	hash, err := HashSecret(secret)
	if err != nil {
		return CreatedToken{}, err
	}

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return CreatedToken{}, fmt.Errorf("auth: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var principalID, existingKind string
	err = tx.QueryRowContext(ctx, `SELECT id, kind FROM principals WHERE name = ?`, name).Scan(&principalID, &existingKind)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		principalID = uuid.Must(uuid.NewV7()).String()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO principals (id, kind, name, created_at) VALUES (?, ?, ?, ?)`,
			principalID, kind, name, now.Format(timeFormat)); err != nil {
			return CreatedToken{}, fmt.Errorf("auth: creating principal %q: %w", name, err)
		}
	case err != nil:
		return CreatedToken{}, fmt.Errorf("auth: looking up principal %q: %w", name, err)
	case (existingKind == KindExecutor) != restricted:
		return CreatedToken{}, fmt.Errorf("%w: %q is a %s principal", ErrPrincipalKind, name, existingKind)
	}

	var expires any
	if expiresAt != nil {
		expires = expiresAt.UTC().Format(timeFormat)
	}
	restrictedCol := 0
	if restricted {
		restrictedCol = 1
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO api_tokens (id, principal_id, name, token_hash, scopes, created_at, expires_at, restricted)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, principalID, name, hash, Join(scopes), now.Format(timeFormat), expires, restrictedCol); err != nil {
		return CreatedToken{}, fmt.Errorf("auth: storing token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return CreatedToken{}, fmt.Errorf("auth: committing token: %w", err)
	}

	return CreatedToken{
		Token: Token{
			ID: id, PrincipalID: principalID, Name: name,
			Scopes: Sort(scopes), CreatedAt: now, ExpiresAt: expiresAt,
			Restricted: restricted,
		},
		Secret: raw,
	}, nil
}

const tokenColumns = `id, principal_id, name, scopes, created_at, last_used_at, expires_at, revoked_at, restricted`

func scanToken(rows interface{ Scan(...any) error }) (Token, error) {
	var (
		tk                         Token
		scopes, created            string
		lastUsed, expires, revoked sql.NullString
	)
	if err := rows.Scan(&tk.ID, &tk.PrincipalID, &tk.Name, &scopes, &created, &lastUsed, &expires, &revoked, &tk.Restricted); err != nil {
		return Token{}, err
	}
	tk.Scopes = Split(scopes)
	var err error
	if tk.CreatedAt, err = time.Parse(timeFormat, created); err != nil {
		return Token{}, fmt.Errorf("auth: token %s has an unparseable created_at: %w", tk.ID, err)
	}
	for _, f := range []struct {
		src sql.NullString
		dst **time.Time
		col string
	}{{lastUsed, &tk.LastUsedAt, "last_used_at"}, {expires, &tk.ExpiresAt, "expires_at"}, {revoked, &tk.RevokedAt, "revoked_at"}} {
		if !f.src.Valid || f.src.String == "" {
			continue
		}
		t, err := time.Parse(timeFormat, f.src.String)
		if err != nil {
			return Token{}, fmt.Errorf("auth: token %s has an unparseable %s: %w", tk.ID, f.col, err)
		}
		tt := t
		*f.dst = &tt
	}
	return tk, nil
}

// Get returns one token by id.
func (s *Store) Get(ctx context.Context, id string) (Token, error) {
	row := s.reader.QueryRowContext(ctx, `SELECT `+tokenColumns+` FROM api_tokens WHERE id = ?`, id)
	tk, err := scanToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	return tk, err
}

// List returns every token, newest first. It never returns a hash: an
// administrative listing that prints credential material is a listing that ends
// up in a support ticket.
func (s *Store) List(ctx context.Context) ([]Token, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT `+tokenColumns+` FROM api_tokens ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("auth: listing tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []Token{}
	for rows.Next() {
		tk, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, tk)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("auth: listing tokens: %w", err)
	}
	return out, nil
}

// Revoke marks a token unusable. It is idempotent in effect but reports a
// second revocation, so a script cannot mistake "already revoked" for "revoked
// something just now".
func (s *Store) Revoke(ctx context.Context, id string) (Token, error) {
	tk, err := s.Get(ctx, id)
	if err != nil {
		return Token{}, err
	}
	if tk.RevokedAt != nil {
		return tk, ErrRevoked
	}
	now := s.clock.Now()
	if _, err := s.writer.ExecContext(ctx,
		`UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		now.Format(timeFormat), id); err != nil {
		return Token{}, fmt.Errorf("auth: revoking token %s: %w", id, err)
	}
	tk.RevokedAt = &now
	return tk, nil
}

// TouchLastUsed records that a token was used. See Verifier for why this is not
// called on every request.
func (s *Store) TouchLastUsed(ctx context.Context, id string, at time.Time) error {
	if _, err := s.writer.ExecContext(ctx,
		`UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, at.UTC().Format(timeFormat), id); err != nil {
		return fmt.Errorf("auth: recording last use of token %s: %w", id, err)
	}
	return nil
}

// PrincipalOf returns the principal a token acts as.
func (s *Store) PrincipalOf(ctx context.Context, tokenID string) (Principal, error) {
	var (
		p       Principal
		created string
	)
	err := s.reader.QueryRowContext(ctx,
		`SELECT p.id, p.kind, p.name, p.created_at FROM principals p
		 JOIN api_tokens t ON t.principal_id = p.id WHERE t.id = ?`, tokenID).
		Scan(&p.ID, &p.Kind, &p.Name, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrNotFound
	}
	if err != nil {
		return Principal{}, fmt.Errorf("auth: loading principal for token %s: %w", tokenID, err)
	}
	if p.CreatedAt, err = time.Parse(timeFormat, created); err != nil {
		return Principal{}, fmt.Errorf("auth: principal %s has an unparseable created_at: %w", p.ID, err)
	}
	return p, nil
}

// hashOf reads the stored argon2id hash for a token.
func (s *Store) hashOf(ctx context.Context, id string) (string, error) {
	var hash string
	err := s.reader.QueryRowContext(ctx, `SELECT token_hash FROM api_tokens WHERE id = ?`, id).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("auth: reading token %s: %w", id, err)
	}
	return hash, nil
}

// ExecutorPrincipal resolves ref — a principal id or name — to an executor
// principal (ADR-0104). A grant names the principal it admits, and only an
// executor is worth admitting: a user or service principal is not confined by
// a grant, so granting one would read as access control while changing nothing.
// ErrNotFound when no principal matches; ErrPrincipalKind when one does but is
// not an executor.
func (s *Store) ExecutorPrincipal(ctx context.Context, ref string) (Principal, error) {
	var (
		p       Principal
		created string
	)
	err := s.reader.QueryRowContext(ctx,
		`SELECT id, kind, name, created_at FROM principals WHERE id = ? OR name = ?
		 ORDER BY (id = ?) DESC LIMIT 1`, ref, ref, ref).
		Scan(&p.ID, &p.Kind, &p.Name, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrNotFound
	}
	if err != nil {
		return Principal{}, fmt.Errorf("auth: resolving principal %q: %w", ref, err)
	}
	if p.Kind != KindExecutor {
		return Principal{}, fmt.Errorf("%w: %q is a %s principal, not an executor", ErrPrincipalKind, ref, p.Kind)
	}
	if p.CreatedAt, err = time.Parse(timeFormat, created); err != nil {
		return Principal{}, fmt.Errorf("auth: principal %s has an unparseable created_at: %w", p.ID, err)
	}
	return p, nil
}
