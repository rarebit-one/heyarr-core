package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	yaml "go.yaml.in/yaml/v3"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// ADR-0015: api/openapi.yaml is hand-written, and this test is the mechanism
// that keeps that from meaning "write both and hope".
//
// The routes are enumerated by walking the real router rather than from a list
// in this file. A hand-maintained list here would make this a test that one
// list matches another list the same person wrote at the same time — which
// passes forever and catches nothing. It also means a route added by any other
// mount, including one this test has never heard of, is checked the moment it
// is wired in.

const specPath = "../../api/openapi.yaml"

// openAPI is the fragment of the document this test needs. Parsing only the
// paths is deliberate: this test asserts route parity, and validating the whole
// schema here would make an unrelated schema mistake fail as a routing error.
type openAPI struct {
	Paths map[string]map[string]yaml.Node `yaml:"paths"`
}

// httpMethods are the keys under a path item that are operations. A path item
// may also carry `parameters`, `summary` and `$ref`, which are not routes.
var httpMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

// newTestController builds a controller and its server against a real database.
func newTestController(t *testing.T) *Controller {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.DataDir = dir
	cfg.Database.Path = filepath.Join(dir, "heyarr.db")
	cfg.CAS.Root = filepath.Join(dir, "cas")
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = ""
	return New(cfg, slog.New(slog.DiscardHandler))
}

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	c := newTestController(t)
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Options{Path: c.cfg.Database.Path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	blobStore, err := cas.OpenFS(c.cfg.CAS.Root)
	if err != nil {
		t.Fatal(err)
	}
	// No health tracker: this fixture asserts the OpenAPI surface, and
	// liveness recording is a side effect on the request path rather than a
	// route.
	srv, _, err := c.newServer(db, blobStore, 4, nil, "peer-under-test")
	if err != nil {
		t.Fatal(err)
	}
	return srv.Handler()
}

// noMembers is a trust root that admits nobody. The peer surface's routes are
// registered whether or not anyone is enrolled, and this test is about the
// route table rather than about who may reach it.
type noMembers struct{}

func (noMembers) Lookup(context.Context, []byte) (mtls.Peer, error) {
	return mtls.Peer{}, mtls.ErrNotAMember
}

// newTestPeerHandler builds the mTLS peer surface's router (M4-05).
//
// It is enumerated by the parity test alongside the client API because
// ADR-0015's rule is about routes Heyarr serves, not about routes that happen
// to be mounted on one particular listener. A second listener that the parity
// test did not walk would be a surface where an undocumented route is free —
// which is precisely the gap the whole mechanism exists to close.
func newTestPeerHandler(t *testing.T) http.Handler {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	material, err := mtls.NewMaterial(mtls.MaterialOptions{PrivateKey: priv, PeerID: "peer-under-test"})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := peerapi.New(peerapi.Options{
		Material: material, Members: noMembers{}, SelfPeerID: "peer-under-test",
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	return srv.Handler()
}

// allRoutes is every route this binary serves, on either listener.
func allRoutes(t *testing.T) map[string]bool {
	t.Helper()
	out := routeSet(t, newTestHandler(t))
	for route := range routeSet(t, newTestPeerHandler(t)) {
		out[route] = true
	}
	return out
}

// routeSet enumerates every registered route as "METHOD /path".
func routeSet(t *testing.T, h http.Handler) map[string]bool {
	t.Helper()
	router, ok := h.(chi.Routes)
	if !ok {
		t.Fatalf("the server handler is %T, which cannot be walked; the parity test would silently check nothing", h)
	}
	out := map[string]bool{}
	err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		out[method+" "+normalisePath(route)] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walking the router: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("the walk found no routes at all, so this test proves nothing")
	}
	return out
}

// normalisePath strips the trailing slash chi appends to a mounted subtree, so
// "/api/v1/works" and "/api/v1/works/" are one route rather than two.
func normalisePath(route string) string {
	if len(route) > 1 {
		route = strings.TrimSuffix(route, "/")
	}
	if route == "" {
		route = "/"
	}
	return route
}

func specSet(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(specPath))
	if err != nil {
		t.Fatalf("reading %s: %v (ADR-0015 requires it to exist)", specPath, err)
	}
	var doc openAPI
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing %s: %v", specPath, err)
	}
	out := map[string]bool{}
	for path, item := range doc.Paths {
		for method := range item {
			if !httpMethods[strings.ToLower(method)] {
				continue
			}
			out[strings.ToUpper(method)+" "+normalisePath(path)] = true
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s documents no operations at all, so this test proves nothing", specPath)
	}
	return out
}

func TestEveryRegisteredRouteIsDocumented(t *testing.T) {
	routes := allRoutes(t)
	documented := specSet(t)

	var undocumented []string
	for route := range routes {
		if !documented[route] {
			undocumented = append(undocumented, route)
		}
	}
	sort.Strings(undocumented)
	if len(undocumented) > 0 {
		t.Errorf("%d route(s) are served and not documented in %s:\n  %s\n"+
			"ADR-0015: the specification is hand-written and this is the only thing keeping it true.",
			len(undocumented), specPath, strings.Join(undocumented, "\n  "))
	}
}

func TestEveryDocumentedPathIsRegistered(t *testing.T) {
	routes := allRoutes(t)
	documented := specSet(t)

	var missing []string
	for route := range documented {
		if !routes[route] {
			missing = append(missing, route)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d operation(s) are documented in %s and not served:\n  %s\n"+
			"A specification that describes routes that do not exist is worse than none.",
			len(missing), specPath, strings.Join(missing, "\n  "))
	}
}

// dumpRoutes is not a test of anything; it exists so that adding a route and
// then running `go test -run TestRouteInventory -v ./internal/controller` tells
// you exactly what to write in the specification.
func TestRouteInventory(t *testing.T) {
	routes := allRoutes(t)
	list := make([]string, 0, len(routes))
	for r := range routes {
		list = append(list, r)
	}
	sort.Strings(list)
	t.Log("registered routes:\n" + strings.Join(list, "\n"))
	if testing.Verbose() {
		fmt.Println(strings.Join(list, "\n"))
	}
}
