package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.Database.Path = filepath.Join(dir, "heyarr.db")
	cfg.CAS.Root = filepath.Join(dir, "cas")
	// Port ZERO, and this line is why the suite is hermetic.
	//
	// config.Defaults() carries the real listen address, 127.0.0.1:7777. A
	// test that overrode only the data directory therefore contended for the
	// port a RUNNING heyarr holds, so `go test ./...` on a machine that is
	// also serving would fail — reproducibly, at low load, on both a branch
	// and its merge base (#220). CI never saw it because hosted runners are
	// clean, which is the worst way for a test to be wrong.
	cfg.HTTP.Addr = "127.0.0.1:0"
	return cfg
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Regression from CI. Run receives the SHUTDOWN context, and it was being used
// for schema migration too — so a SIGTERM arriving while the migration was in
// flight cancelled it mid-statement and the role reported a startup failure.
// It passed on fast runners and failed on a slow macOS one, which is the worst
// way to find out.
//
// Transactional DDL means the interruption was safe rather than corrupting, but
// an ordinary restart should not surface as an error, and a service you are
// afraid to restart mid-upgrade is worse than one that takes a moment to stop.
func TestRunCompletesSchemaWorkEvenIfShutdownIsRequestedFirst(t *testing.T) {
	cfg := testConfig(t)

	// Already cancelled before Run is even entered — the most hostile version
	// of the race that broke CI.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := New(cfg, discard()).Run(ctx); err != nil {
		t.Fatalf("Run reported an error for what is an ordinary shutdown: %v", err)
	}

	// The schema work must still have happened, not been skipped.
	db, err := sqlite.Open(context.Background(), sqlite.Options{Path: cfg.Database.Path})
	if err != nil {
		t.Fatalf("the database was never created: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	version, err := sqlite.SchemaVersion(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if version == 0 {
		t.Error("shutdown during startup left the database unmigrated")
	}
}

func TestRunReportsAnUnusableDatabase(t *testing.T) {
	cfg := testConfig(t)
	// A directory where the database file should be: openable, unusable.
	cfg.Database.Path = t.TempDir()

	err := New(cfg, discard()).Run(context.Background())
	if err == nil {
		t.Fatal("Run succeeded against an unusable database path")
	}
	if !strings.Contains(err.Error(), "controller:") {
		t.Errorf("error = %q, want it to name the role that failed", err)
	}
}

func TestRunStopsWhenCancelled(t *testing.T) {
	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- New(cfg, discard()).Run(ctx) }()

	// Let it get past startup, then ask it to stop.
	waitForMigratedDatabase(t, cfg.Database.Path)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v on clean shutdown, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of cancellation")
	}
}

func waitForMigratedDatabase(t *testing.T, path string) {
	t.Helper()
	// Poll the file rather than opening the database. Opening in a tight loop
	// makes every attempt queue on busy_timeout, which starves the very
	// migration this is waiting for — the wait causes the thing it detects.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			time.Sleep(50 * time.Millisecond) // let the migration commit
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the controller never created its database")
}

// syncBuffer lets the test read the log while the controller is still writing
// to it, so it can wait for a readiness line rather than guessing a duration.
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

// "controller started" is a promise the acceptance script, the supervisor and
// an operator tailing the log all rely on: the API is reachable now. A start
// line printed before the socket exists is a lie that costs someone an
// afternoon.
func TestTheAPIIsListeningBeforeTheControllerReportsItStarted(t *testing.T) {
	cfg := testConfig(t)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = "" // t.TempDir() overruns sun_path on macOS

	logs := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- New(cfg, log).Run(ctx) }()

	line := waitForLogLine(t, logs, "controller started")

	// The acceptance script greps for this field; losing it would break the
	// milestone gate rather than a unit test.
	if _, ok := line["schema_version"]; !ok {
		t.Errorf("the start line carries no schema_version: %v", line)
	}
	addr, _ := line["http_addr"].(string)
	if addr == "" {
		t.Fatalf("the start line carries no bound address: %v", line)
	}

	// No polling, no retry: if the promise holds, this works on the first try.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("the API was not reachable the moment the controller said it had started: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v on clean shutdown", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	// And the listener must be gone afterwards, not leaked into the next test
	// or the next start.
	if conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Error("the HTTP listener survived the controller's shutdown")
	}
}

// waitForLogLine polls the log for a message rather than sleeping, and returns
// the decoded record.
func waitForLogLine(t *testing.T, logs *syncBuffer, msg string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, raw := range strings.Split(logs.String(), "\n") {
			if raw == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(raw), &rec); err != nil {
				continue
			}
			if rec["msg"] == msg {
				return rec
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("never saw %q in the log:\n%s", msg, logs.String())
	return nil
}

// ADR-0011 again, one layer up: the role must not start the listener either.
func TestTheControllerRefusesAnUnauthenticatedPublicBind(t *testing.T) {
	cfg := testConfig(t)
	cfg.HTTP.Addr = "0.0.0.0:0"
	cfg.HTTP.Auth.Enabled = false
	cfg.HTTP.UnixSocket = ""

	err := New(cfg, discard()).Run(context.Background())
	if err == nil {
		t.Fatal("the controller served an unauthenticated public bind")
	}
	if !strings.Contains(err.Error(), "refusing to start") {
		t.Errorf("error = %v, want it to name the refusal", err)
	}
}
