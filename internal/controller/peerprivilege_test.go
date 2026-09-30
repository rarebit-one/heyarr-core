package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// A peer is not an admin (ADR-0033), asserted ROUTE BY ROUTE.
//
// The admin surface is discovered from the real router rather than typed out
// here. A hand-written list would be a list one person wrote next to the code
// they were writing, and the route it would miss is the one added next month —
// which is exactly the route an escalation would use. So this probes every
// registered /api/v1 route with a write-scoped token, treats "this token does
// not carry the admin scope" as the definition of the admin surface, and then
// re-runs each of those routes over a connection that presented a peer
// certificate.
//
// The floor below keeps the discovery honest: a discovery that found nothing
// would make every assertion in this file vacuous and the file would still
// pass.

// adminSurfaceFloor is what the admin surface is known to contain today. It is
// a MINIMUM, never the list under test: routes are discovered, and this only
// fails the discovery if it stops finding what everyone knows is there.
var adminSurfaceFloor = []string{
	"POST /api/v1/tokens",
	"GET /api/v1/tokens",
	"DELETE /api/v1/tokens/{id}",
	"POST /api/v1/peers",
	"DELETE /api/v1/peers/{id}",
}

// peerSurfaceHarness is a client API with the full mount list, authentication
// on, and an injectable answer to "did this connection present a peer
// certificate?".
//
// The extractor is injected because the client listener never asks for a
// client certificate, so the production extractor could only ever answer
// "no" here — and a test that could only exercise the "no" branch would be a
// test of nothing. What is injected is the SEAM the real TLS extractor fills
// (httpapi.PresentedPeerKey), not the decision: everything downstream of it is
// the production path.
type peerSurfaceHarness struct {
	server *httptest.Server
	tokens *auth.Store
	router chi.Routes
}

