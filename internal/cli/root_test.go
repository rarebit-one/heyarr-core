package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func run(t *testing.T, ctx context.Context, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errb bytes.Buffer
	cmd := NewRootCommand(Options{Stdout: &out, Stderr: &errb, ShutdownGrace: 2 * time.Second})
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(ctx)
	return out.String(), errb.String(), err
}

func TestVersionJSON(t *testing.T) {
	out, _, err := run(t, context.Background(), "version", "--json")
	if err != nil {
		t.Fatalf("version --json: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	for _, key := range []string{"version", "commit", "date", "go_version"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing key %q in %v", key, got)
		}
	}
}

func TestVersionHuman(t *testing.T) {
	out, _, err := run(t, context.Background(), "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.HasPrefix(out, "heyarr ") {
		t.Errorf("version output = %q", out)
	}
}

func TestUnknownCommandIsAnError(t *testing.T) {
	if _, _, err := run(t, context.Background(), "nope"); err == nil {
		t.Error("unknown command succeeded")
	}
}

func TestConfigPrintResolvesLayers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "heyarr.yaml")
	if err := os.WriteFile(path, []byte("data_dir: /srv/heyarr\npeer:\n  name: bartley\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HEYARR_PEER__SITE", "bartley-ridge")

	out, _, err := run(t, context.Background(), "--config", path, "config", "print")
	if err != nil {
		t.Fatalf("config print: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("config print is not valid JSON: %v\n%s", err, out)
	}
	peer, _ := got["Peer"].(map[string]any)
	if peer["Name"] != "bartley" {
		t.Errorf("peer.name = %v, want the file value", peer["Name"])
	}
	if peer["Site"] != "bartley-ridge" {
		t.Errorf("peer.site = %v, want the environment value", peer["Site"])
	}
}

func TestConfigPrintSurfacesValidationErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "heyarr.yaml")
	body := "data_dir: /srv/heyarr\nhttp:\n  addr: \"0.0.0.0:7777\"\n  auth:\n    enabled: false\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := run(t, context.Background(), "--config", path, "config", "print")
	if err == nil {
		t.Fatal("config print accepted an unauthenticated public bind")
	}
	if !strings.Contains(err.Error(), "refusing to start") {
		t.Errorf("error = %q, want the ADR-0011 refusal", err)
	}
}

// The M1-02 acceptance criterion: `heyarr all` starts, logs a structured line
// carrying version and commit, and exits cleanly when its context is cancelled.
func TestRolesStartAndStopCleanly(t *testing.T) {
	for _, role := range []string{"controller", "worker", "peer", "all"} {
		t.Run(role, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "heyarr.yaml")
			body := "data_dir: " + filepath.Join(dir, "data") + "\npeer:\n  name: test\nlog:\n  format: json\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			type result struct {
				stderr string
				err    error
			}
			done := make(chan result, 1)
			go func() {
				_, errb, err := run(t, ctx, "--config", path, role)
				done <- result{errb, err}
			}()

			// Give the roles a moment to start, then ask them to stop.
			time.Sleep(150 * time.Millisecond)
			cancel()

			select {
			case got := <-done:
				if got.err != nil {
					t.Fatalf("%s exited with %v\n%s", role, got.err, got.stderr)
				}
				assertLogged(t, got.stderr, "heyarr starting")
				assertLogged(t, got.stderr, "heyarr stopped")
				// Every startup line must carry build identity, so a support
				// question never starts with "which build is that".
				assertLogged(t, got.stderr, `"version"`)
				assertLogged(t, got.stderr, `"commit"`)
				for _, want := range expectedRoleLogs(role) {
					assertLogged(t, got.stderr, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s did not exit within 5s of cancellation", role)
			}
		})
	}
}

func expectedRoleLogs(role string) []string {
	switch role {
	case "all":
		return []string{
			"controller started", "worker started", "peer started",
			"controller stopped", "worker stopped", "peer stopped",
		}
	default:
		return []string{role + " started", role + " stopped"}
	}
}

func assertLogged(t *testing.T, logs, want string) {
	t.Helper()
	if !strings.Contains(logs, want) {
		t.Errorf("logs do not contain %q\n--- logs ---\n%s", want, logs)
	}
}

// A role refusing to start on bad configuration must do so before anything
// listens or writes.
func TestRoleRefusesBadConfigBeforeStarting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "heyarr.yaml")
	if err := os.WriteFile(path, []byte("data_dir: relative/path\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := run(t, context.Background(), "--config", path, "all")
	if err == nil {
		t.Fatal("all started with an invalid data_dir")
	}
	if strings.Contains(stderr, "heyarr starting") {
		t.Error("the process logged a startup line before rejecting the configuration")
	}
}
