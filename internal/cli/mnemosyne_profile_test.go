package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
)

// The mnemosyne binary shares its subcommand tree with heyarr. Before the
// loader was carried on the command context, `mnemosyne token create` loaded
// heyarr's profile defaults and opened <data_dir>/heyarr.db beside the server's
// mnemosyne.db — every token it minted was unknown to the server. The shared
// commands must read and write the same database the mnemosyne server opens.
func TestMnemosyneSharedCommandsUseMnemosyneProfile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mnemosyne.yaml")
	if err := os.WriteFile(cfgPath, []byte("data_dir: "+dir+"\npeer:\n  name: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mn := func(args ...string) string {
		t.Helper()
		var out, errb bytes.Buffer
		cmd := NewMnemosyneRootCommand(Options{Stdout: &out, Stderr: &errb, ShutdownGrace: 2 * time.Second})
		cmd.SetArgs(append([]string{"--config", cfgPath}, args...))
		if err := cmd.ExecuteContext(context.Background()); err != nil {
			t.Fatalf("mnemosyne %s: %v\nstderr: %s", strings.Join(args, " "), err, errb.String())
		}
		return out.String()
	}

	if out := mn("token", "create", "laptop", "--scopes", "write", "--json"); !strings.Contains(out, `"token"`) {
		t.Fatalf("expected a minted token, got %s", out)
	}
	if list := mn("token", "list", "--json"); !strings.Contains(list, "laptop") {
		t.Fatalf("token did not land in the database the list reads: %s", list)
	}

	// The personal profile mounts no identities API, so `admin user pin` is how
	// its first user is pinned; the pin must land where the server reads it.
	if out := mn("admin", "user", "pin", "owner", fixedUserKey(1), "--recovery-key", rekeyOldRecovery, "--json"); !strings.Contains(out, fixedUserKey(1)) {
		t.Fatalf("expected the pinned key, got %s", out)
	}
	if err := withDeviceAuth(config.WithLoader(context.Background(), config.LoadMnemosyne), cfgPath,
		func(ctx context.Context, s *deviceauth.Store) error {
			_, err := s.EnrolUser(ctx, fixedUserKey(1), "dup", rekeyOldRecovery)
			if !errors.Is(err, deviceauth.ErrUserExists) {
				t.Errorf("the pin did not land in mnemosyne.db: re-enrolling it = %v, want ErrUserExists", err)
			}
			return nil
		}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "mnemosyne.db")); err != nil {
		t.Errorf("mnemosyne.db was not created in data_dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "heyarr.db")); !os.IsNotExist(err) {
		t.Errorf("heyarr.db must not exist under the mnemosyne profile (stat err=%v)", err)
	}
}
