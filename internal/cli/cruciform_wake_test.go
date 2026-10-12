package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/device"

	"github.com/rarebit-one/heyarr-core/internal/api/weblogin"
	"github.com/rarebit-one/heyarr-core/internal/config"
)

// The wake client POSTs the device cert, a fresh possession proof, ops and the relay session to the node's
// /v1/unwrap-wake, exactly the fields the server verifies and forwards.
func TestCruciformWakePostsToTheNode(t *testing.T) {
	var got struct {
		Cert       string   `json:"cert"`
		Possession string   `json:"possession"`
		Ops        []string `json:"ops"`
		RelayBase  string   `json:"relay_base"`
		Session    string   `json:"session"`
	}
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"woken":1}`))
	}))
	t.Cleanup(srv.Close)

	prove := func(time.Time) (string, error) { return "proof-xyz", nil }
	wake := newCruciformWake(srv.URL, srv.Client(), "cert-abc", []string{"op-1"}, prove)
	if err := wake(context.Background(), "https://relay.example/pair", "sess-9"); err != nil {
		t.Fatalf("wake: %v", err)
	}
	if gotPath != weblogin.UnwrapWakePrefix {
		t.Fatalf("posted to %q, want %q", gotPath, weblogin.UnwrapWakePrefix)
	}
	if got.Cert != "cert-abc" || got.Possession != "proof-xyz" || got.RelayBase != "https://relay.example/pair" || got.Session != "sess-9" {
		t.Fatalf("wake body = %+v", got)
	}
	if len(got.Ops) != 1 || got.Ops[0] != "op-1" {
		t.Fatalf("wake ops = %v, want [op-1]", got.Ops)
	}
}

// A non-2xx from the node is surfaced as an error, so the transport can fall back
// (or the caller can report) rather than silently believing the phone was woken.
func TestCruciformWakeSurfacesNodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not authorised", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	prove := func(time.Time) (string, error) { return "proof", nil }
	wake := newCruciformWake(srv.URL, srv.Client(), "cert", nil, prove)
	if err := wake(context.Background(), "r", "s"); err == nil {
		t.Fatal("a node refusal should surface as an error")
	}
}

// warnWakeFailedOn returns a func(error) that writes the wake-failed warning to
// the supplied writer, not to os.Stderr, so a caller that builds NewRootCommand
// with Options.Stderr captures it (#740).
func TestWarnWakeFailedOnWritesToWriter(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "network error",
			err:  errors.New("dial tcp: connection refused"),
			want: "heyarr: dial tcp: connection refused; still waiting for the phone on the relay (open Cruciform to approve)\n",
		},
		{
			name: "timeout",
			err:  errors.New("context deadline exceeded"),
			want: "heyarr: context deadline exceeded; still waiting for the phone on the relay (open Cruciform to approve)\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			fn := warnWakeFailedOn(&buf)
			fn(tc.err)
			got := buf.String()
			if got != tc.want {
				t.Fatalf("warnWakeFailedOn wrote %q, want %q", got, tc.want)
			}
			if !strings.Contains(got, tc.err.Error()) {
				t.Fatalf("output %q does not contain the error %q", got, tc.err.Error())
			}
		})
	}
}

// An un-enrolled device (a generated key but no enrolment cert) yields a nil
// WakeFunc, not an error: the offload still opens when the phone is already
// reachable, and the away-path wake becomes available once the device is enrolled.
func TestBuildCruciformWakeNilWhenUnenrolled(t *testing.T) {
	dir := t.TempDir()
	ds, err := device.NewStore(device.StoreOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ds.Generate("", false); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.HTTP.Addr = "127.0.0.1:8080" // a reachable node address, so nodeWakeClient succeeds
	cfg.HTTP.UnixSocket = ""

	wake, err := buildCruciformWake(cfg, dir)
	if err != nil {
		t.Fatalf("buildCruciformWake: %v", err)
	}
	if wake != nil {
		t.Fatal("an un-enrolled device should yield a nil WakeFunc")
	}
}

// TestWarnWakeFailedOnSerialisesConcurrentWarnings: the gateway's concurrent
// unwraps can fail their wakes together and share one callback; under -race an
// unsynchronised bytes.Buffer would be reported, and every line must arrive whole.
func TestWarnWakeFailedOnSerialisesConcurrentWarnings(t *testing.T) {
	var buf bytes.Buffer
	warn := warnWakeFailedOn(&buf)
	const n = 32
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			warn(errors.New("cruciform wake: node refused (503)"))
		}()
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != n {
		t.Fatalf("got %d lines, want %d", len(lines), n)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "heyarr: cruciform wake: node refused (503); still waiting") {
			t.Fatalf("an interleaved or partial line: %q", l)
		}
	}
}
