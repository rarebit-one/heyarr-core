package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/auth"
)

// An executor token (ADR-0104) is restricted, acts as an executor principal and
// carries read+write and never admin; verifying it yields a Restricted identity.
func TestAnExecutorTokenIsRestricted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _, _ := newStore(t)

	created, err := store.CreateExecutor(ctx, "runner", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !created.Token.Restricted {
		t.Error("CreateExecutor minted an unrestricted token")
	}
	if got := auth.Join(created.Token.Scopes); got != "read,write" {
		t.Errorf("executor scopes = %q, want read,write", got)
	}

	id, err := newVerifier(t, store).Verify(ctx, created.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if !id.Restricted || id.Principal.Kind != auth.KindExecutor {
		t.Errorf("verified identity restricted=%v kind=%q, want true / executor", id.Restricted, id.Principal.Kind)
	}
	if id.Allows(auth.ScopeAdmin) {
		t.Error("an executor token carries admin")
	}

	stored, err := store.Get(ctx, created.Token.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Restricted {
		t.Error("the stored token lost its restriction")
	}
}

// The regression guard for every existing credential: an ordinary token is
// unrestricted, its identity is unrestricted, and its JSON carries no new field.
func TestAnOrdinaryTokenIsUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _, _ := newStore(t)

	created, err := store.Create(ctx, "player", []auth.Scope{auth.ScopeRead, auth.ScopeWrite, auth.ScopeAdmin}, nil)
	if err != nil {
		t.Fatal(err)
	}
	id, err := newVerifier(t, store).Verify(ctx, created.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if id.Restricted || created.Token.Restricted {
		t.Error("an ordinary token came back restricted")
	}
	if id.Principal.Kind != auth.KindService {
		t.Errorf("principal kind = %q, want service", id.Principal.Kind)
	}
	if strings.Contains(renderToken(t, created.Token), "restricted") {
		t.Error("an ordinary token's JSON grew a restricted field")
	}
}

// A name is one kind of principal: an executor's name cannot be given an
// ordinary token (which would be an unconfined credential for a confined
// principal), nor a service's name a restricted one.
func TestAPrincipalNameHasOneKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _, _ := newStore(t)

	if _, err := store.CreateExecutor(ctx, "runner", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, "runner", []auth.Scope{auth.ScopeRead}, nil); !errors.Is(err, auth.ErrPrincipalKind) {
		t.Errorf("an ordinary token for an executor's name: err = %v, want ErrPrincipalKind", err)
	}
	if _, err := store.Create(ctx, "player", []auth.Scope{auth.ScopeRead}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateExecutor(ctx, "player", nil); !errors.Is(err, auth.ErrPrincipalKind) {
		t.Errorf("an executor token for a service's name: err = %v, want ErrPrincipalKind", err)
	}
	// Rotation still works within a kind.
	if _, err := store.CreateExecutor(ctx, "runner", nil); err != nil {
		t.Errorf("a second executor token for the same executor: %v", err)
	}
}

func TestExecutorPrincipalResolvesOnlyExecutors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _, _ := newStore(t)

	exe, err := store.CreateExecutor(ctx, "runner", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, "player", []auth.Scope{auth.ScopeRead}, nil); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"runner", exe.Token.PrincipalID} {
		p, err := store.ExecutorPrincipal(ctx, ref)
		if err != nil || p.ID != exe.Token.PrincipalID || p.Kind != auth.KindExecutor {
			t.Errorf("ExecutorPrincipal(%q) = %+v, %v", ref, p, err)
		}
	}
	if _, err := store.ExecutorPrincipal(ctx, "player"); !errors.Is(err, auth.ErrPrincipalKind) {
		t.Errorf("a service principal resolved as an executor: %v", err)
	}
	if _, err := store.ExecutorPrincipal(ctx, "nobody"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("an unknown principal: %v", err)
	}
}
