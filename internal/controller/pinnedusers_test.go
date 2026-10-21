package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"log/slog"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// #802: a controller that authenticates but has pinned no user refuses every
// device as device_unknown_user, and the caller is told nothing more than a
// 401. The operator is told once, at startup, at Warn.
func TestWarnIfNoUsersPinned(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		authEnabled bool
		pinned      int
		wantWarning bool
	}{
		{"auth on, no users pinned", true, 0, true},
		{"auth on, a user pinned", true, 1, false},
		// With authentication off nothing is refused, so there is nothing to warn of.
		{"auth off, no users pinned", false, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db := testdb.Migrated(t)
			eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
			if err != nil {
				t.Fatal(err)
			}
			store, err := deviceauth.New(deviceauth.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
			if err != nil {
				t.Fatal(err)
			}
			for range tc.pinned {
				pub, _, err := ed25519.GenerateKey(nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.EnrolUser(ctx, identity.FormatPublicKey(pub), "owner", ""); err != nil {
					t.Fatal(err)
				}
			}

			var logs bytes.Buffer
			warnIfNoUsersPinned(ctx, slog.New(slog.NewTextHandler(&logs, nil)), tc.authEnabled, store)

			got := strings.Count(logs.String(), noUsersPinnedWarning)
			if tc.wantWarning && (got != 1 || !strings.Contains(logs.String(), "level=WARN") ||
				!strings.Contains(logs.String(), "device_unknown_user")) {
				t.Errorf("want one Warn naming device_unknown_user, got:\n%s", logs.String())
			}
			if !tc.wantWarning && logs.Len() != 0 {
				t.Errorf("want no log line, got:\n%s", logs.String())
			}
		})
	}
}

// The unit test proves the decision; this proves the real startup path makes
// it. A fresh controller has pinned no one and authenticates by default, so it
// must say so before it reports itself started.
func TestAFreshControllerWarnsThatNoUsersArePinned(t *testing.T) {
	cfg := testConfig(t)
	cfg.HTTP.UnixSocket = "" // t.TempDir() overruns sun_path on macOS
	if !cfg.HTTP.Auth.Enabled {
		t.Fatal("the test config does not authenticate; the warning has nothing to warn of")
	}
	_, logs, stop := startedController(t, cfg)
	defer stop()

	line := waitForLogLine(t, logs, noUsersPinnedWarning)
	if line["level"] != "WARN" || line["refusal"] != "device_unknown_user" {
		t.Errorf("startup warning = %v, want level WARN naming device_unknown_user", line)
	}
	if n := strings.Count(logs.String(), noUsersPinnedWarning); n != 1 {
		t.Errorf("the warning was logged %d times, want once:\n%s", n, logs.String())
	}
}
