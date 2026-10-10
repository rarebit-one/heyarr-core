package controller

// Profile-routing tests (ADR-0107).
//
// Each test case builds a real controller (against a migrated test database),
// walks the chi router with chi.Walk, and asserts that exactly the expected
// routes are present or absent. This is table-driven because there are three
// interesting configurations:
//
//  1. Personal profile — only the personal-state plane, vault blob upload,
//     vault placement pins and blob content serving.
//  2. Media profile, vault.enabled=true (the default) — the full route table.
//  3. Media profile, vault.enabled=false — full table minus vault upload and
//     placement routes (blob content serving stays).
//
// The test intentionally shares infrastructure with openapi_test.go: it uses
// routeSet and normalisePath from that file (both unexported, same package).
// The fixture uses newTestHandler as a template.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"

	"log/slog"
)

// newProfileHandler builds a real server handler for the given config.  It
// follows newTestHandler's approach: real (in-memory) database, no health
// tracker, the router walked immediately after construction.
func newProfileHandler(t *testing.T, cfg config.Config) *Controller {
	t.Helper()
	dir := t.TempDir()
	cfg.DataDir = dir
	cfg.Database.Path = filepath.Join(dir, "heyarr.db")
	cfg.CAS.Root = filepath.Join(dir, "cas")
	cfg.HTTP.Addr = "127.0.0.1:0"
	cfg.HTTP.UnixSocket = ""
	return New(cfg, slog.New(slog.DiscardHandler))
}

// buildRouteSet migrates the database, opens it, and returns the route set
// for the given controller.  It is the profile-test analogue of newTestHandler.
func buildRouteSet(t *testing.T, c *Controller) map[string]bool {
	t.Helper()
	ctx := context.Background()
	testdb.WriteMigrated(t, c.cfg.Database.Path)
	db, err := sqlite.Open(ctx, sqlite.Options{Path: c.cfg.Database.Path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	blobStore, err := cas.OpenFS(c.cfg.CAS.Root)
	if err != nil {
		t.Fatal(err)
	}
	srv, _, err := c.newServer(t.Context(), db, blobStore, 4, nil, "peer-under-test", nil)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	return routeSet(t, srv.Handler())
}

// routeContaining returns the routes in rs that contain the given fragment.
func routeContaining(rs map[string]bool, fragment string) []string {
	var out []string
	for r := range rs {
		if strings.Contains(r, fragment) {
			out = append(out, r)
		}
	}
	return out
}

// TestProfileRouting is the table-driven test.
func TestProfileRouting(t *testing.T) {
	tests := []struct {
		name string
		cfg  func() config.Config

		// Routes that MUST be present (checked by path fragment).
		mustHave []string
		// Routes that MUST be absent (checked by path fragment).
		mustAbsent []string
	}{
		{
			name: "personal profile: has blob serving and vault routes",
			cfg: func() config.Config {
				cfg := config.MnemosyneDefaults()
				cfg.Vault.Enabled = true
				return cfg
			},
			mustHave: []string{
				"/api/v1/blobs",             // blob content GET/HEAD
				"/api/v1/vault/blobs",       // vault blob upload (PUT)
				"/api/v1/vault/placements",  // placement pins
				"/api/v1/spaces",            // personal-state plane
				"/enrol",                    // device enrolment
				"/healthz",                  // health
				"/metrics",                  // metrics
				"/readyz",                   // readiness
			},
			mustAbsent: []string{
				"/api/v1/works",    // library (media profile only)
				"/api/v1/assets",   // assets (media profile only)
				"/api/v1/jobs",     // scheduler (media profile only)
				"/compat/subsonic", // compat adapters (media profile only)
				"/compat/opds",     // compat adapters (media profile only)
			},
		},
		{
			name: "media profile vault.enabled=true: has full route table",
			cfg: func() config.Config {
				cfg := config.Defaults()
				cfg.Vault.Enabled = true
				return cfg
			},
			mustHave: []string{
				"/api/v1/blobs",            // blob content serving
				"/api/v1/vault/blobs",      // vault blob upload
				"/api/v1/vault/placements", // placement pins
				"/api/v1/spaces",           // personal-state plane
				"/api/v1/works",            // library
				"/api/v1/assets",           // assets
				"/enrol",                   // device enrolment
				"/healthz",
				"/readyz",
				"/metrics",
			},
			mustAbsent: nil,
		},
		{
			name: "media profile vault.enabled=false: removes vault upload and placement",
			cfg: func() config.Config {
				cfg := config.Defaults()
				cfg.Vault.Enabled = false
				return cfg
			},
			mustHave: []string{
				"/api/v1/blobs",  // blob content serving STAYS
				"/api/v1/spaces", // personal-state plane stays
				"/api/v1/works",  // library stays
				"/enrol",
				"/healthz",
				"/readyz",
				"/metrics",
			},
			mustAbsent: []string{
				"/api/v1/vault/blobs",      // upload gone
				"/api/v1/vault/placements", // placement gone
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newProfileHandler(t, tc.cfg())
			routes := buildRouteSet(t, c)

			for _, fragment := range tc.mustHave {
				found := routeContaining(routes, fragment)
				if len(found) == 0 {
					t.Errorf("profile %q: expected a route containing %q but found none\nall routes: %v",
						c.cfg.Profile, fragment, routeList(routes))
				}
			}
			for _, fragment := range tc.mustAbsent {
				found := routeContaining(routes, fragment)
				if len(found) > 0 {
					t.Errorf("profile %q: expected no route containing %q but found: %v",
						c.cfg.Profile, fragment, found)
				}
			}
		})
	}
}

// routeList returns the route set as a sorted slice, for diagnostic output.
func routeList(rs map[string]bool) []string {
	out := make([]string, 0, len(rs))
	for r := range rs {
		out = append(out, r)
	}
	// sort for determinism
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
