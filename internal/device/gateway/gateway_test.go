// The gateway is tested end to end against a REAL controller — a real database,
// a real CAS, the real OpenSubsonic adapter and the real encrypted personal-state
// API — and a REAL device decrypt path. Nothing on the invariant-critical path is
// mocked: the playlist is genuinely encrypted client-side, pushed as ciphertext,
// and decrypted on the device by the gateway's production Library. A recording
// middleware wraps the whole controller so the test can assert what the
// controller did and did not see.
//
//nolint:bodyclose // response bodies are closed by the t.Cleanup the harness registers
package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	psapi "github.com/rarebit-one/heyarr-core/internal/api/personalstate"
	"github.com/rarebit-one/heyarr-core/internal/api/subsonic"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	apiclient "github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/device"
	"github.com/rarebit-one/heyarr-core/internal/device/gateway"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	psclient "github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync"
	psstore "github.com/rarebit-one/heyarr-core/internal/personalstate/store"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

const stamp = "2026-08-01T00:00:00Z"

// The plaintext playlist items. These strings are the whole point of the
// invariant assertion: they are what the gateway serves the app and what the
// controller must NEVER see in the clear.
var secretItems = []string{"tr:ea1", "SECRET-track-hunter2"}

// The app's credential (to the DEVICE) and, distinct from it, the device's
// credential (to the CONTROLLER).
const (
	appUser   = "phone"
	appPass   = "app-side-secret"
	ctlUser   = "device"
	trackFLAC = "the-datum-flac-bytes-0123456789"
)

type harness struct {
	t         *testing.T
	gateway   *httptest.Server
	api       *apiclient.Client // the device's controller client, to inspect ciphertext at rest
	spaceID   string
	ctlBodies func() []byte   // everything the controller wrote as a response body
	ctlPaths  func() []string // every path the controller received a request for
}

func newHarness(t *testing.T) *harness {
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
	casStore, err := cas.OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blobHandler, err := blobs.New(blobs.Options{Store: casStore, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := subsonic.New(subsonic.Options{
		DB: db, Auth: verifier, Blobs: blobHandler,
		ServerVersion: "test-controller", Logger: slog.New(slog.DiscardHandler),
	})
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
		Build:              buildinfo.Info{Version: "test", Commit: "abc", Date: stamp},
		SchemaVersion:      4,
		KnownSchemaVersion: 4,
		Mount:              []httpapi.MountFunc{ps.Mount},
		MountPublic:        []httpapi.MountFunc{sub.Mount},
	})
	if err != nil {
		t.Fatal(err)
	}

	cap := &capture{}
	controller := httptest.NewServer(cap.middleware(srv.Handler()))
	t.Cleanup(controller.Close)

	// The device's controller credential: read to fetch, write to push spaces
	// and changes.
	ctlToken := mint(t, authStore, "device", auth.ScopeRead, auth.ScopeWrite)

	seedMusic(t, ctx, db, casStore)

	// A real device, with a real key and encryption key.
	ds, err := device.NewStore(device.StoreOptions{Dir: filepath.Join(dir, "device")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ds.Generate("test-device", false); err != nil {
		t.Fatal(err)
	}

	apiClient, err := apiclient.New(apiclient.Options{Addr: controller.URL, Token: ctlToken})
	if err != nil {
		t.Fatal(err)
	}

	spaceID := createEncryptedPlaylist(t, ctx, ds, apiClient)

	gw, err := gateway.New(gateway.Options{
		Personal: gateway.NewSpaceLibrary(apiClient, filepath.Join(dir, "device")),
		Controller: gateway.Controller{
			BaseURL: controller.URL,
			User:    ctlUser,
			Bearer:  ctlToken,
			Client:  controller.Client(),
		},
		DeviceUser:     appUser,
		DevicePassword: appPass,
		ServerVersion:  "test-gateway",
	})
	if err != nil {
		t.Fatal(err)
	}
	gwServer := httptest.NewServer(gw.Handler())
	t.Cleanup(gwServer.Close)

	return &harness{
		t: t, gateway: gwServer, api: apiClient, spaceID: spaceID,
		ctlBodies: cap.bodiesSnapshot, ctlPaths: cap.pathsSnapshot,
	}
}

func mint(t *testing.T, store *auth.Store, name string, scopes ...auth.Scope) string {
	t.Helper()
	created, err := store.Create(context.Background(), name, scopes, nil)
	if err != nil {
		t.Fatal(err)
	}
	return created.Secret
}

// createEncryptedPlaylist mints a space wrapped for this device, encrypts two
// playlist adds under the space key, and pushes them as ciphertext. It returns
// the space id. Everything here is the ordinary device-side flow (§73, ADR-0049)
// — the same one `heyarr space create` / `space put` run.
func createEncryptedPlaylist(t *testing.T, ctx context.Context, ds *device.Store, c *apiclient.Client) string {
	t.Helper()
	priv, err := ds.LoadEncryptionKey()
	if err != nil {
		t.Fatal(err)
	}
	mine := encryption.FormatPublicKey(priv.PublicKey().Bytes())
	recip, err := psclient.ParseRecipient(mine)
	if err != nil {
		t.Fatal(err)
	}
	mgr := psclient.New()
	sp, wrapped, err := mgr.Create(spaces.KindPersonal, time.Now().UTC(), []psclient.Recipient{recip})
	if err != nil {
		t.Fatal(err)
	}
	req := apiclient.CreateSpaceRequest{ID: sp.ID, Kind: string(sp.Kind)}
	for _, w := range wrapped {
		req.WrappedKeys = append(req.WrappedKeys, apiclient.WrappedKeyInput{Recipient: w.Recipient, Wrapped: w.Wrapped})
	}
	if _, err := c.CreateSpace(ctx, req); err != nil {
		t.Fatal(err)
	}

	st := crdt.New()
	var parents []string
	for _, item := range secretItems {
		ch := st.Add(item)
		ec, err := statesync.Encode(mgr, sp.ID, parents, ch)
		if err != nil {
			t.Fatal(err)
		}
		id, err := c.PutChange(ctx, ec)
		if err != nil {
			t.Fatal(err)
		}
		parents = []string{id}
	}
	return sp.ID
}

func seedMusic(t *testing.T, ctx context.Context, db *sqlite.DB, store cas.Store) {
	t.Helper()
	exec := func(q string, args ...any) {
		if _, err := db.Writer().Exec(q, args...); err != nil {
			t.Fatalf("seeding (%s): %v", q, err)
		}
	}
	exec(`INSERT INTO libraries (id, name, content_type, enabled, created_at)
		VALUES ('lib-music', 'Music', 'music', 1, ?)`, stamp)
	exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES ('wa', 'music', 'music:wa', 'Contour Lines', 'contour lines', 2001, json_object('artist', 'The Cartographers'), ?, ?)`, stamp, stamp)

	desc, err := store.Put(ctx, bytes.NewReader([]byte(trackFLAC)))
	if err != nil {
		t.Fatal(err)
	}
	hash := desc.Hash.String()
	exec(`INSERT INTO editions (id, work_id, edition_key, label, edition_type, attributes, created_at)
		VALUES ('ea1', 'wa', 'ea1', 'FLAC', 'flac', json_object('disc', 1, 'track', 1, 'track_title', 'Datum'), ?)`, stamp)
	exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, ?, 'audio/flac', ?)`, hash, len(trackFLAC), stamp)
	exec(`INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash, role, filename, mime, identification_source, created_at, updated_at)
		VALUES ('aea1', 'ea1', 'lib-music', 'managed', ?, 'primary', 'Datum.flac', 'audio/flac', 'scan', ?, ?)`, hash, stamp, stamp)
}

// --- capture middleware -----------------------------------------------------

type capture struct {
	mu     sync.Mutex
	bodies bytes.Buffer
	paths  []string
}

func (c *capture) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.paths = append(c.paths, r.URL.Path)
		c.mu.Unlock()
		next.ServeHTTP(&teeWriter{ResponseWriter: w, cap: c}, r)
	})
}

