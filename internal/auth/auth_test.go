package auth_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

func newStore(t *testing.T) (*auth.Store, *fakeClock, *sqlite.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{t: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)}
	store, err := auth.NewStore(auth.StoreOptions{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	return store, clock, db
}

func newVerifier(t *testing.T, store *auth.Store) *auth.Verifier {
	t.Helper()
	v, err := auth.NewVerifier(auth.VerifierOptions{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func mustLastUsed(t *testing.T, store *auth.Store, id string) time.Time {
	t.Helper()
	tk, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if tk.LastUsedAt == nil {
		t.Fatalf("token %s has no last_used_at", id)
	}
	return *tk.LastUsedAt
}

func renderToken(t *testing.T, tk auth.Token) string {
	t.Helper()
	b, err := json.Marshal(tk)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestScopeAuthorityIsOrdered(t *testing.T) {
	tests := []struct {
		name string
		held []auth.Scope
		want auth.Scope
		ok   bool
	}{
		{"read satisfies read", []auth.Scope{auth.ScopeRead}, auth.ScopeRead, true},
		{"read does not satisfy write", []auth.Scope{auth.ScopeRead}, auth.ScopeWrite, false},
		{"read does not satisfy admin", []auth.Scope{auth.ScopeRead}, auth.ScopeAdmin, false},
		{"write implies read", []auth.Scope{auth.ScopeWrite}, auth.ScopeRead, true},
		{"write satisfies write", []auth.Scope{auth.ScopeWrite}, auth.ScopeWrite, true},
		{"write does not satisfy admin", []auth.Scope{auth.ScopeWrite}, auth.ScopeAdmin, false},
		{"admin implies write", []auth.Scope{auth.ScopeAdmin}, auth.ScopeWrite, true},
		{"admin implies read", []auth.Scope{auth.ScopeAdmin}, auth.ScopeRead, true},
		{"no scopes satisfies nothing", nil, auth.ScopeRead, false},
		{"an unknown requirement is never satisfied", []auth.Scope{auth.ScopeAdmin}, auth.Scope("root"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := auth.Allows(tt.held, tt.want); got != tt.ok {
				t.Errorf("Allows(%v, %q) = %v, want %v", tt.held, tt.want, got, tt.ok)
			}
		})
	}
}

func TestParseScopesRejectsWhatItDoesNotUnderstand(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "read", want: "read"},
		{in: "read,write", want: "read,write"},
		{in: "write,read", want: "read,write"}, // canonical order
		{in: "read,read", want: "read"},        // deduplicated
		{in: " admin , read ", want: "read,admin"},
		{in: "root", wantErr: true},
		{in: "read,root", wantErr: true}, // a typo must not silently grant read only
		{in: "", wantErr: true},
		{in: ",,", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := auth.ParseScopes(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseScopes(%q) = %v, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if auth.Join(got) != tt.want {
				t.Errorf("ParseScopes(%q) = %q, want %q", tt.in, auth.Join(got), tt.want)
			}
		})
	}
}

func TestSplitDropsScopesItDoesNotKnow(t *testing.T) {
	// A row written by a newer binary must grant what this one understands and
	// nothing more — not fail, and certainly not fail open.
	if got := auth.Join(auth.Split("read,teleport,write")); got != "read,write" {
		t.Errorf("Split = %q, want read,write", got)
	}
}

func TestTheStoredHashIsNotTheToken(t *testing.T) {
	store, _, db := newStore(t)
	ctx := context.Background()

	created, err := store.Create(ctx, "jellyfin", []auth.Scope{auth.ScopeRead}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Read the hash the way an attacker with the database file would.
	var hash string
	if err := db.Reader().QueryRowContext(ctx, `SELECT token_hash FROM api_tokens WHERE id = ?`, created.Token.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}

	if hash == created.Secret {
		t.Fatal("the plaintext token was stored")
	}
	if strings.Contains(hash, created.Secret) {
		t.Fatal("the stored hash contains the token")
	}
	// The secret half specifically, not just the whole string: the token
	// carries its row id in the clear by design, so a substring check on the
	// full token would pass even if the secret leaked.
	secretPart := created.Secret[strings.LastIndex(created.Secret, "_")+1:]
	if strings.Contains(hash, secretPart) {
		t.Fatal("the stored hash contains the token secret")
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("the hash is not argon2id: %q", hash)
	}
}

func TestVerifyRejectsAWrongSecretOfTheSameLength(t *testing.T) {
	store, _, _ := newStore(t)
	ctx := context.Background()
	v := newVerifier(t, store)

	created, err := store.Create(ctx, "sonarr", []auth.Scope{auth.ScopeRead}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Flip one character of the secret half, keeping the length and the
	// selector identical, so the lookup succeeds and only the comparison can
	// reject it.
	tampered := []byte(created.Secret)
	last := len(tampered) - 1
	if tampered[last] == 'a' {
		tampered[last] = 'b'
	} else {
		tampered[last] = 'a'
	}
	if len(tampered) != len(created.Secret) {
		t.Fatal("the test tampered with the length, not the value")
	}

	if _, err := v.Verify(ctx, string(tampered)); err == nil {
		t.Fatal("a tampered token was accepted")
	}
	if _, err := v.Verify(ctx, created.Secret); err != nil {
		t.Fatalf("the real token was rejected: %v", err)
	}
}

func TestVerifyRejectsRevokedAndExpiredTokens(t *testing.T) {
	store, clock, _ := newStore(t)
	ctx := context.Background()
	v := newVerifier(t, store)

	expiry := clock.t.Add(time.Hour)
	live, err := store.Create(ctx, "live", []auth.Scope{auth.ScopeRead}, nil)
	if err != nil {
		t.Fatal(err)
	}
	expiring, err := store.Create(ctx, "expiring", []auth.Scope{auth.ScopeRead}, &expiry)
	if err != nil {
		t.Fatal(err)
	}
	doomed, err := store.Create(ctx, "doomed", []auth.Scope{auth.ScopeRead}, nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, tk := range []auth.CreatedToken{live, expiring, doomed} {
		if _, err := v.Verify(ctx, tk.Secret); err != nil {
			t.Fatalf("a fresh token was rejected: %v", err)
		}
	}

	if _, err := store.Revoke(ctx, doomed.Token.ID); err != nil {
		t.Fatal(err)
	}
	// Revocation must bite immediately — not after a cache TTL. The token was
	// verified a moment ago, so this also proves the verified-token cache does
	// not cache authorisation.
	if _, err := v.Verify(ctx, doomed.Secret); err == nil {
		t.Error("a revoked token was accepted")
	} else if !strings.Contains(err.Error(), "revoked") {
		t.Errorf("revoked token error = %v, want it to say revoked", err)
	}

	// Advance the clock rather than sleeping.
	clock.t = expiry.Add(time.Second)
	if _, err := v.Verify(ctx, expiring.Secret); err == nil {
		t.Error("an expired token was accepted")
	} else if !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired token error = %v, want it to say expired", err)
	}
	if _, err := v.Verify(ctx, live.Secret); err != nil {
		t.Errorf("a token with no expiry was rejected after the clock moved: %v", err)
	}
}

func TestVerifyRejectsMalformedCredentials(t *testing.T) {
	store, _, _ := newStore(t)
	v := newVerifier(t, store)
	ctx := context.Background()

	created, err := store.Create(ctx, "svc", []auth.Scope{auth.ScopeRead}, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := created.Secret[len(auth.TokenPrefix):strings.LastIndex(created.Secret, "_")]

	for _, tt := range []struct{ name, token string }{
		{"empty", ""},
		{"no prefix", "deadbeef"},
		{"prefix only", auth.TokenPrefix},
		{"no separator", auth.TokenPrefix + "abcdef"},
		{"unknown selector", auth.TokenPrefix + "aaaaaaaaaaaaaaaaaaaaaaaaaa_" + strings.Repeat("a", 52)},
		{"selector without a secret", auth.TokenPrefix + id + "_"},
		{"truncated secret", created.Secret[:len(created.Secret)-4]},
		{"not base32", auth.TokenPrefix + id + "_" + strings.Repeat("!", 52)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := v.Verify(ctx, tt.token); err == nil {
				t.Error("a malformed credential was accepted")
			}
		})
	}
}

func TestTokensAreDistinctAndCarryEnoughEntropy(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		_, raw, secret, err := auth.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(secret) < 32 {
			t.Fatalf("the secret is %d bytes, want at least 32 (256 bits)", len(secret))
		}
		if !strings.HasPrefix(raw, auth.TokenPrefix) {
			t.Fatalf("token %q has no recognisable prefix — a leaked credential nobody can spot is one nobody revokes", raw)
		}
		if seen[raw] {
			t.Fatal("NewToken repeated itself")
		}
		seen[raw] = true
	}
}

func TestLastUsedIsRecordedButThrottled(t *testing.T) {
	store, clock, _ := newStore(t)
	ctx := context.Background()
	v, err := auth.NewVerifier(auth.VerifierOptions{Store: store, TouchInterval: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.Create(ctx, "player", []auth.Scope{auth.ScopeRead}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := v.Verify(ctx, created.Secret); err != nil {
		t.Fatal(err)
	}
	first := mustLastUsed(t, store, created.Token.ID)

	// A second request a moment later must NOT write again: authentication is
	// on the hot path of every request and the database is single-writer.
	clock.t = clock.t.Add(time.Second)
	if _, err := v.Verify(ctx, created.Secret); err != nil {
		t.Fatal(err)
	}
	if got := mustLastUsed(t, store, created.Token.ID); !got.Equal(first) {
		t.Errorf("last_used_at was rewritten one second later (%s then %s) — the throttle is not working", first, got)
	}

	// Past the interval it must write again, or the field is useless.
	clock.t = clock.t.Add(10 * time.Minute)
	if _, err := v.Verify(ctx, created.Secret); err != nil {
		t.Fatal(err)
	}
	if got := mustLastUsed(t, store, created.Token.ID); !got.After(first) {
		t.Errorf("last_used_at was never refreshed: still %s", got)
	}
}

func TestCreateReusesThePrincipalForTheSameName(t *testing.T) {
	store, _, _ := newStore(t)
	ctx := context.Background()

	a, err := store.Create(ctx, "jellyfin", []auth.Scope{auth.ScopeRead}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Create(ctx, "jellyfin", []auth.Scope{auth.ScopeRead, auth.ScopeWrite}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a.Token.PrincipalID != b.Token.PrincipalID {
		t.Error("rotating a token created a second identity with the same name")
	}
	if a.Token.ID == b.Token.ID {
		t.Error("two tokens share an id")
	}
}

func TestRevokeReportsASecondRevocation(t *testing.T) {
	store, _, _ := newStore(t)
	ctx := context.Background()

	created, err := store.Create(ctx, "svc", []auth.Scope{auth.ScopeRead}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Revoke(ctx, created.Token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Revoke(ctx, created.Token.ID); err == nil {
		t.Error("revoking twice reported success — a script cannot tell it did nothing")
	}
	if _, err := store.Revoke(ctx, "no-such-token"); err == nil {
		t.Error("revoking a token that does not exist reported success")
	}
}

func TestListNeverReturnsCredentialMaterial(t *testing.T) {
	store, _, _ := newStore(t)
	ctx := context.Background()

	created, err := store.Create(ctx, "svc", []auth.Scope{auth.ScopeAdmin}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 {
		t.Fatalf("List returned %d tokens, want 1", len(tokens))
	}
	// The Token struct has no hash or secret field. This asserts the rendered
	// form has none either, which is what a future "just add the hash for
	// debugging" change would break.
	rendered := renderToken(t, tokens[0])
	if strings.Contains(rendered, created.Secret) || strings.Contains(rendered, "argon2") {
		t.Errorf("a token listing carried credential material: %s", rendered)
	}
}

// Unpadded base32 leaves spare bits in the final character, so a string can be
// mutated and still decode. Two things must hold: a non-canonical spelling is
// rejected outright, and a spelling that IS canonical decodes to a different
// secret and fails verification. Between them, one credential has exactly one
// accepted form.
func TestATokenHasExactlyOneSpelling(t *testing.T) {
	store, _, _ := newStore(t)
	v := newVerifier(t, store)
	ctx := context.Background()

	created, err := store.Create(ctx, "svc", []auth.Scope{auth.ScopeRead}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(ctx, created.Secret); err != nil {
		t.Fatalf("the canonical token was rejected: %v", err)
	}

	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	last := len(created.Secret) - 1
	nonCanonical, accepted := 0, 0
	for _, c := range alphabet {
		if byte(c) == created.Secret[last] {
			continue
		}
		variant := created.Secret[:last] + string(c)
		if _, err := auth.ParseToken(variant); err != nil {
			nonCanonical++
			continue
		}
		if _, err := v.Verify(ctx, variant); err == nil {
			accepted++
		}
	}
	if accepted != 0 {
		t.Errorf("%d mutated tokens authenticated", accepted)
	}
	if nonCanonical == 0 {
		t.Error("no non-canonical spelling was rejected at parse time — the canonical check is not working")
	}
}
