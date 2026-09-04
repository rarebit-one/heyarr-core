package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Identity is a successfully authenticated caller.
type Identity struct {
	Token     Token
	Principal Principal
	// Anonymous marks the synthetic identity used when authentication is
	// disabled — which configuration only permits on a loopback listener.
	Anonymous bool
	// Guest marks the anonymous, read-only browse identity admitted when guest
	// mode is enabled and a caller presents no credential (ADR-0074). It holds
	// only `read`, so scope already keeps it off every write and admin route;
	// the marker exists for the read routes a scope check cannot distinguish —
	// per-identity state a Guest must not see even though it may read the
	// shared library (RefuseGuest).
	Guest bool
}

// Allows reports whether this identity may perform an action requiring want.
func (i Identity) Allows(want Scope) bool { return Allows(i.Token.Scopes, want) }

// Verifier authenticates presented bearer tokens.
//
// # Why there is a cache here
//
// argon2id at the parameters in token.go costs tens of milliseconds and 64 MiB
// per verification, by design — that is what makes a stolen database useless.
// Paying it on every request would put a hard ceiling of a few dozen requests
// per second on an API whose whole job is to serve a media library, and a
// range-heavy player issues a lot of requests.
//
// The trade-off taken is a small in-process cache of *verifications*, keyed by
// a SHA-256 digest of the presented credential and holding only a token id and
// an expiry. What is emphatically NOT cached is authorisation: every request
// still reads the token row, so revocation, expiry and a scope change take
// effect on the very next request rather than up to a TTL later. The cache
// removes the argon2 cost and nothing else.
//
// What this concedes: an attacker with read access to this process's memory
// learns which token digests are valid — which is strictly less than they
// already have, since they can read the request headers going past. And a
// process restart pays argon2 once per distinct token, which is the correct
// place for that cost to land.
type Verifier struct {
	store *Store

	mu    sync.Mutex
	cache map[[sha256.Size]byte]cacheEntry
	touch map[string]time.Time

	ttl           time.Duration
	maxEntries    int
	touchInterval time.Duration
}

type cacheEntry struct {
	tokenID string
	expires time.Time
}

// Verifier defaults.
const (
	// DefaultCacheTTL bounds how long a verification is reused. It exists to
	// bound memory and to re-pay argon2 occasionally, not as a revocation
	// window — revocation is immediate because the row is always read.
	DefaultCacheTTL = 10 * time.Minute
	// DefaultCacheSize bounds the cache. Ten thousand entries is far more
	// distinct credentials than a homelab will ever have; it is a guard against
	// an attacker growing the map with garbage, not a tuning parameter.
	DefaultCacheSize = 10_000
	// DefaultTouchInterval throttles last_used_at.
	DefaultTouchInterval = 5 * time.Minute
)

// VerifierOptions configure a Verifier.
type VerifierOptions struct {
	Store *Store
	// CacheTTL, CacheSize and TouchInterval are zero-means-default.
	CacheTTL      time.Duration
	CacheSize     int
	TouchInterval time.Duration
}

// NewVerifier constructs a Verifier.
func NewVerifier(opts VerifierOptions) (*Verifier, error) {
	if opts.Store == nil {
		return nil, errors.New("auth: a store is required")
	}
	v := &Verifier{
		store:         opts.Store,
		cache:         make(map[[sha256.Size]byte]cacheEntry),
		touch:         make(map[string]time.Time),
		ttl:           opts.CacheTTL,
		maxEntries:    opts.CacheSize,
		touchInterval: opts.TouchInterval,
	}
	if v.ttl <= 0 {
		v.ttl = DefaultCacheTTL
	}
	if v.maxEntries <= 0 {
		v.maxEntries = DefaultCacheSize
	}
	if v.touchInterval <= 0 {
		v.touchInterval = DefaultTouchInterval
	}
	return v, nil
}

// Verify authenticates a presented credential.
//
// The error is for the log. Callers must not relay it to the client: "no such
// token" and "wrong secret" are different facts, and telling an unauthorised
// caller which one applies is free reconnaissance.
func (v *Verifier) Verify(ctx context.Context, raw string) (Identity, error) {
	presented, err := ParseToken(raw)
	if err != nil {
		return Identity{}, err
	}

	tk, principal, err := v.load(ctx, presented.ID)
	if err != nil {
		return Identity{}, err
	}

	now := v.store.Now()
	if err := tk.Active(now); err != nil {
		return Identity{}, err
	}

	key := cacheKey(presented.Raw)
	if !v.cached(key, tk.ID, now) {
		hash, err := v.store.hashOf(ctx, tk.ID)
		if err != nil {
			return Identity{}, err
		}
		ok, err := VerifySecret(hash, presented.Secret)
		if err != nil {
			return Identity{}, err
		}
		if !ok {
			return Identity{}, ErrBadSecret
		}
		v.remember(key, tk.ID, now)
	}

	v.recordUse(ctx, tk.ID, now)
	return Identity{Token: tk, Principal: principal}, nil
}