func (c *capture) bodiesSnapshot() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.bodies.Bytes()...)
}

func (c *capture) pathsSnapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.paths...)
}

type teeWriter struct {
	http.ResponseWriter
	cap *capture
}

func (t *teeWriter) Write(b []byte) (int, error) {
	t.cap.mu.Lock()
	t.cap.bodies.Write(b)
	t.cap.mu.Unlock()
	return t.ResponseWriter.Write(b)
}

// --- request helpers --------------------------------------------------------

func (h *harness) creds() url.Values {
	return url.Values{"u": {appUser}, "p": {appPass}, "c": {"stock-app"}, "v": {"1.16.1"}, "f": {"json"}}
}

func (h *harness) raw(method string, extra url.Values) *http.Response {
	h.t.Helper()
	q := h.creds()
	for k, vs := range extra {
		q[k] = vs
	}
	u := h.gateway.URL + gateway.Prefix + "/" + method + "?" + q.Encode()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// rawQuery performs a request with EXACTLY the given query — no credentials
// merged in — for authentication tests that must control every parameter.
func (h *harness) rawQuery(method string, q url.Values) *http.Response {
	h.t.Helper()
	u := h.gateway.URL + gateway.Prefix + "/" + method + "?" + q.Encode()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func (h *harness) get(method string, extra url.Values) subResp {
	h.t.Helper()
	return decodeResp(h.t, h.raw(method, extra))
}

func decodeResp(t *testing.T, resp *http.Response) subResp {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		Resp subResp `json:"subsonic-response"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatalf("response is not JSON (%d): %s", resp.StatusCode, body)
	}
	return w.Resp
}

// The client-side view of the envelope, independent of the gateway's own types.
type subResp struct {
	Status string `json:"status"`
	Type   string `json:"type"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Playlists *struct {
		Playlist []playlistT `json:"playlist"`
	} `json:"playlists"`
	Playlist *playlistT `json:"playlist"`
	Artists  *struct {
		Index []struct {
			Artist []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"artist"`
		} `json:"index"`
	} `json:"artists"`
}

type playlistT struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SongCount int    `json:"songCount"`
	Entry     []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"entry"`
}
