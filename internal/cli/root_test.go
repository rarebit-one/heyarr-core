package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer lets the test read stderr while the process is still writing it,
// so it can wait for a readiness line instead of guessing a duration.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

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

// The M1-02 acceptance criterion: each role starts, logs a structured line
// carrying version and commit, and exits cleanly when its context is cancelled.
//
// It waits for the role's own "started" line rather than sleeping a fixed
// duration. A fixed wait is a bet on machine speed, and it lost as soon as the
// core schema migration got bigger: on a slow CI runner the cancel arrived
// mid-migration, the controller correctly reported a clean stop during startup,
// and the test failed looking for a line that was never going to appear.
func TestRolesStartAndStopCleanly(t *testing.T) {
	for _, role := range []string{"controller", "worker", "peer", "all"} {
		t.Run(role, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "heyarr.yaml")
			body := "data_dir: " + filepath.Join(dir, "data") + "\npeer:\n  name: test\nlog:\n  format: json\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			errb := &syncBuffer{}
			ctx, cancel := context.WithCancel(context.Background())

			cmd := NewRootCommand(Options{Stdout: &out, Stderr: errb, ShutdownGrace: 2 * time.Second})
			cmd.SetArgs([]string{"--config", path, role})

			done := make(chan error, 1)
			go func() { done <- cmd.ExecuteContext(ctx) }()

			// Wait until every role this command runs has reported itself up.
			for _, want := range startupLines(role) {
				waitForLog(t, errb, want)
			}
			cancel()

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("%s exited with %v\n%s", role, err, errb.String())
				}
				logs := errb.String()
				assertLogged(t, logs, "heyarr starting")
				assertLogged(t, logs, "heyarr stopped")
				// Every startup line carries build identity, so a support
				// question never begins with "which build is that".
				assertLogged(t, logs, `"version"`)
				assertLogged(t, logs, `"commit"`)
				for _, want := range expectedRoleLogs(role) {
					assertLogged(t, logs, want)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("%s did not exit within 30s of cancellation\n%s", role, errb.String())
			}
		})
	}
}

// startupLines is what must appear before the role set is fully up.
func startupLines(role string) []string {
	if role == "all" {
		return []string{"controller started", "worker started", "peer started"}
	}
	return []string{role + " started"}
}

func waitForLog(t *testing.T, b *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(b.String(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("never saw %q in the logs\n--- logs ---\n%s", want, b.String())
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