// load reads the token and its principal in one query. It is on the hot path of
// every authenticated request, so it is one round trip on the read pool rather
// than two.
func (v *Verifier) load(ctx context.Context, id string) (Token, Principal, error) {
	row := v.store.reader.QueryRowContext(ctx,
		`SELECT t.id, t.principal_id, t.name, t.scopes, t.created_at, t.last_used_at,
		        t.expires_at, t.revoked_at, p.kind, p.name, p.created_at
		 FROM api_tokens t JOIN principals p ON p.id = t.principal_id
		 WHERE t.id = ?`, id)

	var (
		tk                         Token
		scopes, created            string
		lastUsed, expires, revoked sql.NullString
		pKind, pName, pCreated     string
	)
	err := row.Scan(&tk.ID, &tk.PrincipalID, &tk.Name, &scopes, &created, &lastUsed,
		&expires, &revoked, &pKind, &pName, &pCreated)
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, Principal{}, ErrNotFound
	}
	if err != nil {
		return Token{}, Principal{}, fmt.Errorf("auth: loading token: %w", err)
	}

	tk.Scopes = Split(scopes)
	if tk.CreatedAt, err = time.Parse(timeFormat, created); err != nil {
		return Token{}, Principal{}, fmt.Errorf("auth: token %s has an unparseable created_at: %w", tk.ID, err)
	}
	for _, f := range []struct {
		src sql.NullString
		dst **time.Time
	}{{lastUsed, &tk.LastUsedAt}, {expires, &tk.ExpiresAt}, {revoked, &tk.RevokedAt}} {
		if !f.src.Valid || f.src.String == "" {
			continue
		}
		t, err := time.Parse(timeFormat, f.src.String)
		if err != nil {
			return Token{}, Principal{}, fmt.Errorf("auth: token %s has an unparseable timestamp: %w", tk.ID, err)
		}
		tt := t
		*f.dst = &tt
	}

	p := Principal{ID: tk.PrincipalID, Kind: pKind, Name: pName}
	if p.CreatedAt, err = time.Parse(timeFormat, pCreated); err != nil {
		return Token{}, Principal{}, fmt.Errorf("auth: principal %s has an unparseable created_at: %w", p.ID, err)
	}
	return tk, p, nil
}

func (v *Verifier) cached(key [sha256.Size]byte, tokenID string, now time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	e, ok := v.cache[key]
	if !ok || now.After(e.expires) || e.tokenID != tokenID {
		return false
	}
	return true
}

func (v *Verifier) remember(key [sha256.Size]byte, tokenID string, now time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.cache) >= v.maxEntries {
		// Drop what has expired first; if that frees nothing, drop everything.
		// An LRU here would be more code and better hit rates in a scenario
		// that does not exist: this map is sized for a handful of credentials
		// and only grows past that under attack, where discarding it is right.
		for k, e := range v.cache {
			if now.After(e.expires) {
				delete(v.cache, k)
			}
		}
		if len(v.cache) >= v.maxEntries {
			clear(v.cache)
		}
	}
	v.cache[key] = cacheEntry{tokenID: tokenID, expires: now.Add(v.ttl)}
}

// recordUse updates last_used_at, at most once per token per TouchInterval.
//
// Authentication happens on every request; a write on every request would turn
// a read-only workload into a write-heavy one against a single-writer database
// (ADR-0003), and last_used_at is an operator convenience — "is anything still
// using this token?" — that is answered just as well at five-minute resolution.
// A failure is logged nowhere and swallowed here on purpose: refusing a valid
// request because a bookkeeping write failed would be the wrong trade.
func (v *Verifier) recordUse(ctx context.Context, tokenID string, now time.Time) {
	v.mu.Lock()
	last, ok := v.touch[tokenID]
	if ok && now.Sub(last) < v.touchInterval {
		v.mu.Unlock()
		return
	}
	v.touch[tokenID] = now
	v.mu.Unlock()
	_ = v.store.TouchLastUsed(ctx, tokenID, now)
}
