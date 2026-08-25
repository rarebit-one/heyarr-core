package cli

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// recoverConfig writes a minimal config with a data dir, and returns the config
// path and the data dir.
func recoverConfig(t *testing.T) (configPath, dataDir string) {
	t.Helper()
	return recoverConfigIn(t, t.TempDir())
}

// recoverConfigIn writes the config into the given directory — used with a SHORT
// directory for tests that bind a unix socket, whose path length is capped on
// macOS well below what t.TempDir() produces.
func recoverConfigIn(t *testing.T, dir string) (configPath, dataDir string) {
	t.Helper()
	path := filepath.Join(dir, "heyarr.yaml")
	body := "data_dir: " + dir + "\npeer:\n  name: test\n  site: test-site\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, dir
}

// shortDir makes a short-pathed temp directory (for bindable unix sockets) and
// cleans it up.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestRecoverRequiresItsFlags(t *testing.T) {
	t.Parallel()
	configPath, _ := recoverConfig(t)
	_, _, err := run(t, context.Background(), "--config", configPath, "recover")
	if err == nil {
		t.Fatal("recover with no flags succeeded")
	}
	if !strings.Contains(err.Error(), "required") {
		t.Errorf("error = %v, want it to name the required flags", err)
	}
}

// TestRecoverRefusesAMismatchedConfirmation proves the exact-match guard: a
// --confirm that is not the data directory is refused before anything is fetched
// or touched.
func TestRecoverRefusesAMismatchedConfirmation(t *testing.T) {
	t.Parallel()
	configPath, dataDir := recoverConfig(t)
	// A key file so the flag-presence check passes; the confirmation is checked
	// before the key is even read, so its contents do not matter here.
	keyFile := filepath.Join(t.TempDir(), "peer.key")
	if err := os.WriteFile(keyFile, []byte("ed25519-seed:"+strings.Repeat("00", 32)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := run(t, context.Background(), "--config", configPath, "recover",
		"--from-endpoint", "https://peer.invalid:7443",
		"--from-key", strings.Repeat("aa", 32),
		"--identity-key", keyFile,
		"--confirm", dataDir+"/not-it")
	if err == nil {
		t.Fatal("a mismatched --confirm was accepted")
	}
	if !strings.Contains(err.Error(), "does not match the data directory") {
		t.Errorf("error = %v, want a confirmation-mismatch refusal", err)
	}
}

// TestRecoverRefusesALiveDataDirectory proves the live-dir refusal is a
// MECHANISM, not a warning: a process listening on the data dir's socket makes
// recover refuse, before it fetches or touches anything.
func TestRecoverRefusesALiveDataDirectory(t *testing.T) {
	t.Parallel()
	configPath, dataDir := recoverConfigIn(t, shortDir(t))
	// Something is listening on the data dir's socket — a live controller.
	socket := filepath.Join(dataDir, "heyarr.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	keyFile := filepath.Join(t.TempDir(), "peer.key")
	if err := os.WriteFile(keyFile, []byte("ed25519-seed:"+strings.Repeat("00", 32)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = run(t, context.Background(), "--config", configPath, "recover",
		"--from-endpoint", "https://peer.invalid:7443",
		"--from-key", strings.Repeat("aa", 32),
		"--identity-key", keyFile,
		"--confirm", dataDir)
	if err == nil {
		t.Fatal("recover ran over a live data directory")
	}
	if !strings.Contains(err.Error(), "is live") {
		t.Errorf("error = %v, want a live-data-directory refusal", err)
	}
}

// TestDataDirIsLiveDistinguishesLiveFromStale proves the dial-based check
// returns true only for a socket something is accepting on, not a stale file.
func TestDataDirIsLiveDistinguishesLiveFromStale(t *testing.T) {
	t.Parallel()
	dir := shortDir(t)
	socket := filepath.Join(dir, "heyarr.sock")

	// No socket at all: not live.
	if live, _ := dataDirIsLive(socket); live {
		t.Error("a nonexistent socket reported live")
	}

	// A stale socket file (nothing listening): not live.
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if live, _ := dataDirIsLive(socket); live {
		t.Error("a stale socket file reported live")
	}
	_ = os.Remove(socket)

	// A real listener: live.
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	if live, _ := dataDirIsLive(socket); !live {
		t.Error("a live listener was not detected")
	}
}
