package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// M4-03, ADR-0010, ADR-0012.
//
// These tests drive the REAL startup path — controller.Run against a real
// database and a real CAS root — because the property under test is "the
// process refuses to start". A unit test of the comparison function can prove
// the comparison is right and prove nothing about whether anything calls it
// before a listener is bound, and that second half is the whole issue.

// startedController runs a controller until it logs "controller started" and
// returns the decoded start line plus a stop function.
func startedController(t *testing.T, cfg config.Config) (map[string]any, *syncBuffer, func()) {
	t.Helper()
	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- New(cfg, slog.New(slog.NewJSONHandler(logs, nil))).Run(ctx) }()

	line := waitForLogLine(t, logs, "controller started")
	stop := func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v on clean shutdown", err)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("Run did not return after cancellation")
		}
	}
	return line, logs, stop
}

// runAndExpectRefusal runs a controller that must fail, and asserts it never
// reported itself started. A refusal that logs and carries on is the bug M4-03
// exists to close, so "it returned an error" is not enough on its own.
func runAndExpectRefusal(t *testing.T, cfg config.Config) (*syncBuffer, error) {
	t.Helper()
	logs := &syncBuffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- New(cfg, slog.New(slog.NewJSONHandler(logs, nil))).Run(ctx) }()

	// Watch for BOTH outcomes rather than only waiting for the error. A node
	// that starts happily is the failure this asserts against, and waiting for
	// an error that is never coming reports it as a timeout — which reads like
	// a slow machine rather than like a missing refusal. Confirmed by
	// sabotage: with the comparison removed, the first version of this helper
	// failed with "neither started nor refused within 30s", which is the right
	// verdict for the wrong reason.
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("the controller ran to completion with a contested identity:\n%s", logs.String())
			}
			if strings.Contains(logs.String(), "controller started") {
				t.Fatalf("the controller reported itself started and THEN failed — it served under a "+
					"contested identity, which is the failure this test exists to prevent:\n%s", logs.String())
			}
			return logs, err
		default:
		}
		if strings.Contains(logs.String(), "controller started") {
			t.Fatalf("the controller STARTED with a contested identity instead of refusing — "+
				"the ADR-0010 comparison is not being made, or not before the listener is bound:\n%s",
				logs.String())
		}
		if time.Now().After(deadline) {
			t.Fatal("the controller neither started nor refused within 30s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// seedOnDisk is the PRIVATE key material, read the way an attacker with the
// data directory would read it. The tests scan output for exactly these bytes.
func seedOnDisk(t *testing.T, dataDir string) string {
	t.Helper()
	raw, err := os.ReadFile(identity.KeyPath(dataDir))
	if err != nil {
		t.Fatalf("reading the private key: %v", err)
	}
	text := strings.TrimSpace(string(raw))
	_, hexSeed, ok := strings.Cut(text, ":")
	if !ok || len(hexSeed) != ed25519.SeedSize*2 {
		t.Fatalf("the key file is not the expected shape: %q", text)
	}
	return hexSeed
}

func markerPeerID(t *testing.T, casRoot string) string {
	t.Helper()
	store, err := cas.OpenFS(casRoot)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.MarkerPeerID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func setMarkerPeerID(t *testing.T, casRoot, peerID string) {
	t.Helper()
	store, err := cas.OpenFS(casRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindPeer(peerID); err != nil {
		t.Fatal(err)
	}
	// Assert the write applied. A test that corrupts a file and does not check
	// the corruption landed is a test that passes when its own setup silently
	// failed.
	if got := markerPeerID(t, casRoot); got != peerID {
		t.Fatalf("the marker still says %s after being set to %s", got, peerID)
	}
}

// A fresh start generates a keypair, persists it in both places, and a second
// start reuses it — byte-identically, not merely "startup succeeded".
func TestThePeerIdentityIsStableAcrossRestarts(t *testing.T) {
	cfg := testConfig(t)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = ""
	// Loopback with authentication off, so the scan below reads the same bytes
	// a client would without a token muddying what is being asserted.
	cfg.HTTP.Auth.Enabled = false

	first, firstLogs, stop := startedController(t, cfg)
	firstKey, _ := first["peer_public_key"].(string)
	firstPeer, _ := first["peer_id"].(string)
	stop()

	if firstPeer == "" {
		t.Fatal("the start line carries no peer_id")
	}
	if !strings.HasPrefix(firstKey, "ed25519:") || len(firstKey) != len("ed25519:")+ed25519.PublicKeySize*2 {
		t.Fatalf("peer_public_key = %q, want ed25519:<64 hex>", firstKey)
	}

	// Both places, populated.
	if got := markerPeerID(t, cfg.CAS.Root); got != firstPeer {
		t.Errorf("the CAS marker names peer %q, the database names %q", got, firstPeer)
	}

	// The private key: 0600, in the data directory, and NOT in the CAS root
	// (which is the thing that gets rebuilt and restored from elsewhere).
	info, err := os.Stat(identity.KeyPath(cfg.DataDir))
	if err != nil {
		t.Fatalf("no private key in the data directory: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the private key is %#o, want 0600", perm)
	}
	if _, err := os.Stat(filepath.Join(cfg.CAS.Root, identity.KeyFileName)); err == nil {
		t.Error("the private key is inside the CAS root, which travels between hosts")
	}

	seed := seedOnDisk(t, cfg.DataDir)

	// Restart. The identity must be reused, not regenerated.
	second, secondLogs, stop2 := startedController(t, cfg)
	defer stop2()
	secondKey, _ := second["peer_public_key"].(string)
	secondPeer, _ := second["peer_id"].(string)

	if secondKey != firstKey {
		t.Errorf("the public key changed across a restart:\n  first  %s\n  second %s", firstKey, secondKey)
	}
	if secondPeer != firstPeer {
		t.Errorf("the peer id changed across a restart: %s then %s", firstPeer, secondPeer)
	}
	if seedOnDisk(t, cfg.DataDir) != seed {
		t.Error("the private key on disk was rewritten by the second start")
	}

	// The private key must not appear in any log line. Scanned, not reasoned
	// about: the assertion is over captured output, so a future change that
	// logs the key fails here even if nobody reads this test again.
	for name, logs := range map[string]string{"first start": firstLogs.String(), "second start": secondLogs.String()} {
		if strings.Contains(logs, seed) {
			t.Errorf("the private key appears in the %s log", name)
		}
	}

	// ...nor in the API. Fetch the real peers collection off the running
	// server and scan the raw bytes.
	addr, _ := second["http_addr"].(string)
	body := getBody(t, "http://"+addr+"/api/v1/peers")
	if strings.Contains(body, seed) {
		t.Error("the private key appears in GET /api/v1/peers")
	}
	if !strings.Contains(body, firstKey) {
		t.Errorf("GET /api/v1/peers does not carry the public key %s:\n%s", firstKey, body)
	}

	// And the field is where the CLI reads it from, with the value an operator
	// would copy to the other site.
	var page struct {
		Items []struct {
			ID        string  `json:"id"`
			IsSelf    bool    `json:"is_self"`
			PublicKey *string `json:"public_key"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decoding the peers page: %v", err)
	}
	var found bool
	for _, p := range page.Items {
		if !p.IsSelf {
			continue
		}
		found = true
		if p.PublicKey == nil {
			t.Fatal("the self peer's public_key is null after an identity was established")
		}
		if *p.PublicKey != firstKey {
			t.Errorf("public_key = %q, want %q", *p.PublicKey, firstKey)
		}
		if p.ID != firstPeer {
			t.Errorf("the self peer is %s, want %s", p.ID, firstPeer)
		}
	}
	if !found {
		t.Fatal("no self peer in GET /api/v1/peers")
	}
}

func getBody(t *testing.T, url string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", url, resp.StatusCode, raw)
	}
	return string(raw)
}

// The refusal ADR-0010 promised and nothing implemented until M4-03.
func TestTheControllerRefusesWhenTheCASMarkerNamesAnotherPeer(t *testing.T) {
	cfg := testConfig(t)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = ""

	first, _, stop := startedController(t, cfg)
	peerID, _ := first["peer_id"].(string)
	stop()

	// The mundane scenario: the CAS was rebuilt or restored from a machine
	// that was a different peer. Corrupt the marker, and assert the corruption
	// actually applied before believing anything that follows.
	const other = "01990000-0000-7000-8000-0000000000ff"
	if other == peerID {
		t.Fatal("the fixture peer id collided with the real one")
	}
	setMarkerPeerID(t, cfg.CAS.Root, other)

	logs, err := runAndExpectRefusal(t, cfg)
	if !errors.Is(err, identity.ErrIdentityConflict) {
		t.Fatalf("error = %v, want an identity conflict", err)
	}
	// The error must name BOTH identities: an operator who is told only that
	// something disagrees cannot tell which of the two machines to fix.
	if !strings.Contains(err.Error(), peerID) {
		t.Errorf("the refusal does not name the database's peer %s: %v", peerID, err)
	}
	if !strings.Contains(err.Error(), other) {
		t.Errorf("the refusal does not name the marker's peer %s: %v", other, err)
	}
	if !strings.Contains(err.Error(), filepath.Join(cfg.CAS.Root, cas.MarkerName)) {
		t.Errorf("the refusal does not say where the second identity is written: %v", err)
	}
	if strings.Contains(logs.String(), "peer_public_key") {
		t.Error("the controller logged its identity before refusing to use it")
	}

	// Restoring the marker lets it start again — and the restoration is
	// asserted, so a green run cannot come from a marker that never changed.
	setMarkerPeerID(t, cfg.CAS.Root, peerID)
	if got := markerPeerID(t, cfg.CAS.Root); got != peerID {
		t.Fatalf("the marker was not restored: %s", got)
	}
	second, _, stop2 := startedController(t, cfg)
	defer stop2()
	if got, _ := second["peer_id"].(string); got != peerID {
		t.Errorf("after restoring the marker the node started as %s, want %s", got, peerID)
	}
}

// A private key that does not match the stored public key is the same failure
// wearing different clothes: this node cannot prove it is the peer its own
// catalog says it is.
func TestTheControllerRefusesAPrivateKeyThatDoesNotMatchThePublicKey(t *testing.T) {
	cfg := testConfig(t)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = ""

	first, _, stop := startedController(t, cfg)
	recorded, _ := first["peer_public_key"].(string)
	stop()

	original := seedOnDisk(t, cfg.DataDir)

	// Swap in an unrelated key, at the same permissions, so the only thing
	// wrong is which keypair it is.
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	replacement := hex.EncodeToString(seed)
	if replacement == original {
		t.Fatal("the replacement key is the original one")
	}
	if err := os.WriteFile(identity.KeyPath(cfg.DataDir),
		[]byte("ed25519-seed:"+replacement+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if seedOnDisk(t, cfg.DataDir) != replacement {
		t.Fatal("the key swap did not apply")
	}

	_, err := runAndExpectRefusal(t, cfg)
	if !errors.Is(err, identity.ErrKeyMismatch) {
		t.Fatalf("error = %v, want a key mismatch", err)
	}
	if !strings.Contains(err.Error(), recorded) {
		t.Errorf("the refusal does not name the recorded public key %s: %v", recorded, err)
	}

	// Restore, and it starts again.
	if err := os.WriteFile(identity.KeyPath(cfg.DataDir),
		[]byte("ed25519-seed:"+original+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if seedOnDisk(t, cfg.DataDir) != original {
		t.Fatal("the key restore did not apply")
	}
	line, _, stop2 := startedController(t, cfg)
	defer stop2()
	if got, _ := line["peer_public_key"].(string); got != recorded {
		t.Errorf("after restoring the key the node started as %s, want %s", got, recorded)
	}
}

// A recorded public key with no private key anywhere. Generating a fresh one
// would keep the peer id and change the identity, which is precisely the state
// every peer that pinned the old key would reject — silently, at replication
// time, hours later.
func TestTheControllerRefusesAMissingPrivateKey(t *testing.T) {
	cfg := testConfig(t)
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = ""

	first, _, stop := startedController(t, cfg)
	recorded, _ := first["peer_public_key"].(string)
	stop()

	if err := os.Remove(identity.KeyPath(cfg.DataDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(identity.KeyPath(cfg.DataDir)); !os.IsNotExist(err) {
		t.Fatalf("the key was not removed: %v", err)
	}

	_, err := runAndExpectRefusal(t, cfg)
	if !errors.Is(err, identity.ErrKeyMissing) {
		t.Fatalf("error = %v, want a missing-key refusal", err)
	}
	if !strings.Contains(err.Error(), recorded) {
		t.Errorf("the refusal does not name the recorded public key: %v", err)
	}
}
