// Restricted principals and the per-space access list (ADR-0104), proven through
// the real middleware chain, the real personal-state API, blob route and vault
// routes: every route against a restricted principal with a grant, one without,
// a user's device, a guest and an ordinary service token. The last three rows
// are the regression guard — their answers are what they were before ADR-0104.
//
//nolint:bodyclose // responses are closed in rh.do
package httpapi_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rarebit-one/void-which-binds-go/enrolment"
	"github.com/rarebit-one/void-which-binds-go/hashing"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	psapi "github.com/rarebit-one/heyarr-core/internal/api/personalstate"
	"github.com/rarebit-one/heyarr-core/internal/api/vaultblob"
	"github.com/rarebit-one/heyarr-core/internal/api/vaultplacement"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/guest"
	"github.com/rarebit-one/heyarr-core/internal/leases"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	psstore "github.com/rarebit-one/heyarr-core/internal/personalstate/store"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// vaultIndex is a fixed set of vault blob hashes — the catalog's answer, without
// the catalog. The catalog's own query is tested where it lives.
type vaultIndex map[string]bool

func (v vaultIndex) IsVaultBlob(_ context.Context, h string) (bool, error) { return v[h], nil }

// pins records placement pins, accepting everything.
type pins struct{}

func (pins) RecordVaultBlob(context.Context, string, int64, string) error { return nil }
func (pins) PinPlacement(context.Context, string, string) error           { return nil }
func (pins) UnpinPlacement(context.Context, string, string) error         { return nil }

type restrictedHarness struct {
	ts *httptest.Server

	// Authorization header values, one per caller kind. guest is "".
	granted, ungranted, readOnly, service string

	spaceID, otherSpaceID     string
	vaultBlob, mediaBlob      string
	uploadHash                string
	uploadBody                []byte
	executorID, executor2Name string
	deviceCredential          func() string
}

