// A member's device writes to the spaces it is a member of (ADR-0128), proven
// through the real middleware chain with real Device credentials: the
// personal-state API, the vault upload, confirm and placement routes, a device
// revoked mid-scene, and a member removed mid-scene.
//
//nolint:bodyclose // responses are closed in mh.do
package httpapi_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rarebit-one/void-which-binds-go/enrolment"
	"github.com/rarebit-one/void-which-binds-go/hashing"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	psapi "github.com/rarebit-one/heyarr-core/internal/api/personalstate"
	"github.com/rarebit-one/heyarr-core/internal/api/vaultblob"
	"github.com/rarebit-one/heyarr-core/internal/api/vaultplacement"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	psstore "github.com/rarebit-one/heyarr-core/internal/personalstate/store"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// confirmLog records whether each confirm was held to the caller's own uploads.
type confirmLog struct {
	pins
	mu      sync.Mutex
	ownOnly []bool
}

func (c *confirmLog) ConfirmUploadPins(_ context.Context, _ string, blobs []string, _ string, ownOnly bool) ([]catalog.ConfirmResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ownOnly = append(c.ownOnly, ownOnly)
	out := make([]catalog.ConfirmResult, 0, len(blobs))
	for _, b := range blobs {
		out = append(out, catalog.ConfirmResult{BlobHash: b, Status: "confirmed"})
	}
	return out, nil
}

func (c *confirmLog) last() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ownOnly[len(c.ownOnly)-1]
}

// enrolledDevice is one device of the scene: how to present it, its device
// key and its encryption key.
type enrolledDevice struct {
	credential func() string
	key, enc   string
}

type memberHarness struct {
	ts       *httptest.Server
	devices  *deviceauth.Store
	confirms *confirmLog
	admin    string
	// alice0 is alice's management-authorised device (ADR-0065); alice1,
	// bob1 and carol1 sit at the read floor (ADR-0067).
	alice0, alice1, bob1, carol1 enrolledDevice
	shared, personal             string
}

