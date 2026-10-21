package httpapi_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rarebit-one/void-which-binds-go/enrolment"
	"github.com/rarebit-one/void-which-binds-go/grant"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/leases"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// These prove ADR-0048's amended acceptance at the HTTP boundary (#285): with
// the issuing sibling nowhere in sight, a device of a pinned user that has no
// management grant here WRITES on a lease this node cached from a sibling it
// opted into, and the same write without one is 403 while the read still
// succeeds. Everything runs through the real middleware chain (authenticate ->
// RequireScope), a real device store and a real lease cache, on an injected
// clock.

// The lease cache is what the controller wires as DeviceWriteLeases.
var _ httpapi.DeviceWriteLeases = (*leases.CacheStore)(nil)

// liftClock is the server's clock, moved by the test rather than by sleeping.
type liftClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *liftClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *liftClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// spyLeases counts how often the request path consults the lease check.
type spyLeases struct {
	inner httpapi.DeviceWriteLeases
	calls atomic.Int64
}

func (s *spyLeases) LiftDeviceWrite(ctx context.Context, deviceKey string, now time.Time) (string, error) {
	s.calls.Add(1)
	return s.inner.LiftDeviceWrite(ctx, deviceKey, now)
}

// keySet is a leases.SiblingKeys over a fixed map, or an error.
type keySet struct {
	keys map[string]ed25519.PublicKey
	err  error
}

func (k keySet) PeerKeys(context.Context) (map[string]ed25519.PublicKey, error) { return k.keys, k.err }

// switchKeys is the cache's honoured set (in production, the opt-in ∩
// membership), switchable while the server runs. Unset, it honours none.
type switchKeys struct {
	mu    sync.Mutex
	inner leases.SiblingKeys
}

func (s *switchKeys) PeerKeys(ctx context.Context) (map[string]ed25519.PublicKey, error) {
	s.mu.Lock()
	inner := s.inner
	s.mu.Unlock()
	if inner == nil {
		return map[string]ed25519.PublicKey{}, nil
	}
	return inner.PeerKeys(ctx)
}

type liftOptions struct {
	optIn    []string // config peer.lease_issuers
	mgmt     stubMgmt // this node's management grants
	sessions httpapi.SessionValidator
}

type liftHarness struct {
	ts     *httptest.Server
	srv    *httpapi.Server
	cache  *leases.CacheStore
	spy    *spyLeases
	store  *deviceauth.Store
	tokens *auth.Store
	clock  *liftClock
	logs   *syncBuffer
	base   time.Time

	honoured   *switchKeys
	selfID     string
	keyA, keyB ed25519.PrivateKey // A is opted into; B is a pinned sibling that is not
	idA, idB   string
	devD, devE string
	privD      ed25519.PrivateKey
	certD      string
}