func newRestrictedHarness(t *testing.T) *restrictedHarness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "heyarr.db")
	testdb.WriteMigrated(t, dbPath)
	db, err := sqlite.Open(ctx, sqlite.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	casDir := filepath.Join(dir, "cas")
	if err := os.MkdirAll(casDir, 0o750); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.Peer = config.Peer{Name: "test-peer", Site: "test-site"}
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = ""
	cfg.HTTP.Guest.Enabled = true

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
	devStore, err := deviceauth.New(deviceauth.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	leaseStore, err := leases.New(leases.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog, Signer: signer})
	if err != nil {
		t.Fatal(err)
	}
	ps, err := psstore.New(psstore.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		t.Fatal(err)
	}
	psAPI, err := psapi.New(psapi.Options{Store: ps, Principals: authStore, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	casStore, err := cas.OpenFS(casDir)
	if err != nil {
		t.Fatal(err)
	}

	h := &restrictedHarness{}
	put := func(b []byte) string {
		d, err := casStore.Put(ctx, bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		return d.Hash.String()
	}
	h.vaultBlob = put([]byte("vault ciphertext"))
	h.mediaBlob = put([]byte("a media file"))
	h.uploadBody = []byte("ciphertext an executor pushes")
	hh := hashing.New()
	_, _ = hh.Write(h.uploadBody)
	h.uploadHash = hh.Sum().String()

	blobHandler, err := blobs.New(blobs.Options{
		Store: casStore, VaultBlobs: vaultIndex{h.vaultBlob: true}, Grants: ps,
	})
	if err != nil {
		t.Fatal(err)
	}
	vb, err := vaultblob.New(vaultblob.Options{Store: casStore, Pinner: pins{}, SelfPeer: "peer-self", Grants: ps})
	if err != nil {
		t.Fatal(err)
	}
	vp, err := vaultplacement.New(vaultplacement.Options{Pinner: pins{}, Grants: ps})
	if err != nil {
		t.Fatal(err)
	}

	// The household's device: enrolled, and authorised for write (ADR-0065).
	u, userPriv, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	devicePub, devicePriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := enrolment.SignCert(userPriv, devicePub, "", time.Now().UTC(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := devStore.EnrolUser(ctx, u.UserID(), "household", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := devStore.EnrolDevice(ctx, cert, "laptop"); err != nil {
		t.Fatal(err)
	}
	h.deviceCredential = func() string {
		proof, err := enrolment.SignPossession(devicePriv, cert, time.Now().UTC(), 0)
		if err != nil {
			t.Fatal(err)
		}
		return "Device " + cert + "~" + proof
	}

	srv, err := httpapi.New(httpapi.Options{
		Config: cfg, Logger: slog.New(slog.DiscardHandler), DB: db, Verifier: verifier,
		DeviceVerifier: devStore, ManagementAuthorizer: stubMgmt{identity.FormatPublicKey(devicePub): true},
		GuestLeases: guest.NewMinter(leaseStore, guest.DefaultTTL), Events: eventLog,
		Build:         buildinfo.Info{Version: "test", Commit: "abc123", Date: "2026-08-20T00:00:00Z"},
		SchemaVersion: 1, KnownSchemaVersion: 1, CASRoot: casDir,
		// testRoutes stands in for the media and catalog surface (/probe).
		Mount: []httpapi.MountFunc{testRoutes, psAPI.Mount, blobHandler.Mount, vb.Mount, vp.Mount},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(h.ts.Close)

	mint := func(f func() (auth.CreatedToken, error)) auth.CreatedToken {
		c, err := f()
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	exec := mint(func() (auth.CreatedToken, error) { return authStore.CreateExecutor(ctx, "executor-a", nil) })
	exec2 := mint(func() (auth.CreatedToken, error) { return authStore.CreateExecutor(ctx, "executor-b", nil) })
	exec3 := mint(func() (auth.CreatedToken, error) { return authStore.CreateExecutor(ctx, "executor-c", nil) })
	svc := mint(func() (auth.CreatedToken, error) {
		return authStore.Create(ctx, "player", []auth.Scope{auth.ScopeRead, auth.ScopeWrite, auth.ScopeAdmin}, nil)
	})
	h.granted, h.ungranted, h.readOnly = "Bearer "+exec.Secret, "Bearer "+exec2.Secret, "Bearer "+exec3.Secret
	h.service = "Bearer " + svc.Secret
	h.executorID, h.executor2Name = exec.Token.PrincipalID, "executor-b"

	// The device creates two spaces (so it owns them) and grants executor-a
	// read,write on the first and executor-c read on the first; the second is
	// granted to nobody.
	h.spaceID, h.otherSpaceID = uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
	for _, id := range []string{h.spaceID, h.otherSpaceID} {
		h.mustStatus(t, http.MethodPost, "/spaces", h.deviceCredential(), map[string]any{
			"id": id, "kind": "family",
			"wrapped_keys": []map[string]any{
				{"recipient": "x25519:" + strings.Repeat("11", 32), "wrapped": []byte("owner wrap")},
				{"recipient": "x25519:" + strings.Repeat("22", 32), "wrapped": []byte("someone else's wrap")},
			},
		}, http.StatusCreated)
	}
	h.mustStatus(t, http.MethodPost, "/spaces/"+h.spaceID+"/grants", h.deviceCredential(),
		map[string]any{"principal": "executor-a", "caps": "read,write"}, http.StatusCreated)
	h.mustStatus(t, http.MethodPost, "/spaces/"+h.spaceID+"/grants", h.deviceCredential(),
		map[string]any{"principal": exec3.Token.PrincipalID, "caps": "read"}, http.StatusCreated)
	return h
}

// do sends a request; body is JSON-encoded unless it is already []byte.
func (h *restrictedHarness) do(t *testing.T, method, path, authz string, body any) (int, string) {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(buf)
	}
	if !strings.HasPrefix(path, "/metrics") {
		path = httpapi.APIPrefix + path
	}
	req, err := http.NewRequest(method, h.ts.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := body.(map[string]any); ok {
		req.Header.Set("Content-Type", "application/json")
	}
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (h *restrictedHarness) mustStatus(t *testing.T, method, path, authz string, body any, want int) string {
	t.Helper()
	got, b := h.do(t, method, path, authz, body)
	if got != want {
		t.Fatalf("%s %s = %d, want %d\n%s", method, path, got, want, b)
	}
	return b
}

// passed is "the gate let it through": the handler answered with something
// other than an authentication, authorisation or not-found refusal.
const passed = -1

// TestRestrictedPrincipalRouteMatrix is the ADR-0104 handler table: every route
// × {restricted-granted, restricted-ungranted, user device, guest, service}.
func TestRestrictedPrincipalRouteMatrix(t *testing.T) {
	t.Parallel()
	h := newRestrictedHarness(t)
	sp, other := h.spaceID, h.otherSpaceID
	junk := map[string]any{}

	type row struct {
		method, path string
		body         any
		// granted, ungranted, device, guest, service
		want [5]int
	}
	F, N, OK := http.StatusForbidden, http.StatusNotFound, http.StatusOK
	rows := []row{
		// Personal-state reads: the granted executor reaches its space; the
		// ungranted one sees it as absent.
		{"GET", "/spaces", nil, [5]int{OK, OK, OK, F, OK}},
		{"GET", "/spaces/" + sp + "/keys", nil, [5]int{OK, N, OK, F, OK}},
		{"GET", "/spaces/" + sp + "/key-history", nil, [5]int{OK, N, OK, F, OK}},
		{"GET", "/spaces/" + sp + "/changes", nil, [5]int{OK, N, OK, F, OK}},
		{"GET", "/spaces/" + other + "/changes", nil, [5]int{N, N, OK, F, OK}},
		// Pushes: past the gate a junk body is the handler's 400.
		{"POST", "/spaces/" + sp + "/changes", junk, [5]int{400, N, 400, F, 400}},
		{"POST", "/spaces/" + other + "/changes", junk, [5]int{N, N, 400, F, 400}},
		{"POST", "/spaces/" + sp + "/snapshots", junk, [5]int{400, N, 400, F, 400}},
		// Never for a restricted principal, whatever it holds.
		{"POST", "/spaces", junk, [5]int{F, F, 400, F, 400}},
		{"POST", "/spaces/" + sp + "/keys", junk, [5]int{F, F, 400, F, 400}},
		{"DELETE", "/spaces/" + sp + "/keys/x25519:" + strings.Repeat("33", 32), nil, [5]int{F, F, F, F, http.StatusNoContent}},
		{"POST", "/spaces/" + sp + "/rotate", junk, [5]int{F, F, F, F, 400}},
		{"POST", "/spaces/" + sp + "/compact", junk, [5]int{F, F, F, F, passed}},
		{"POST", "/state/replicate", nil, [5]int{F, F, F, F, http.StatusServiceUnavailable}},
		// The grant API: a device only; a bearer, even an admin one, is refused.
		{"DELETE", "/spaces/" + sp + "/grants/" + h.executor2Name, nil, [5]int{F, F, N, F, F}},
		// Blobs: a vault blob by hash, never media.
		{"GET", "/blobs/" + h.vaultBlob + "/content", nil, [5]int{OK, N, OK, OK, OK}},
		{"HEAD", "/blobs/" + h.vaultBlob + "/content", nil, [5]int{OK, N, OK, OK, OK}},
		{"GET", "/blobs/" + h.mediaBlob + "/content", nil, [5]int{N, N, OK, OK, OK}},
		{"PUT", "/vault/blobs/" + h.uploadHash, h.uploadBody, [5]int{http.StatusCreated, F, passed, F, passed}},
		{"POST", "/vault/placements", map[string]any{"blob_hash": h.vaultBlob, "peer_id": "peer-b"}, [5]int{passed, F, passed, F, passed}},
		{"DELETE", "/vault/placements", map[string]any{"blob_hash": h.vaultBlob, "peer_id": "peer-b"}, [5]int{F, F, passed, F, passed}},
		// Everything else is closed to a restricted principal by default.
		{"GET", "/probe", nil, [5]int{F, F, OK, OK, OK}},
		{"GET", "/system", nil, [5]int{F, F, OK, passed, OK}},
		{"GET", "/metrics", nil, [5]int{F, F, OK, passed, OK}},
	}
	callers := []string{"granted", "ungranted", "device", "guest", "service"}
	for _, rw := range rows {
		for i, who := range callers {
			authz := map[string]string{
				"granted": h.granted, "ungranted": h.ungranted, "guest": "", "service": h.service,
			}[who]
			if who == "device" {
				authz = h.deviceCredential()
			}
			got, body := h.do(t, rw.method, rw.path, authz, rw.body)
			want := rw.want[i]
			if want == passed {
				if got == http.StatusUnauthorized || got == F || got == N || got == http.StatusTooManyRequests {
					t.Errorf("%s %s as %s = %d, want the gate to let it through\n%s", rw.method, rw.path, who, got, body)
				}
				continue
			}
			if got != want {
				t.Errorf("%s %s as %s = %d, want %d\n%s", rw.method, rw.path, who, got, want, body)
			}
		}
	}
}

// A restricted principal enumerates only its granted spaces and sees only its
// own wraps — with no service recipient registered yet, none at all. Every
// other caller sees every space and every wrap, as before.
func TestRestrictedListingsAreFiltered(t *testing.T) {
	t.Parallel()
	h := newRestrictedHarness(t)

	var list struct {
		Spaces []struct{ ID string } `json:"spaces"`
	}
	if err := json.Unmarshal([]byte(h.mustStatus(t, "GET", "/spaces", h.granted, nil, 200)), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Spaces) != 1 || list.Spaces[0].ID != h.spaceID {
		t.Errorf("the granted executor listed %+v, want only its granted space", list.Spaces)
	}
	if err := json.Unmarshal([]byte(h.mustStatus(t, "GET", "/spaces", h.ungranted, nil, 200)), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Spaces) != 0 {
		t.Errorf("the ungranted executor listed %+v, want nothing", list.Spaces)
	}
	if err := json.Unmarshal([]byte(h.mustStatus(t, "GET", "/spaces", h.service, nil, 200)), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Spaces) != 2 {
		t.Errorf("a service token listed %d spaces, want both", len(list.Spaces))
	}

	var keys struct {
		WrappedKeys []struct{ Recipient string } `json:"wrapped_keys"`
	}
	if err := json.Unmarshal([]byte(h.mustStatus(t, "GET", "/spaces/"+h.spaceID+"/keys", h.granted, nil, 200)), &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys.WrappedKeys) != 0 {
		t.Errorf("a restricted caller was shown other recipients' wraps: %+v", keys.WrappedKeys)
	}
	if err := json.Unmarshal([]byte(h.mustStatus(t, "GET", "/spaces/"+h.spaceID+"/keys", h.deviceCredential(), nil, 200)), &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys.WrappedKeys) != 2 {
		t.Errorf("the owner's device saw %d wraps, want both", len(keys.WrappedKeys))
	}
}

// A read grant does not push; a revoke closes the gate on the next request; and
// only a device grants — never a bearer, never on a user principal.
func TestGrantCapsAndRevocation(t *testing.T) {
	t.Parallel()
	h := newRestrictedHarness(t)
	sp := h.spaceID

	// executor-c holds read only.
	h.mustStatus(t, "GET", "/spaces/"+sp+"/changes", h.readOnly, nil, 200)
	body := h.mustStatus(t, "POST", "/spaces/"+sp+"/changes", h.readOnly, map[string]any{}, http.StatusForbidden)
	if !strings.Contains(body, httpapi.CodeRestrictedPrincipal) {
		t.Errorf("a read-only push was refused without the restricted code: %s", body)
	}
	// And a read grant is not enough to store vault content.
	h.mustStatus(t, "PUT", "/vault/blobs/"+h.uploadHash, h.readOnly, h.uploadBody, http.StatusForbidden)

	// A bearer — even admin — cannot grant; nor can a user principal be granted.
	h.mustStatus(t, "POST", "/spaces/"+sp+"/grants", h.service,
		map[string]any{"principal": h.executor2Name, "caps": "read"}, http.StatusForbidden)
	h.mustStatus(t, "POST", "/spaces/"+sp+"/grants", h.deviceCredential(),
		map[string]any{"principal": "player", "caps": "read"}, http.StatusBadRequest)
	h.mustStatus(t, "POST", "/spaces/"+sp+"/grants", h.deviceCredential(),
		map[string]any{"principal": "nobody", "caps": "read"}, http.StatusNotFound)

	// Revoke executor-a; its next fetch is a 404 and it lists nothing.
	h.mustStatus(t, "GET", "/spaces/"+sp+"/changes", h.granted, nil, 200)
	h.mustStatus(t, "DELETE", "/spaces/"+sp+"/grants/"+h.executorID, h.deviceCredential(), nil, http.StatusNoContent)
	h.mustStatus(t, "GET", "/spaces/"+sp+"/changes", h.granted, nil, http.StatusNotFound)
	h.mustStatus(t, "GET", "/blobs/"+h.vaultBlob+"/content", h.granted, nil, http.StatusNotFound)
	h.mustStatus(t, "PUT", "/vault/blobs/"+h.uploadHash, h.granted, h.uploadBody, http.StatusForbidden)
}

// The confinement is deny-by-default: a route this test has never heard of,
// mounted beside the others, is closed to a restricted principal and open to
// everyone else.
func TestAnUnlistedRouteIsClosedToARestrictedPrincipal(t *testing.T) {
	t.Parallel()
	h := newRestrictedHarness(t)
	body := h.mustStatus(t, "GET", "/personal", h.granted, nil, http.StatusForbidden)
	if !strings.Contains(body, httpapi.CodeRestrictedPrincipal) {
		t.Errorf("refusal lacks the restricted code: %s", body)
	}
	h.mustStatus(t, "GET", "/personal", h.service, nil, http.StatusOK)
}

// The restricted blob budget is a fixed window per principal, on an injected
// clock (ADR-0017): spent within a window, independent across principals, and
// renewed when the window rolls over.
func TestRateLimiterWindows(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	l := httpapi.NewRateLimiter(2, time.Minute, func() time.Time { return now })
	for i := range 2 {
		if !l.Allow("a") {
			t.Fatalf("request %d of 2 was refused", i+1)
		}
	}
	if l.Allow("a") {
		t.Error("a third request inside the window was allowed")
	}
	if !l.Allow("b") {
		t.Error("one principal's spend refused another")
	}
	now = now.Add(time.Minute)
	if !l.Allow("a") {
		t.Error("the budget did not renew with the window")
	}
}
