package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

// authorised asks the question the auth path asks of a device credential
// (httpapi.ManagementAuthorizer), so the test proves the grant is one the
// server will honour, not merely a row.
func authorised(t *testing.T, cfg, key string) bool {
	t.Helper()
	var ok bool
	if err := withCatalog(context.Background(), cfg, func(ctx context.Context, c *catalog.Catalog) error {
		var err error
		ok, err = c.ManagementAuthorized(ctx, key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestAdminManagementGrantListRevoke(t *testing.T) {
	cfg := tokenConfig(t)
	ctx := context.Background()
	key := fixedUserKey(3)

	if authorised(t, cfg, key) {
		t.Fatal("a device was authorised before any grant")
	}
	out, _, err := run(t, ctx, "--config", cfg, "admin", "management", "grant", key, "--reason", "owner laptop")
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if !strings.Contains(out, key) || !authorised(t, cfg, key) {
		t.Fatalf("after grant: output %q, authorised %v", out, authorised(t, cfg, key))
	}
	// Idempotent: a second grant re-stamps the reason.
	if _, _, err := run(t, ctx, "--config", cfg, "admin", "management", "grant", key, "--reason", "renewed"); err != nil {
		t.Fatalf("re-grant: %v", err)
	}
	list, _, err := run(t, ctx, "--config", cfg, "admin", "management", "list", "--json")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(list, key) || !strings.Contains(list, "renewed") || strings.Count(list, `"device_key"`) != 1 {
		t.Errorf("list after re-grant = %s, want one grant with the new reason", list)
	}

	if _, _, err := run(t, ctx, "--config", cfg, "admin", "management", "revoke", key); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if authorised(t, cfg, key) {
		t.Error("a revoked device is still authorised")
	}
	if _, _, err := run(t, ctx, "--config", cfg, "admin", "management", "revoke", key); err == nil ||
		!strings.Contains(err.Error(), "no management grant") {
		t.Errorf("revoking twice = %v, want a no-grant refusal", err)
	}
	if out, _, err := run(t, ctx, "--config", cfg, "admin", "management", "list", "--json"); err != nil || strings.TrimSpace(out) != "[]" {
		t.Errorf("empty list = %q, %v; want []", out, err)
	}
}

func TestAdminManagementRefusals(t *testing.T) {
	cfg := tokenConfig(t)
	ctx := context.Background()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"not a key", []string{"grant", "laptop", "--reason", "x"}, "not a device key"},
		{"x25519 is not a device key", []string{"grant", rekeyOldRecovery, "--reason", "x"}, "not a device key"},
		{"no reason", []string{"grant", fixedUserKey(3)}, "--reason"},
		{"blank reason", []string{"grant", fixedUserKey(3), "--reason", "  "}, "--reason"},
		{"revoke not a key", []string{"revoke", "ed25519:zz"}, "not a device key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--config", cfg, "admin", "management"}, tc.args...)
			if _, _, err := run(t, ctx, args...); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("admin management %v = %v, want a refusal containing %q", tc.args, err, tc.want)
			}
		})
	}
	if authorised(t, cfg, fixedUserKey(3)) {
		t.Error("a refused grant authorised the device anyway")
	}
}

func TestAdminManagementJSONShapes(t *testing.T) {
	cfg := tokenConfig(t)
	ctx := context.Background()
	out, _, err := run(t, ctx, "--config", cfg, "admin", "management", "grant", fixedUserKey(3), "--reason", "owner laptop", "--json")
	if err != nil {
		t.Fatalf("grant --json: %v", err)
	}
	testutil.Golden(t, "testdata/admin_management_grant.json", []byte(normalise(out)))

	out, _, err = run(t, ctx, "--config", cfg, "admin", "management", "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}
	testutil.Golden(t, "testdata/admin_management_list.json", []byte(normalise(out)))
}