func newPeerSurfaceHarness(t *testing.T, presented httpapi.PresentedPeerKey) *peerSurfaceHarness {
	t.Helper()
	ctx := context.Background()
	c := newTestController(t)

	testdb.WriteMigrated(t, c.cfg.Database.Path)
	db, err := sqlite.Open(ctx, sqlite.Options{Path: c.cfg.Database.Path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	blobStore, err := cas.OpenFS(filepath.Clean(c.cfg.CAS.Root))
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.NewStore(auth.StoreOptions{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := auth.NewVerifier(auth.VerifierOptions{Store: tokens})
	if err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	members, err := membership.New(membership.Options{DB: db, Events: eventLog})
	if err != nil {
		t.Fatal(err)
	}
	identities, err := deviceauth.New(deviceauth.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		t.Fatal(err)
	}
	mounts, publicMounts, err := c.mounts(t.Context(), mountDeps{
		db: db, tokens: tokens, verifier: verifier, blobStore: blobStore, eventLog: eventLog,
		members: members, identities: identities, selfPeerID: "peer-under-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	known, err := sqlite.KnownSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}

	cfg := c.cfg
	// Authentication ON. The question is whether a peer credential can stand
	// in for an admin token, and with authentication off there is no token to
	// stand in for.
	cfg.HTTP.Auth.Enabled = true

	srv, err := httpapi.New(httpapi.Options{
		Config: cfg, Logger: slog.New(slog.DiscardHandler), DB: db, Verifier: verifier,
		Events: eventLog, Build: buildinfo.Info{Version: "test"},
		SchemaVersion: known, KnownSchemaVersion: known,
		Mount:            mounts,
		MountPublic:      publicMounts,
		PeerMembership:   allMembers{},
		PresentedPeerKey: presented,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := srv.Handler()
	router, ok := handler.(chi.Routes)
	if !ok {
		t.Fatalf("the server handler is %T and cannot be walked; the admin surface could not be discovered", handler)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &peerSurfaceHarness{server: ts, tokens: tokens, router: router}
}

// Two clients, because the two phases of this file want opposite things from a
// timeout.
//
// Discovery walks EVERY registered route, and some of them do not answer
// promptly: the event stream holds its connection open, and the renderer
// routes run a real SSDP sweep that takes seconds. It needs a short deadline
// so such a route is passed over rather than hanging the test. The assertions
// afterwards run against routes that are known to answer immediately, and
// they need a generous one — argon2id verification is deliberately expensive
// (ADR-0011) and the first request on a token pays for it, under -race, on a
// machine running the rest of the suite. A three-second deadline there is how
// this file failed in CI while passing alone.
var (
	probeClient  = &http.Client{Timeout: 5 * time.Second}
	assertClient = &http.Client{Timeout: 60 * time.Second}
)

// allMembers admits every key. It is the most permissive trust root there is:
// if a peer certificate could ever confer admin, this is the configuration in
// which it would.
type allMembers struct{}

func (allMembers) IsMember(context.Context, []byte) (bool, error) { return true, nil }

func (h *peerSurfaceHarness) token(t *testing.T, name string, scope auth.Scope) string {
	t.Helper()
	created, err := h.tokens.Create(context.Background(), name, []auth.Scope{scope}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return created.Secret
}

// do issues one request with the generous deadline and reads the whole body.
// Everything that asserts uses it.
func (h *peerSurfaceHarness) do(t *testing.T, method, route, token string) (int, string) {
	t.Helper()
	status, body, err := h.request(assertClient, method, route, token, true)
	if err != nil {
		t.Fatal(err)
	}
	return status, body
}

// probe issues one request with the short deadline, for the discovery walk.
//
// Only a 403 has its body read. The scope refusal that identifies the admin
// surface is a 403 with a short, finite body; any other status already says
// "not this", and reading on would wait out the deadline on a route that
// answers by streaming. It takes no *testing.T because the walk probes
// concurrently.
func (h *peerSurfaceHarness) probe(method, route, token string) (int, string, error) {
	return h.request(probeClient, method, route, token, false)
}

// request issues one request. The error is for a request that could not be
// built; a request that was sent but not answered is reported in the body.
func (h *peerSurfaceHarness) request(c *http.Client, method, route, token string, readAll bool) (int, string, error) {
	url := h.server.URL + fillParams(route)
	req, err := http.NewRequestWithContext(context.Background(), method, url, strings.NewReader("{}"))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		// A route that answers slowly (the renderer sweep) or by holding the
		// connection open before any header is written would hang this test
		// rather than fail it. A response that never arrives is not a refusal
		// and not an admin route: it is reported as neither, and the
		// discovery floor is what catches a day when that swallows something
		// real.
		return 0, "(no response: " + err.Error() + ")", nil
	}
	defer func() { _ = resp.Body.Close() }()
	if !readAll && resp.StatusCode != http.StatusForbidden {
		return resp.StatusCode, "(body not read)", nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// A streaming route sent headers and then kept the connection open.
		// Reported as neither a refusal nor an admin route.
		return 0, "(no body: " + err.Error() + ")", nil
	}
	return resp.StatusCode, string(body), nil
}

// fillParams substitutes a value that resolves to nothing for every path
// parameter. Every refusal under test happens in middleware, before a handler
// ever looks a resource up, so an id that matches no row is exactly right: a
// route that answers 404 got past the credential check, which is the outcome
// this file is watching for.
func fillParams(route string) string {
	out := make([]string, 0, 4)
	for _, seg := range strings.Split(route, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			seg = "no-such-id"
		}
		out = append(out, seg)
	}
	return strings.Join(out, "/")
}

// discovered memoises the admin surface across the tests in this file.
//
// Every harness here is built by newPeerSurfaceHarness over the same wiring,
// so the surface is a property of the router, not of the harness that
// happened to walk it. Walking it once is the whole saving: the walk has to
// wait out the probe deadline on the routes that do not answer promptly, and
// it would otherwise pay that again per test. A walk that fails leaves the
// cache empty, so the next test walks again rather than inheriting a failure.
var discovered struct {
	sync.Mutex
	routes []string
}

// adminRoutes discovers the admin surface by asking the router.
func adminRoutes(t *testing.T, h *peerSurfaceHarness) []string {
	t.Helper()
	discovered.Lock()
	defer discovered.Unlock()
	if discovered.routes == nil {
		discovered.routes = walkAdminRoutes(t, h)
	}
	return slices.Clone(discovered.routes)
}

func walkAdminRoutes(t *testing.T, h *peerSurfaceHarness) []string {
	t.Helper()
	writeToken := h.token(t, "discovery", auth.ScopeWrite)

	// Warm the token before the walk. Verification is memoised but the FIRST
	// use of a credential pays argon2id in full, and paying it inside a probe
	// with a short deadline would mis-read one route as "no response".
	if status, body := h.do(t, http.MethodGet, httpapi.APIPrefix+"/system", writeToken); status != http.StatusOK {
		t.Fatalf("the discovery token does not work at all (%d), so the walk below would find "+
			"nothing\n%s", status, body)
	}

	type probed struct {
		route  string
		status int
		body   string
		err    error
	}
	var (
		wg      sync.WaitGroup
		results []*probed
	)
	// Concurrently, so the routes that wait out the probe deadline wait it
	// out together rather than one after another.
	err := chi.Walk(h.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = normalisePath(route)
		if !strings.HasPrefix(route, httpapi.APIPrefix) {
			return nil
		}
		p := &probed{route: method + " " + route}
		results = append(results, p)
		wg.Go(func() {
			p.status, p.body, p.err = h.probe(method, route, writeToken)
		})
		return nil
	})
	wg.Wait()
	if err != nil {
		t.Fatalf("walking the router: %v", err)
	}

	var found []string
	for _, p := range results {
		if p.err != nil {
			t.Fatalf("probing %s: %v", p.route, p.err)
		}
		if p.status == http.StatusForbidden && strings.Contains(p.body, "does not carry the admin scope") {
			found = append(found, p.route)
		}
	}
	sort.Strings(found)

	for _, want := range adminSurfaceFloor {
		if !slices.Contains(found, want) {
			t.Fatalf("the admin surface discovery did not find %q. Everything in this file is "+
				"asserted against what it found, so a discovery that has stopped working would "+
				"make every assertion below vacuous.\nfound:\n  %s",
				want, strings.Join(found, "\n  "))
		}
	}
	return found
}

// TestAPeerCertificateIsRefusedOnEveryAdminRoute.
//
// The credential in the request is a real, valid, admin-scoped bearer token —
// the strongest client credential this system issues. The only thing that
// makes the request different from a legitimate admin call is that it arrived
// over a connection presenting a peer certificate, and that alone must refuse
// it: a peer authenticates AS THAT PEER and authorises the peer surface only.
func TestAPeerCertificateIsRefusedOnEveryAdminRoute(t *testing.T) {
	peerPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	// Two harnesses over identical wiring: one where every connection is a
	// peer connection, one where none is. The second is the control, and
	// without it a route that is simply broken would read as a refusal.
	onPeerConn := newPeerSurfaceHarness(t, func(*http.Request) ([]byte, bool) { return peerPub, true })
	onClientConn := newPeerSurfaceHarness(t, func(*http.Request) ([]byte, bool) { return nil, false })

	// Discovered on the CLIENT harness, where the admin routes still answer
	// with the scope refusal that identifies them. On the peer harness they
	// answer with the refusal under test, which would make the discovery
	// depend on the very behaviour it is meant to enumerate targets for.
	routes := adminRoutes(t, onClientConn)
	t.Logf("admin surface discovered from the router: %d route(s)\n  %s",
		len(routes), strings.Join(routes, "\n  "))

	peerAdmin := onPeerConn.token(t, "admin-over-peer-connection", auth.ScopeAdmin)
	plainAdmin := onClientConn.token(t, "admin", auth.ScopeAdmin)

	for _, route := range routes {
		method, path, ok := strings.Cut(route, " ")
		if !ok {
			t.Fatalf("unparsable route %q", route)
		}
		t.Run(route, func(t *testing.T) {
			status, body := onPeerConn.do(t, method, path, peerAdmin)
			if status != http.StatusForbidden {
				t.Fatalf("%s reached the admin surface over a peer certificate: %d. A peer "+
					"certificate is not an admin credential (ADR-0033)\n%s", route, status, body)
			}
			if !strings.Contains(body, "not an admin credential") {
				t.Fatalf("%s was refused for some other reason than the peer certificate:\n%s", route, body)
			}

			// The control, on the same route with the same token shape and no
			// peer certificate: it is NOT refused this way. Without it, a
			// route that 403s for everybody would pass above.
			status, body = onClientConn.do(t, method, path, plainAdmin)
			if strings.Contains(body, "not an admin credential") {
				t.Fatalf("%s was refused as a peer credential on a connection with no certificate "+
					"(%d) — the refusal is not about the peer certificate at all\n%s", route, status, body)
			}
			if status == http.StatusForbidden {
				t.Fatalf("%s refused a genuine admin token (%d), so the assertion above proves "+
					"nothing\n%s", route, status, body)
			}
		})
	}
}

// TestAPeerCertificateStillReachesTheNonAdminClientSurface.
//
// The refusal above must be a refusal of ESCALATION, not a blanket ban that
// happens to cover the admin routes. If a peer certificate refused everything,
// the test above would pass on a server that had simply stopped working, and
// `heyarr all` — where a request can be both — would be the casualty.
func TestAPeerCertificateStillReachesTheNonAdminClientSurface(t *testing.T) {
	peerPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h := newPeerSurfaceHarness(t, func(*http.Request) ([]byte, bool) { return peerPub, true })
	token := h.token(t, "reader", auth.ScopeRead)

	if status, body := h.do(t, http.MethodGet, "/api/v1/system", token); status != http.StatusOK {
		t.Fatalf("a read request on a peer connection was refused: %d\n%s", status, body)
	}
}

// TestTheAdminSurfaceIsNotRegisteredOnThePeerRouter is the structural half of
// the same rule: the escalation is refused on the client API, and the routes
// are not on the peer router to be reached in the first place.
//
// 404 rather than 401 is the assertion that matters. The peer router's
// NotFound runs before its identity middleware, so an unmatched path answers
// 404 while a REGISTERED one would answer 401 for want of a certificate —
// which makes 404 the evidence that no such route exists here.
func TestTheAdminSurfaceIsNotRegisteredOnThePeerRouter(t *testing.T) {
	client := newPeerSurfaceHarness(t, func(*http.Request) ([]byte, bool) { return nil, false })
	routes := adminRoutes(t, client)

	peerHandler := newTestPeerHandler(t)
	peerServer := httptest.NewServer(peerHandler)
	t.Cleanup(peerServer.Close)

	// The control: a route that IS registered on the peer router answers 401
	// here, not 404. It proves the 404s below are about the route table rather
	// than about this server refusing everything.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, peerServer.URL+"/peer/v1/attachment", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a registered peer route answered %d, want 401 — the 404s below would then mean "+
			"nothing", resp.StatusCode)
	}

	for _, route := range routes {
		method, path, ok := strings.Cut(route, " ")
		if !ok {
			t.Fatalf("unparsable route %q", route)
		}
		t.Run(route, func(t *testing.T) {
			req, err := http.NewRequestWithContext(context.Background(), method,
				peerServer.URL+fillParams(path), strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s is registered on the peer router (%d). The admin surface is reached "+
					"with an admin bearer token on the client listener, never with a peer "+
					"certificate (ADR-0033)", route, resp.StatusCode)
			}
		})
	}
}
