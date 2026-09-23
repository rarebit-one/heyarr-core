package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rarebit-one/voidbind-go/device"

	"github.com/rarebit-one/heyarr-core/internal/api/weblogin"
	"github.com/rarebit-one/heyarr-core/internal/config"
)

// The wake client POSTs the device cert, ops and the relay session to the node's
// /v1/unwrap-wake, exactly the fields the server verifies and forwards.
func TestCruciformWakePostsToTheNode(t *testing.T) {
	var got struct {
		Cert      string   `json:"cert"`
		Ops       []string `json:"ops"`
		RelayBase string   `json:"relay_base"`
		Session   string   `json:"session"`
	}
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"woken":1}`))
	}))
	t.Cleanup(srv.Close)

	wake := newCruciformWake(srv.URL, srv.Client(), "cert-abc", []string{"op-1"})
	if err := wake(context.Background(), "https://relay.example/pair", "sess-9"); err != nil {
		t.Fatalf("wake: %v", err)
	}
	if gotPath != weblogin.UnwrapWakePrefix {
		t.Fatalf("posted to %q, want %q", gotPath, weblogin.UnwrapWakePrefix)
	}
	if got.Cert != "cert-abc" || got.RelayBase != "https://relay.example/pair" || got.Session != "sess-9" {
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

	wake := newCruciformWake(srv.URL, srv.Client(), "cert", nil)
	if err := wake(context.Background(), "r", "s"); err == nil {
		t.Fatal("a node refusal should surface as an error")
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