func newMemberHarness(t *testing.T) *memberHarness {
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
	ps, err := psstore.New(psstore.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		t.Fatal(err)
	}
	authority, err := psapi.NewMemberWriteAuthority(ps, devStore)
	if err != nil {
		t.Fatal(err)
	}
	psAPI, err := psapi.New(psapi.Options{
		Store: ps, Principals: authStore, Recipients: ps, Logger: slog.New(slog.DiscardHandler),
		Authorizer: devStore, Members: devStore, MemberWrites: authority,
	})
	if err != nil {
		t.Fatal(err)
	}
	casStore, err := cas.OpenFS(casDir)
	if err != nil {
		t.Fatal(err)
	}
	h := &memberHarness{devices: devStore, confirms: &confirmLog{}}
	vb, err := vaultblob.New(vaultblob.Options{
		Store: casStore, Pinner: h.confirms, SelfPeer: "peer-self", Grants: ps, Members: authority,
	})
	if err != nil {
		t.Fatal(err)
	}
	vp, err := vaultplacement.New(vaultplacement.Options{Pinner: pins{}, Grants: ps, Members: authority})
	if err != nil {
		t.Fatal(err)
	}

	enrolUser := func(name string) ed25519.PrivateKey {
		u, priv, err := enrolment.GenerateUserIdentity()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := devStore.EnrolUser(ctx, u.UserID(), name, ""); err != nil {
			t.Fatal(err)
		}
		return priv
	}
	enrolDevice := func(userPriv ed25519.PrivateKey, name, encByte string) enrolledDevice {
		pub, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		enc := "x25519:" + strings.Repeat(encByte, 32)
		cert, err := enrolment.SignCert(userPriv, pub, enc, time.Now().UTC(), 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := devStore.EnrolDevice(ctx, cert, name); err != nil {
			t.Fatal(err)
		}
		return enrolledDevice{
			key: identity.FormatPublicKey(pub), enc: enc,
			credential: func() string {
				proof, err := enrolment.SignPossession(priv, cert, time.Now().UTC(), 0)
				if err != nil {
					t.Fatal(err)
				}
				return "Device " + cert + "~" + proof
			},
		}
	}
	alice, bob, carol := enrolUser("alice"), enrolUser("bob"), enrolUser("carol")
	h.alice0 = enrolDevice(alice, "alice laptop", "a0")
	h.alice1 = enrolDevice(alice, "alice phone", "a1")
	h.bob1 = enrolDevice(bob, "bob phone", "b1")
	h.carol1 = enrolDevice(carol, "carol phone", "c1")

	srv, err := httpapi.New(httpapi.Options{
		Config: cfg, Logger: slog.New(slog.DiscardHandler), DB: db, Verifier: verifier,
		DeviceVerifier: devStore, ManagementAuthorizer: stubMgmt{h.alice0.key: true}, Events: eventLog,
		Build:         buildinfo.Info{Version: "test", Commit: "abc123", Date: "2026-08-20T00:00:00Z"},
		SchemaVersion: 1, KnownSchemaVersion: 1, CASRoot: casDir,
		Mount: []httpapi.MountFunc{psAPI.Mount, vb.Mount, vp.Mount},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.ts = httptest.NewServer(srv.Handler())
	t.Cleanup(h.ts.Close)
	svc, err := authStore.Create(ctx, "operator", []auth.Scope{auth.ScopeRead, auth.ScopeWrite, auth.ScopeAdmin}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.admin = "Bearer " + svc.Secret

	// Alice's authorised device creates a shared space wrapped for both her
	// devices and shares it with bob, then wraps it for bob's phone; and a
	// personal space wrapped for her laptop only.
	h.shared, h.personal = uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
	wraps := func(encs ...string) []map[string]any {
		out := []map[string]any{}
		for _, e := range encs {
			out = append(out, map[string]any{"recipient": e, "wrapped": []byte("wrap for " + e[7:11])})
		}
		return out
	}
	h.mustStatus(t, http.MethodPost, "/spaces", h.alice0.credential(), map[string]any{
		"id": h.shared, "kind": "shared", "wrapped_keys": wraps(h.alice0.enc, h.alice1.enc),
	}, http.StatusCreated)
	h.mustStatus(t, http.MethodPost, "/spaces", h.alice0.credential(), map[string]any{
		"id": h.personal, "kind": "personal", "wrapped_keys": wraps(h.alice0.enc),
	}, http.StatusCreated)
	h.mustStatus(t, http.MethodPost, "/spaces/"+h.shared+"/members", h.alice0.credential(),
		map[string]any{"user": "bob"}, http.StatusCreated)
	h.mustStatus(t, http.MethodPost, "/spaces/"+h.shared+"/keys", h.alice0.credential(),
		map[string]any{"wrapped_keys": wraps(h.bob1.enc)}, http.StatusOK)
	return h
}

func (h *memberHarness) do(t *testing.T, method, path, authz string, body any) (int, string) {
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
	req, err := http.NewRequest(method, h.ts.URL+httpapi.APIPrefix+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := body.([]byte); !ok && body != nil {
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

func (h *memberHarness) mustStatus(t *testing.T, method, path, authz string, body any, want int) string {
	t.Helper()
	got, b := h.do(t, method, path, authz, body)
	if got != want {
		t.Fatalf("%s %s = %d, want %d\n%s", method, path, got, want, b)
	}
	return b
}

// mustRefuse asserts a 403 carrying code ("" for any 403).
func (h *memberHarness) mustRefuse(t *testing.T, method, path, authz string, body any, code string) {
	t.Helper()
	got, b := h.do(t, method, path, authz, body)
	if got != http.StatusForbidden || (code != "" && !strings.Contains(b, `"code":"`+code+`"`)) {
		t.Fatalf("%s %s = %d %s, want 403 %s", method, path, got, b, code)
	}
}

func (h *memberHarness) change(t *testing.T, spaceID string, n int) map[string]any {
	t.Helper()
	ch, err := protocol.NewChange(spaceID, nil, []byte{byte(n), 0xc7})
	if err != nil {
		t.Fatal(err)
	}
	buf, err := json.Marshal(ch)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func blobOf(b []byte) string {
	hh := hashing.New()
	_, _ = hh.Write(b)
	return hh.Sum().String()
}

// TestAMembersDeviceWritesItsSpacesAndNothingElse is ADR-0128 end to end.
func TestAMembersDeviceWritesItsSpacesAndNothingElse(t *testing.T) {
	t.Parallel()
	h := newMemberHarness(t)
	changes := func(space string) string { return "/spaces/" + space + "/changes?key_epoch=0" }

	// Changes: each device that holds the space's current key and whose user
	// is a member pushes with its own credential; nobody else does.
	h.mustStatus(t, http.MethodPost, changes(h.shared), h.alice1.credential(), h.change(t, h.shared, 1), http.StatusCreated)
	h.mustStatus(t, http.MethodPost, changes(h.shared), h.bob1.credential(), h.change(t, h.shared, 2), http.StatusCreated)
	h.mustRefuse(t, http.MethodPost, changes(h.shared), h.carol1.credential(), h.change(t, h.shared, 3), httpapi.CodeSpaceNotMember)
	// Alice's own phone holds no copy of her personal space: no write there.
	h.mustRefuse(t, http.MethodPost, changes(h.personal), h.alice1.credential(), h.change(t, h.personal, 4), httpapi.CodeSpaceNotMember)
	// Bob's membership of one space reaches no other.
	h.mustRefuse(t, http.MethodPost, changes(h.personal), h.bob1.credential(), h.change(t, h.personal, 5), httpapi.CodeSpaceNotMember)

	// The space says so on own-devices, so a client knows which credential to use.
	_, body := h.do(t, http.MethodGet, "/spaces/"+h.shared+"/own-devices", h.bob1.credential(), nil)
	if !strings.Contains(body, `"member_write":true`) {
		t.Errorf("own-devices for bob on the shared space = %s, want member_write true", body)
	}
	_, body = h.do(t, http.MethodGet, "/spaces/"+h.personal+"/own-devices", h.alice1.credential(), nil)
	if !strings.Contains(body, `"member_write":false`) {
		t.Errorf("own-devices for alice's phone on her personal space = %s, want member_write false", body)
	}

	// Vault blobs and pins carry no space: a writing member of some space
	// uploads, confirms only its own user's uploads, and pins; a stranger
	// does none of it; and nobody but the write scope removes a pin.
	payload := []byte("a member's ciphertext")
	hash := blobOf(payload)
	h.mustStatus(t, http.MethodPut, "/vault/blobs/"+hash, h.bob1.credential(), payload, http.StatusCreated)
	h.mustRefuse(t, http.MethodPut, "/vault/blobs/"+hash, h.carol1.credential(), payload, httpapi.CodeSpaceNotMember)
	confirm := map[string]any{"blobs": []string{hash}}
	h.mustStatus(t, http.MethodPost, "/vault/blobs/confirm", h.bob1.credential(), confirm, http.StatusOK)
	if !h.confirms.last() {
		t.Error("a member's device confirmed beyond its own user's uploads")
	}
	h.mustStatus(t, http.MethodPost, "/vault/blobs/confirm", h.admin, confirm, http.StatusOK)
	if h.confirms.last() {
		t.Error("the write scope's confirm was narrowed to its own uploads")
	}
	h.mustRefuse(t, http.MethodPost, "/vault/blobs/confirm", h.carol1.credential(), confirm, httpapi.CodeSpaceNotMember)
	pin := map[string]any{"blob_hash": hash, "peer_id": "peer-b"}
	h.mustStatus(t, http.MethodPost, "/vault/placements", h.bob1.credential(), pin, http.StatusOK)
	h.mustRefuse(t, http.MethodPost, "/vault/placements", h.carol1.credential(), pin, httpapi.CodeSpaceNotMember)
	h.mustRefuse(t, http.MethodDelete, "/vault/placements", h.bob1.credential(), pin, "")

	// Nothing else opens to a member's device: not compaction, admin, key
	// management, membership, grants or snapshots.
	sp := "/spaces/" + h.shared
	junk := map[string]any{}
	for _, rt := range []struct{ method, path string }{
		{http.MethodPost, sp + "/compact"},
		{http.MethodPost, sp + "/rotate"},
		{http.MethodDelete, sp + "/keys/" + h.alice0.enc},
		{http.MethodPost, sp + "/keys"},
		{http.MethodPost, sp + "/snapshots"},
		{http.MethodPost, sp + "/members"},
		{http.MethodPost, sp + "/grants"},
		{http.MethodPost, "/state/replicate"},
		{http.MethodPost, "/spaces"},
	} {
		h.mustRefuse(t, rt.method, rt.path, h.bob1.credential(), junk, "")
	}

	// A removed member's device is shut out, though its copy remains until a
	// rotation.
	h.mustStatus(t, http.MethodDelete, sp+"/members/bob", h.alice0.credential(), nil, http.StatusOK)
	h.mustRefuse(t, http.MethodPost, changes(h.shared), h.bob1.credential(), h.change(t, h.shared, 6), httpapi.CodeSpaceNotMember)
	h.mustRefuse(t, http.MethodPut, "/vault/blobs/"+hash, h.bob1.credential(), payload, httpapi.CodeSpaceNotMember)

	// A revoked device stops authenticating at all.
	if _, err := h.devices.RevokeDevice(context.Background(), h.alice1.key); err != nil {
		t.Fatal(err)
	}
	h.mustStatus(t, http.MethodPost, changes(h.shared), h.alice1.credential(), h.change(t, h.shared, 7), http.StatusUnauthorized)
}

// memberWritesFunc is a MemberWrites from a function.
type memberWritesFunc func(principalID, deviceKey, spaceID string) (bool, error)

func (f memberWritesFunc) MemberMayWrite(_ context.Context, principalID, deviceKey, spaceID string) (bool, error) {
	return f(principalID, deviceKey, spaceID)
}

// TestRequireWriteOrMember is the middleware's table: who is asked, what each
// answer becomes, and that a lookup error never lets a request through.
func TestRequireWriteOrMember(t *testing.T) {
	t.Parallel()
	readDev := auth.Identity{DeviceKey: "ed25519:dev", Principal: auth.Principal{ID: "p-1", Kind: auth.KindUser},
		Token: auth.Token{Scopes: []auth.Scope{auth.ScopeRead}}}
	yes := memberWritesFunc(func(p, d, s string) (bool, error) { return p == "p-1" && d == "ed25519:dev" && s == "sp-1", nil })
	no := memberWritesFunc(func(string, string, string) (bool, error) { return false, nil })
	broken := memberWritesFunc(func(string, string, string) (bool, error) { return false, errors.New("disk on fire") })
	restricted := readDev
	restricted.Restricted = true
	guestID := auth.Identity{Guest: true, Token: auth.Token{Scopes: []auth.Scope{auth.ScopeRead}}}
	bearerRead := auth.Identity{Principal: auth.Principal{ID: "p-1", Kind: auth.KindUser},
		Token: auth.Token{Scopes: []auth.Scope{auth.ScopeRead}}}
	bearerWrite := bearerRead
	bearerWrite.Token.Scopes = []auth.Scope{auth.ScopeRead, auth.ScopeWrite}

	for _, tc := range []struct {
		name   string
		id     *auth.Identity
		m      httpapi.MemberWrites
		space  string
		want   int
		lifted bool
	}{
		{"no identity", nil, yes, "sp-1", http.StatusUnauthorized, false},
		{"the write scope passes without asking", &bearerWrite, broken, "sp-1", http.StatusOK, false},
		{"a member's device is lifted", &readDev, yes, "sp-1", http.StatusOK, true},
		{"on another space it is not", &readDev, yes, "sp-2", http.StatusForbidden, false},
		{"a non-member's device", &readDev, no, "sp-1", http.StatusForbidden, false},
		{"a lookup error fails closed", &readDev, broken, "sp-1", http.StatusInternalServerError, false},
		{"no authority fails closed", &readDev, nil, "sp-1", http.StatusForbidden, false},
		{"a read bearer is never asked", &bearerRead, yes, "sp-1", http.StatusForbidden, false},
		{"a restricted read identity is never asked", &restricted, yes, "sp-1", http.StatusForbidden, false},
		{"a guest is never asked", &guestID, yes, "sp-1", http.StatusForbidden, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chi.NewRouter()
			var lifted bool
			r.With(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if tc.id != nil {
						req = req.WithContext(httpapi.ContextWithIdentity(req.Context(), *tc.id))
					}
					next.ServeHTTP(w, req)
				})
			}, httpapi.RequireWriteOrMember(tc.m, "id")).Post("/spaces/{id}/changes", func(w http.ResponseWriter, req *http.Request) {
				lifted = httpapi.MemberLifted(req.Context())
			})
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/spaces/"+tc.space+"/changes", nil))
			if rec.Code != tc.want || lifted != tc.lifted {
				t.Fatalf("got %d (lifted %v) %s, want %d (lifted %v)", rec.Code, lifted, rec.Body, tc.want, tc.lifted)
			}
		})
	}
}
