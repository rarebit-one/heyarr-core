// The Personal MCP's read verbs are tested against a REAL controller — a real
// database and the real encrypted personal-state API — and a REAL device decrypt
// path. Nothing on the invariant-critical path is mocked: each CRDT type is
// genuinely encrypted client-side, pushed as ciphertext, and decrypted on the
// device by the production personalStateReader. The decisive assertion is over
// the CIPHERTEXT AT REST — the exact bytes the controller stores for these
// spaces' changes — which must never contain the personal-state plaintext
// (Invariant 6, §72), mirroring the gateway's own boundary test.
package cli

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	psapi "github.com/rarebit-one/heyarr-core/internal/api/personalstate"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	apiclient "github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/device"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	psclient "github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync"
	psstore "github.com/rarebit-one/heyarr-core/internal/personalstate/store"
)

// psHarness is a running controller with the encrypted personal-state API, a
// real device, and an authenticated client — everything the read verbs need.
type psHarness struct {
	t         *testing.T
	ctx       context.Context
	client    *apiclient.Client
	mgr       *psclient.Manager
	deviceDir string
}

func newPSHarness(t *testing.T) *psHarness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(dir, "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	authStore, err := auth.NewStore(auth.StoreOptions{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := auth.NewVerifier(auth.VerifierOptions{Store: authStore})
	if err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	psStore, err := psstore.New(psstore.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		t.Fatal(err)
	}
	ps, err := psapi.New(psapi.Options{Store: psStore, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.HTTP.Auth.Enabled = true
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = ""

	srv, err := httpapi.New(httpapi.Options{
		Config: cfg, Logger: slog.New(slog.DiscardHandler), DB: db, Verifier: verifier, Events: eventLog,
		Build:              buildinfo.Info{Version: "test", Commit: "abc", Date: "2026-08-01T00:00:00Z"},
		SchemaVersion:      4,
		KnownSchemaVersion: 4,
		Mount:              []httpapi.MountFunc{ps.Mount},
	})
	if err != nil {
		t.Fatal(err)
	}
	controller := httptest.NewServer(srv.Handler())
	t.Cleanup(controller.Close)

	created, err := authStore.Create(ctx, "device", []auth.Scope{auth.ScopeRead, auth.ScopeWrite}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := apiclient.New(apiclient.Options{Addr: controller.URL, Token: created.Secret})
	if err != nil {
		t.Fatal(err)
	}

	deviceDir := filepath.Join(dir, "device")
	ds, err := device.NewStore(device.StoreOptions{Dir: deviceDir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ds.Generate("test-device", false); err != nil {
		t.Fatal(err)
	}
	if _, err := ds.LoadEncryptionKey(); err != nil {
		t.Fatal(err)
	}
	mgr := psclient.New()

	return &psHarness{t: t, ctx: ctx, client: c, mgr: mgr, deviceDir: deviceDir}
}

// createSpace mints a space wrapped for this device and registers it with the
// controller, returning its id. The manager is left open on the space.
func (h *psHarness) createSpace() string {
	h.t.Helper()
	priv, err := device.NewStore(device.StoreOptions{Dir: h.deviceDir})
	if err != nil {
		h.t.Fatal(err)
	}
	pk, err := priv.LoadEncryptionKey()
	if err != nil {
		h.t.Fatal(err)
	}
	recip, err := psclient.ParseRecipient(encryption.FormatPublicKey(pk.PublicKey().Bytes()))
	if err != nil {
		h.t.Fatal(err)
	}
	sp, wrapped, err := h.mgr.Create(spaces.KindPersonal, time.Now().UTC(), []psclient.Recipient{recip})
	if err != nil {
		h.t.Fatal(err)
	}
	req := apiclient.CreateSpaceRequest{ID: sp.ID, Kind: string(sp.Kind)}
	for _, w := range wrapped {
		req.WrappedKeys = append(req.WrappedKeys, apiclient.WrappedKeyInput{Recipient: w.Recipient, Wrapped: w.Wrapped})
	}
	if _, err := h.client.CreateSpace(h.ctx, req); err != nil {
		h.t.Fatal(err)
	}
	return sp.ID
}

// pushChanges encrypts a batch of CRDT changes under the space key and pushes
// each as ciphertext, chaining the causal parents — the ordinary device flow.
func pushChanges[T any](h *psHarness, spaceID string, changes []T) {
	h.t.Helper()
	var parents []string
	for _, ch := range changes {
		ec, err := statesync.EncodeChange(h.mgr, spaceID, parents, ch)
		if err != nil {
			h.t.Fatal(err)
		}
		id, err := h.client.PutChange(h.ctx, ec)
		if err != nil {
			h.t.Fatal(err)
		}
		parents = []string{id}
	}
}

// assertNoPlaintextAtRest fetches the controller's stored ciphertext for a space
// and asserts none of the secret plaintext strings appear in it — the §72 claim
// applied to each new CRDT type's own space.
func (h *psHarness) assertNoPlaintextAtRest(spaceID string, secrets ...string) {
	h.t.Helper()
	changes, err := h.client.Changes(h.ctx, spaceID)
	if err != nil {
		h.t.Fatal(err)
	}
	if len(changes) == 0 {
		h.t.Fatalf("space %s holds no changes at rest — the at-rest check would be vacuous", spaceID)
	}
	for _, ec := range changes {
		for _, secret := range secrets {
			if bytes.Contains(ec.Ciphertext, []byte(secret)) {
				h.t.Errorf("controller ciphertext for %s contains plaintext %q (Invariant 6, §72 VIOLATED)", spaceID, secret)
			}
		}
	}
}

func (h *psHarness) reader() personalStateReader {
	return personalStateReader{ctx: h.ctx, c: h.client, deviceDir: h.deviceDir}
}

// TestPersonalMCPReaderStarredIsDecryptedOnDevice: a genuinely-encrypted starred
// set is read back decrypted, in recency order, and the controller never held
// the plaintext item ids.
func TestPersonalMCPReaderStarredIsDecryptedOnDevice(t *testing.T) {
	h := newPSHarness(t)
	spaceID := h.createSpace()

	s := crdt.NewStarSet()
	var changes []crdt.StarChange
	for _, item := range []string{"tr:SECRET-alpha", "tr:SECRET-bravo"} {
		changes = append(changes, s.Star(item))
	}
	pushChanges(h, spaceID, changes)

	got, err := h.reader().Starred(spaceID)
	if err != nil {
		t.Fatal(err)
	}
	// Most-recently-starred first: bravo was starred after alpha.
	want := []string{"tr:SECRET-bravo", "tr:SECRET-alpha"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Starred = %v, want %v", got, want)
	}
	h.assertNoPlaintextAtRest(spaceID, "tr:SECRET-alpha", "tr:SECRET-bravo")
}

// TestPersonalMCPReaderHistoryIsDecryptedOnDevice: a genuinely-encrypted play
// history is read back decrypted into recent/frequent/now-playing, and the
// controller never held the plaintext item ids.
func TestPersonalMCPReaderHistoryIsDecryptedOnDevice(t *testing.T) {
	h := newPSHarness(t)
	spaceID := h.createSpace()

	log := crdt.NewPlayLog()
	var changes []crdt.PlayChange
	// alpha played twice, bravo once, bravo most recent.
	for _, item := range []string{"tr:SECRET-alpha", "tr:SECRET-alpha", "tr:SECRET-bravo"} {
		changes = append(changes, log.Record(item))
	}
	pushChanges(h, spaceID, changes)

	hist, err := h.reader().History(spaceID)
	if err != nil {
		t.Fatal(err)
	}
	if hist.NowPlaying != "tr:SECRET-bravo" {
		t.Errorf("NowPlaying = %q, want tr:SECRET-bravo", hist.NowPlaying)
	}
	if len(hist.Recent) == 0 || hist.Recent[0] != "tr:SECRET-bravo" {
		t.Errorf("Recent[0] = %v, want tr:SECRET-bravo first", hist.Recent)
	}
	if len(hist.Frequent) == 0 || hist.Frequent[0].ID != "tr:SECRET-alpha" || hist.Frequent[0].Count != 2 {
		t.Errorf("Frequent[0] = %+v, want tr:SECRET-alpha count 2", hist.Frequent)
	}
	h.assertNoPlaintextAtRest(spaceID, "tr:SECRET-alpha", "tr:SECRET-bravo")
}

// TestPersonalMCPReaderReadingPositionIsDecryptedOnDevice: a genuinely-encrypted
// reading position is read back decrypted, latest write wins, and the controller
// never held the plaintext locator or publication id.
func TestPersonalMCPReaderReadingPositionIsDecryptedOnDevice(t *testing.T) {
	h := newPSHarness(t)
	spaceID := h.createSpace()

	pos := crdt.NewReadingPositions()
	var changes []crdt.PositionChange
	changes = append(changes, pos.Set("pub:SECRET-dune", "epubcfi(/6/2)"))
	changes = append(changes, pos.Set("pub:SECRET-dune", "epubcfi(/6/SECRET-14)"))
	pushChanges(h, spaceID, changes)

	got, err := h.reader().ReadingPositions(spaceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].PubID != "pub:SECRET-dune" || got[0].Position != "epubcfi(/6/SECRET-14)" {
		t.Errorf("ReadingPositions = %+v, want the latest position for pub:SECRET-dune", got)
	}
	h.assertNoPlaintextAtRest(spaceID, "pub:SECRET-dune", "SECRET-14")
}