// newLiftHarness is site S (this server) with siblings A and B, and device D of
// a user S has pinned. The cache starts empty and honours nobody until the
// test says (honour / setHonoured).
func newLiftHarness(t *testing.T, opts liftOptions) *liftHarness {
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

	h := &liftHarness{logs: &syncBuffer{}, honoured: &switchKeys{}}
	h.base = time.Now().UTC().Truncate(time.Second)
	h.clock = &liftClock{t: h.base}

	selfPub, _, _ := ed25519.GenerateKey(nil)
	_, h.keyA, _ = ed25519.GenerateKey(nil)
	_, h.keyB, _ = ed25519.GenerateKey(nil)
	h.selfID = identity.FormatPublicKey(selfPub)
	h.idA = identity.FormatPublicKey(h.keyA.Public().(ed25519.PublicKey))
	h.idB = identity.FormatPublicKey(h.keyB.Public().(ed25519.PublicKey))

	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.Peer = config.Peer{Name: "peer-s", Site: "site-s", LeaseIssuers: opts.optIn}
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = filepath.Join(shortTempDir(t), "h.sock")

	h.tokens, err = auth.NewStore(auth.StoreOptions{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := auth.NewVerifier(auth.VerifierOptions{Store: h.tokens})
	if err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	h.store, err = deviceauth.New(deviceauth.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		t.Fatal(err)
	}
	h.cache, err = leases.NewCache(leases.CacheOptions{
		Writer: db.Writer(), Reader: db.Reader(), Events: eventLog,
		Siblings: keySet{keys: map[string]ed25519.PublicKey{
			h.idA: h.keyA.Public().(ed25519.PublicKey), h.idB: h.keyB.Public().(ed25519.PublicKey),
		}},
		Honoured: h.honoured,
		SelfID:   h.selfID, SelfKey: selfPub,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.spy = &spyLeases{inner: h.cache}

	var mgmt httpapi.ManagementAuthorizer
	if opts.mgmt != nil {
		mgmt = opts.mgmt
	}
	h.srv, err = httpapi.New(httpapi.Options{
		Config:               cfg,
		Logger:               slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		DB:                   db,
		Verifier:             verifier,
		DeviceVerifier:       h.store,
		SessionValidator:     opts.sessions,
		ManagementAuthorizer: mgmt,
		DeviceWriteLeases:    h.spy,
		Events:               eventLog,
		Build:                buildinfo.Info{Version: "test", Commit: "abc123", Date: "2026-08-20T00:00:00Z"},
		SchemaVersion:        1,
		KnownSchemaVersion:   1,
		CASRoot:              casDir,
		Mount:                []httpapi.MountFunc{testRoutes, whoamiRoute},
		Now:                  h.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.ts = httptest.NewServer(h.srv.Handler())
	t.Cleanup(h.ts.Close)

	h.enrolDevice(t)
	pubE, _, _ := ed25519.GenerateKey(nil)
	h.devE = identity.FormatPublicKey(pubE)
	return h
}

// whoamiRoute is a WRITE route that reports which sibling's lease, if any,
// lifted the caller: how a test sees what the access log and audit see.
func whoamiRoute(r chi.Router) {
	r.With(httpapi.RequireScope(auth.ScopeWrite)).Post("/whoami", func(w http.ResponseWriter, r *http.Request) {
		id, _ := httpapi.IdentityFrom(r.Context())
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(id.LeaseIssuer))
	})
}

// enrolDevice pins a user at S and enrols device D under it, with a cert issued
// at the harness's base time so the clock can move a day without it lapsing.
func (h *liftHarness) enrolDevice(t *testing.T) {
	t.Helper()
	u, userPriv, err := enrolment.GenerateUserIdentity()
	if err != nil {
		t.Fatal(err)
	}
	pubD, privD, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := enrolment.SignCert(userPriv, pubD, "", h.base, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := h.store.EnrolUser(ctx, u.UserID(), "alice", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.EnrolDevice(ctx, cert, "phone"); err != nil {
		t.Fatal(err)
	}
	h.devD, h.privD, h.certD = identity.FormatPublicKey(pubD), privD, cert
}

// credential is D's Device credential with a possession proof minted at the
// server's current clock.
func (h *liftHarness) credential(t *testing.T) string {
	t.Helper()
	proof, err := enrolment.SignPossession(h.privD, h.certD, h.clock.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return "Device " + h.certD + "~" + proof
}

// lease signs a lease as `by`, issued at the harness's base time for a day.
func (h *liftHarness) lease(t *testing.T, by ed25519.PrivateKey, principal, resource string, caps ...grant.Capability) string {
	t.Helper()
	tok, err := grant.Grant{
		Issuer: identity.FormatPublicKey(by.Public().(ed25519.PublicKey)), Principal: principal, Resource: resource,
		Capabilities: caps, IssuedAt: h.base, ExpiresAt: h.base.Add(leases.DeviceWriteTTL),
	}.Sign(by)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// writeLease is the lease the issuing beat at A mints for D: device D may write
// at site S.
func (h *liftHarness) writeLease(t *testing.T, by ed25519.PrivateKey) string {
	return h.lease(t, by, leases.DevicePrincipal(h.devD), leases.SiteResource(h.selfID), grant.CapabilityWrite)
}

// cacheFrom puts tokens in S's cache as served by `source`.
func (h *liftHarness) cacheFrom(t *testing.T, source string, tokens ...string) {
	t.Helper()
	if _, err := h.cache.Replace(context.Background(), source, tokens, h.base); err != nil {
		t.Fatal(err)
	}
}

func (h *liftHarness) do(t *testing.T, method, path, authHeader string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, h.ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// checks reads heyarr_http_device_lease_checks_total by outcome.
func (h *liftHarness) checks(t *testing.T) map[string]float64 {
	t.Helper()
	families, err := h.srv.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	for _, f := range families {
		if f.GetName() != "heyarr_http_device_lease_checks_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "outcome" {
					out[l.GetValue()] += m.GetCounter().GetValue()
				}
			}
		}
	}
	return out
}

// honour makes sibling A the one issuer the cache honours.
func (h *liftHarness) honour(t *testing.T) {
	t.Helper()
	h.setHonoured(keySet{keys: map[string]ed25519.PublicKey{h.idA: h.keyA.Public().(ed25519.PublicKey)}})
}

func (h *liftHarness) setHonoured(k leases.SiblingKeys) {
	h.honoured.mu.Lock()
	defer h.honoured.mu.Unlock()
	h.honoured.inner = k
}

// The positive case and its control, with the issuer nowhere: a valid lease from
// an opted-in sibling lifts D to write and is recorded as a lease lift naming
// the issuer; with no lease cached the same write is 403 and the read is 200.
func TestALeaseFromAnOptedInSiblingLiftsADeviceToWrite(t *testing.T) {
	t.Parallel()
	h := newLiftHarness(t, liftOptions{optIn: []string{"peer-a-id"}, mgmt: stubMgmt{}})
	h.honour(t)

	// No lease yet: the floor. The read works, the write is 403, never 401.
	if code, _ := h.do(t, http.MethodGet, "/api/v1/probe", h.credential(t)); code != http.StatusOK {
		t.Fatalf("read without a lease = %d, want 200", code)
	}
	if code, _ := h.do(t, http.MethodPost, "/api/v1/probe", h.credential(t)); code != http.StatusForbidden {
		t.Fatalf("write without a lease = %d, want 403", code)
	}
	if got := h.checks(t)["no_lease"]; got != 2 {
		t.Fatalf("no_lease checks = %v, want 2 (one per request)", got)
	}

	h.cacheFrom(t, h.idA, h.writeLease(t, h.keyA))
	code, issuer := h.do(t, http.MethodPost, "/api/v1/whoami", h.credential(t))
	if code != http.StatusCreated {
		t.Fatalf("write with an opted-in sibling's lease = %d, want 201", code)
	}
	if issuer != h.idA {
		t.Fatalf("identity.LeaseIssuer = %q, want A's key %q", issuer, h.idA)
	}
	if code, _ := h.do(t, http.MethodPost, "/api/v1/probe", h.credential(t)); code != http.StatusCreated {
		t.Fatalf("write route with the lease = %d, want 201", code)
	}
	if got := h.checks(t)["lifted"]; got != 2 {
		t.Fatalf("lifted checks = %v, want 2", got)
	}
	// Write, not admin: a lease lifts exactly one scope.
	if code, _ := h.do(t, http.MethodDelete, "/api/v1/probe", h.credential(t)); code != http.StatusForbidden {
		t.Fatalf("admin route with a lease = %d, want 403", code)
	}
	// The access log says the write came through a lease, and whose.
	if logs := h.logs.String(); !strings.Contains(logs, `"write_via":"lease"`) ||
		!strings.Contains(logs, `"lease_issuer":"`+h.idA+`"`) {
		t.Fatalf("the access log does not record the lease lift:\n%s", logs)
	}
}

// Each refusal keeps D on the read floor — the write is 403, the read 200,
// never a 401 — and is counted under its own name. The metric label is compared
// exactly, never by substring (ADR-0048: the reasons share words).
func TestEachLeaseRefusalKeepsTheFloorAndIsNamed(t *testing.T) {
	t.Parallel()
	refuseAt := func(h *liftHarness) time.Time { return h.base.Add(leases.DeviceWriteTTL - grant.SkewMargin) }
	tests := []struct {
		name    string
		tokens  func(t *testing.T, h *liftHarness) []string
		at      func(h *liftHarness) time.Time
		outcome string
	}{
		{
			name:    "expired: the clock is at expiry minus the skew margin",
			tokens:  func(t *testing.T, h *liftHarness) []string { return []string{h.writeLease(t, h.keyA)} },
			at:      refuseAt,
			outcome: "refused_expired",
		},
		{
			name: "principal_mismatch: a lease for device E, a request by device D",
			tokens: func(t *testing.T, h *liftHarness) []string {
				return []string{h.lease(t, h.keyA, leases.DevicePrincipal(h.devE), leases.SiteResource(h.selfID), grant.CapabilityWrite)}
			},
			outcome: "refused_principal_mismatch",
		},
		{
			name: "resource_mismatch: a lease for D at another site",
			tokens: func(t *testing.T, h *liftHarness) []string {
				return []string{h.lease(t, h.keyA, leases.DevicePrincipal(h.devD), leases.SiteResource(h.idB), grant.CapabilityWrite)}
			},
			outcome: "refused_resource_mismatch",
		},
		{
			name: "capability_denied: a lease for D here without write",
			tokens: func(t *testing.T, h *liftHarness) []string {
				return []string{h.lease(t, h.keyA, leases.DevicePrincipal(h.devD), leases.SiteResource(h.selfID), grant.CapabilityRead)}
			},
			outcome: "refused_capability_denied",
		},
		{
			name:    "unknown_issuer: sibling B is pinned but not in peer.lease_issuers",
			tokens:  func(t *testing.T, h *liftHarness) []string { return []string{h.writeLease(t, h.keyB)} },
			outcome: "refused_unknown_issuer",
		},
		{
			name: "unknown_issuer: a key that is not a member at all",
			tokens: func(t *testing.T, h *liftHarness) []string {
				_, stranger, _ := ed25519.GenerateKey(nil)
				return []string{h.writeLease(t, stranger)}
			},
			outcome: "refused_unknown_issuer",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newLiftHarness(t, liftOptions{optIn: []string{"peer-a-id"}})
			h.honour(t)
			h.cacheFrom(t, h.idA, tt.tokens(t, h)...)
			if tt.at != nil {
				h.clock.set(tt.at(h))
			}

			if code, _ := h.do(t, http.MethodPost, "/api/v1/probe", h.credential(t)); code != http.StatusForbidden {
				t.Fatalf("write = %d, want 403 (authenticated, on the floor)", code)
			}
			if code, _ := h.do(t, http.MethodGet, "/api/v1/probe", h.credential(t)); code != http.StatusOK {
				t.Fatalf("read = %d, want 200 (a refused lease never takes the floor away)", code)
			}
			got := h.checks(t)
			if got[tt.outcome] != 2 || len(got) != 1 {
				t.Fatalf("checks = %v, want exactly {%s: 2}", got, tt.outcome)
			}
		})
	}
}

// The cases where the lease check must not lift, or must not even run.
func TestTheLeaseCheckFailsClosedAndStaysInItsLane(t *testing.T) {
	t.Parallel()

	t.Run("an empty opt-in never lifts and never reads the cache, even with a valid lease", func(t *testing.T) {
		t.Parallel()
		h := newLiftHarness(t, liftOptions{optIn: nil})
		h.honour(t) // the cache WOULD honour A: only the config says no
		h.cacheFrom(t, h.idA, h.writeLease(t, h.keyA))
		if code, _ := h.do(t, http.MethodPost, "/api/v1/probe", h.credential(t)); code != http.StatusForbidden {
			t.Fatalf("write with no opt-in = %d, want 403", code)
		}
		if n := h.spy.calls.Load(); n != 0 {
			t.Fatalf("the lease cache was consulted %d times with no opt-in, want 0", n)
		}
		if got := h.checks(t); len(got) != 0 {
			t.Fatalf("checks = %v, want none: off means off", got)
		}
	})

	t.Run("a cache error keeps the read floor and is never a 401", func(t *testing.T) {
		t.Parallel()
		h := newLiftHarness(t, liftOptions{optIn: []string{"peer-a-id"}})
		h.setHonoured(keySet{err: errors.New("membership is unreadable")})
		h.cacheFrom(t, h.idA, h.writeLease(t, h.keyA))
		if code, _ := h.do(t, http.MethodPost, "/api/v1/probe", h.credential(t)); code != http.StatusForbidden {
			t.Fatalf("write when the check fails = %d, want 403", code)
		}
		if code, _ := h.do(t, http.MethodGet, "/api/v1/probe", h.credential(t)); code != http.StatusOK {
			t.Fatalf("read when the check fails = %d, want 200", code)
		}
		if got := h.checks(t); got["error"] != 2 || len(got) != 1 {
			t.Fatalf("checks = %v, want exactly {error: 2}", got)
		}
	})

	t.Run("a web-login session never lifts, even for a device holding a lease", func(t *testing.T) {
		t.Parallel()
		const token = "session-token-lease"
		sessions := stubSessions{}
		h := newLiftHarness(t, liftOptions{optIn: []string{"peer-a-id"}, sessions: sessions})
		sessions[token] = httpapi.SessionPrincipal{UserID: "ed25519:aa", DeviceKey: h.devD}
		h.honour(t)
		h.cacheFrom(t, h.idA, h.writeLease(t, h.keyA))
		if code, _ := h.do(t, http.MethodGet, "/api/v1/probe", "Bearer "+token); code != http.StatusOK {
			t.Fatalf("session read = %d, want 200", code)
		}
		if code, _ := h.do(t, http.MethodPost, "/api/v1/probe", "Bearer "+token); code != http.StatusForbidden {
			t.Fatalf("session write = %d, want 403", code)
		}
		if n := h.spy.calls.Load(); n != 0 {
			t.Fatalf("a session consulted the lease cache %d times, want 0", n)
		}
	})

	t.Run("a bearer token never consults the lease cache", func(t *testing.T) {
		t.Parallel()
		h := newLiftHarness(t, liftOptions{optIn: []string{"peer-a-id"}})
		h.honour(t)
		h.cacheFrom(t, h.idA, h.writeLease(t, h.keyA))
		created, err := h.tokens.Create(context.Background(), "reader", []auth.Scope{auth.ScopeRead}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if code, _ := h.do(t, http.MethodPost, "/api/v1/probe", "Bearer "+created.Secret); code != http.StatusForbidden {
			t.Fatalf("read token write = %d, want 403", code)
		}
		if n := h.spy.calls.Load(); n != 0 {
			t.Fatalf("a bearer token consulted the lease cache %d times, want 0", n)
		}
	})

	t.Run("a device with a management grant writes without consulting the lease cache", func(t *testing.T) {
		t.Parallel()
		mgmt := stubMgmt{}
		h := newLiftHarness(t, liftOptions{optIn: []string{"peer-a-id"}, mgmt: mgmt})
		mgmt[h.devD] = true
		h.honour(t)
		h.cacheFrom(t, h.idA, h.writeLease(t, h.keyA))
		code, issuer := h.do(t, http.MethodPost, "/api/v1/whoami", h.credential(t))
		if code != http.StatusCreated {
			t.Fatalf("granted device write = %d, want 201", code)
		}
		if issuer != "" {
			t.Fatalf("a granted device was recorded as lease-lifted by %q", issuer)
		}
		if n := h.spy.calls.Load(); n != 0 {
			t.Fatalf("a granted device consulted the lease cache %d times, want 0", n)
		}
		if got := h.checks(t); len(got) != 0 {
			t.Fatalf("checks = %v, want none", got)
		}
	})

	t.Run("a lease is never read from the request: an unverified device is 401 whatever is cached", func(t *testing.T) {
		t.Parallel()
		h := newLiftHarness(t, liftOptions{optIn: []string{"peer-a-id"}})
		h.honour(t)
		lease := h.writeLease(t, h.keyA)
		h.cacheFrom(t, h.idA, lease)
		// The lease itself, presented as a credential, authenticates nobody.
		for _, header := range []string{"Bearer " + lease, "Device " + lease} {
			if code, _ := h.do(t, http.MethodPost, "/api/v1/probe", header); code != http.StatusUnauthorized {
				t.Fatalf("%.6s<lease> = %d, want 401", header, code)
			}
		}
		if n := h.spy.calls.Load(); n != 0 {
			t.Fatalf("an unauthenticated request consulted the lease cache %d times, want 0", n)
		}
	})
}
