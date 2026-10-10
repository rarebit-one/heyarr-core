package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runMnemosyneCmd runs the mnemosyne command tree with the given args and
// returns stdout, stderr and the error. It is the mnemosyne analogue of run().
func runMnemosyneCmd(t *testing.T, ctx context.Context, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errb bytes.Buffer
	cmd := NewMnemosyneRootCommand(Options{Stdout: &out, Stderr: &errb, ShutdownGrace: 2 * time.Second})
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(ctx)
	return out.String(), errb.String(), err
}

// TestMnemosyneVersionHuman sanity-checks that the mnemosyne binary produces
// its own version prefix, not heyarr's.
func TestMnemosyneVersionHuman(t *testing.T) {
	out, _, err := runMnemosyneCmd(t, context.Background(), "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.HasPrefix(out, "mnemosyne ") {
		t.Errorf("version output = %q, want mnemosyne prefix", out)
	}
}

// TestMnemosyneRolesStartAndStopCleanly checks that each role subcommand starts
// its expected roles, logs the structured startup lines that contain build
// identity, and exits cleanly when its context is cancelled.
//
// The table covers:
//
//	serve — controller only (personal profile).
//	worker — PersonalWorker only (GC, convergence, blob transfer).
//	all — both roles in one process; the operator default used by the
//	       systemd unit and the homelab-ops Nix module.
func TestMnemosyneRolesStartAndStopCleanly(t *testing.T) {
	cases := []struct {
		role         string
		startLines   []string // must appear before cancel
		stopLines    []string // must appear after cancel
		startingLine string   // top-level startup log line
		stoppingLine string   // top-level stop log line
	}{
		{
			role:         "serve",
			startLines:   []string{"controller started"},
			stopLines:    []string{"controller stopped"},
			startingLine: "mnemosyne starting",
			stoppingLine: "mnemosyne stopped",
		},
		{
			// The worker subcommand logs its own top-level lines
			// ("mnemosyne worker starting" / "mnemosyne worker stopped")
			// rather than the shared "mnemosyne starting" / "mnemosyne stopped"
			// that serve and all emit. The serve/all path goes through
			// runMnemosyne/runMnemosyneAll; the worker path goes through the
			// separate runMnemosyneWorker, which mirrors the worker binary's own
			// log convention.
			role:         "worker",
			startLines:   []string{"mnemosyne worker started"},
			stopLines:    []string{"mnemosyne worker stopped"},
			startingLine: "mnemosyne worker starting",
			stoppingLine: "mnemosyne worker stopped",
		},
		{
			// "all" is the operator default: controller + worker in one process.
			// Both role-specific start and stop lines must appear, and the
			// top-level log pair is the shared "mnemosyne starting" / "mnemosyne
			// stopped" from runMnemosyneAll.
			role:         "all",
			startLines:   []string{"controller started", "mnemosyne worker started"},
			stopLines:    []string{"controller stopped", "mnemosyne worker stopped"},
			startingLine: "mnemosyne starting",
			stoppingLine: "mnemosyne stopped",
		},
	}

	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "mnemosyne.yaml")
			// Pin http.addr to port 0 to avoid contending with any running
			// mnemosyne or heyarr instance. peer.listen is empty, which
			// disables that listener entirely.
			body := "data_dir: " + filepath.Join(dir, "data") +
				"\nhttp:\n  addr: 127.0.0.1:0\npeer:\n  name: test\nlog:\n  format: json\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}

			var out bytes.Buffer
			errb := &syncBuffer{}
			ctx, cancel := context.WithCancel(context.Background())

			cmd := NewMnemosyneRootCommand(Options{Stdout: &out, Stderr: errb, ShutdownGrace: 2 * time.Second})
			cmd.SetArgs([]string{"--config", path, tc.role})

			done := make(chan error, 1)
			go func() { done <- cmd.ExecuteContext(ctx) }()

			for _, want := range tc.startLines {
				waitForLog(t, errb, want, done)
			}
			cancel()

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("%s exited with %v\n%s", tc.role, err, errb.String())
				}
				logs := errb.String()
				assertLogged(t, logs, tc.startingLine)
				assertLogged(t, logs, tc.stoppingLine)
				assertLogged(t, logs, `"version"`)
				assertLogged(t, logs, `"commit"`)
				for _, want := range tc.stopLines {
					assertLogged(t, logs, want)
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("%s did not exit within 30s of cancellation\n%s", tc.role, errb.String())
			}
		})
	}
}
