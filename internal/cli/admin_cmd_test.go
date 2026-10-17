package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

const (
	rekeyOldRecovery = "x25519:1111111111111111111111111111111111111111111111111111111111111111"
	rekeyNewRecovery = "x25519:2222222222222222222222222222222222222222222222222222222222222222"
)

// fixedUserKey is a deterministic rendered Ed25519 key, so the golden file can
// pin the keys themselves rather than a placeholder.
func fixedUserKey(b byte) string {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{b}, ed25519.SeedSize))
	return identity.FormatPublicKey(priv.Public().(ed25519.PublicKey))
}

// pinUser pins a user straight into the config's database, as POST
// /identities/users would.
func pinUser(t *testing.T, cfg, key, name, recovery string) deviceauth.User {
	t.Helper()
	var u deviceauth.User
	err := withDeviceAuth(context.Background(), cfg, func(ctx context.Context, s *deviceauth.Store) error {
		var err error
		u, err = s.EnrolUser(ctx, key, name, recovery)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestAdminUserRekeyJSONShape(t *testing.T) {
	cfg := tokenConfig(t)
	pinUser(t, cfg, fixedUserKey(1), "owner", rekeyOldRecovery)

	out, _, err := run(t, context.Background(), "--config", cfg,
		"admin", "user", "rekey", "owner", fixedUserKey(2), "--recovery-key", rekeyNewRecovery, "--json")
	if err != nil {
		t.Fatalf("admin user rekey: %v", err)
	}
	testutil.Golden(t, "testdata/admin_user_rekey.json", []byte(normalise(out)))
}

func TestAdminUserRekeyHumanOutputAndRefusals(t *testing.T) {
	cfg := tokenConfig(t)
	ctx := context.Background()
	u := pinUser(t, cfg, fixedUserKey(1), "owner", "")

	if _, _, err := run(t, ctx, "--config", cfg,
		"admin", "user", "rekey", "owner", fixedUserKey(2)); err == nil ||
		!strings.Contains(err.Error(), "--recovery-key") {
		t.Errorf("a rekey without --recovery-key = %v, want a refusal naming the flag", err)
	}
	if _, _, err := run(t, ctx, "--config", cfg,
		"admin", "user", "rekey", "owner", fixedUserKey(2), "--recovery-key", " "); err == nil ||
		!strings.Contains(err.Error(), "--recovery-key") {
		t.Errorf("a rekey with a whitespace --recovery-key = %v, want a refusal naming the flag", err)
	}
	if _, _, err := run(t, ctx, "--config", cfg,
		"admin", "user", "rekey", "nobody", fixedUserKey(2), "--recovery-key", rekeyNewRecovery); err == nil {
		t.Error("an unknown principal was rekeyed")
	}

	out, _, err := run(t, ctx, "--config", cfg,
		"admin", "user", "rekey", u.PrincipalID, fixedUserKey(2), "--recovery-key", rekeyNewRecovery)
	if err != nil {
		t.Fatalf("admin user rekey: %v", err)
	}
	for _, want := range []string{fixedUserKey(1), fixedUserKey(2), "(none)", rekeyNewRecovery, "ops deleted   0", "--no-rotate"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	// Rekeying to the key it now holds is refused, not silently repeated.
	if _, _, err := run(t, ctx, "--config", cfg,
		"admin", "user", "rekey", "owner", fixedUserKey(2), "--recovery-key", rekeyNewRecovery); err == nil {
		t.Error("a same-key rekey was accepted")
	}
}

func TestAdminUserPinJSONShape(t *testing.T) {
	cfg := tokenConfig(t)
	out, _, err := run(t, context.Background(), "--config", cfg,
		"admin", "user", "pin", "owner", fixedUserKey(1), "--recovery-key", rekeyOldRecovery, "--json")
	if err != nil {
		t.Fatalf("admin user pin: %v", err)
	}
	testutil.Golden(t, "testdata/admin_user_pin.json", []byte(normalise(out)))
}

func TestAdminUserPinHumanOutputAndRefusals(t *testing.T) {
	cfg := tokenConfig(t)
	ctx := context.Background()

	refusals := []struct {
		name string
		args []string
		want string
	}{
		{"no recovery key", []string{"owner", fixedUserKey(1)}, "--recovery-key"},
		// The store reads a blank recovery key as "none" (a pre-recovery-wrap
		// identity); the command must not let whitespace smuggle that through.
		{"whitespace recovery key", []string{"owner", fixedUserKey(1), "--recovery-key", "  "}, "--recovery-key"},
		{"whitespace name", []string{" ", fixedUserKey(1), "--recovery-key", rekeyOldRecovery}, "<name>"},
		{"empty name", []string{"", fixedUserKey(1), "--recovery-key", rekeyOldRecovery}, "<name>"},
		{"malformed signing key", []string{"owner", "ed25519:zz", "--recovery-key", rekeyOldRecovery}, "malformed"},
		{"recovery key not x25519", []string{"owner", fixedUserKey(1), "--recovery-key", fixedUserKey(2)}, "malformed"},
		{"recovery key upper-case hex", []string{"owner", fixedUserKey(1), "--recovery-key", strings.ToUpper(rekeyOldRecovery[7:])}, "malformed"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"--config", cfg, "admin", "user", "pin"}, tc.args...)
			if _, _, err := run(t, ctx, args...); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("admin user pin %v = %v, want a refusal containing %q", tc.args, err, tc.want)
			}
		})
	}

	out, _, err := run(t, ctx, "--config", cfg,
		"admin", "user", "pin", "owner", fixedUserKey(1), "--recovery-key", rekeyOldRecovery)
	if err != nil {
		t.Fatalf("admin user pin: %v", err)
	}
	for _, want := range []string{"pinned user owner", fixedUserKey(1), rekeyOldRecovery, "every controller"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	// The pin is the store's: a rekey can find it by name, which is how a
	// pinned-then-rotated identity is administered afterwards.
	if _, _, err := run(t, ctx, "--config", cfg,
		"admin", "user", "rekey", "owner", fixedUserKey(2), "--recovery-key", rekeyNewRecovery); err != nil {
		t.Errorf("rekeying the pinned user: %v", err)
	}

	// Pinning a key that is already pinned is refused, not silently repeated.
	if _, _, err := run(t, ctx, "--config", cfg,
		"admin", "user", "pin", "again", fixedUserKey(2), "--recovery-key", rekeyNewRecovery); err == nil ||
		!strings.Contains(err.Error(), "already enrolled") {
		t.Errorf("re-pinning a pinned key = %v, want an already-enrolled refusal", err)
	}
}
