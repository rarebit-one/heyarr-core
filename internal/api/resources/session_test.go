//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// seedSeries inserts a series work and (optionally) its stored tvdb external id,
// so a content-intent search has something to surface a feed identity for
// (ADR-0050, decision 2).
func seedSeries(h *harness, id, title, tvdbID string) {
	h.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, year, created_at, updated_at)
		VALUES (?, 'series', ?, ?, ?, 2016, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		id, id, title, strings.ToLower(title))
	if tvdbID != "" {
		h.exec(`INSERT INTO external_ids (id, entity_type, entity_id, source, value)
			VALUES (?, 'work', ?, 'tvdb', ?)`, "ext-"+id, id, tvdbID)
	}
}

// A content-intent search surfaces the stored tvdb id of a work that has one, so
// a client can follow the hit in one step, and omits it for a work that has none.
func TestSearchSurfacesStoredTVDBID(t *testing.T) {
	h := newHarness(t)
	seedSeries(h, "w-tvdb", "Follow Me Home", "424242")
	seedSeries(h, "w-none", "Follow Me Away", "")

	resp := h.doStable(http.MethodPost, "/api/v1/search", strings.NewReader(`{"query":"follow me"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search = %d", resp.StatusCode)
	}
	raw := h.body(resp) // the response stream is single-read; capture it once
	var out struct {
		Works []struct {
			WorkID string `json:"work_id"`
			TVDBID string `json:"tvdb_id"`
		} `json:"works"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, w := range out.Works {
		got[w.WorkID] = w.TVDBID
	}
	if got["w-tvdb"] != "424242" {
		t.Errorf("a work with a stored tvdb id must surface it, got %q", got["w-tvdb"])
	}
	if _, ok := got["w-none"]; !ok {
		t.Fatal("the second series was not returned by the search")
	}
	if got["w-none"] != "" {
		t.Errorf("a work with no stored tvdb id must omit the field, got %q", got["w-none"])
	}

	// The raw body omits the key entirely for the tvdb-less work (omitempty),
	// rather than sending an empty string a client would have to special-case.
	if strings.Count(string(raw), `"tvdb_id"`) != 1 {
		t.Errorf("exactly one work should carry a tvdb_id field:\n%s", raw)
	}
}

// GET /session reports the caller's own authority. Under the default (auth
// disabled → anonymous admin) harness it reports the anonymous kind and write
// authority — enough to prove the shape a client reads.
func TestSessionIntrospection(t *testing.T) {
	h := newHarness(t)
	resp := h.get("/api/v1/session")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /session = %d: %s", resp.StatusCode, h.body(resp))
	}
	var out struct {
		Kind     string   `json:"kind"`
		Scopes   []string `json:"scopes"`
		CanWrite bool     `json:"can_write"`
	}
	if err := json.Unmarshal(h.body(resp), &out); err != nil {
		t.Fatal(err)
	}
	if out.Kind != "anonymous" {
		t.Errorf("kind = %q, want anonymous under disabled auth", out.Kind)
	}
	if !out.CanWrite {
		t.Error("the anonymous admin identity should report can_write")
	}
}

// The management-grant surface round-trips: issue a grant, see it listed,
// re-issue it idempotently, then revoke it (and a revoke of nothing is a 404).
func TestManagementGrantEndpoints(t *testing.T) {
	h := newHarness(t)

	create := h.do(http.MethodPost, "/api/v1/session/management-grants", "",
		strings.NewReader(`{"device_key":"ed25519:phone","reason":"the operator's phone"}`))
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("POST grant = %d: %s", create.StatusCode, h.body(create))
	}
	if loc := create.Header.Get("Location"); !strings.HasSuffix(loc, "/session/management-grants/ed25519:phone") {
		t.Errorf("Location = %q, want it to name the grant", loc)
	}

	list := h.get("/api/v1/session/management-grants")
	var listed struct {
		Grants []struct {
			DeviceKey string `json:"device_key"`
			Reason    string `json:"reason"`
		} `json:"management_grants"`
	}
	if err := json.Unmarshal(h.body(list), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Grants) != 1 || listed.Grants[0].DeviceKey != "ed25519:phone" {
		t.Fatalf("grant not listed: %+v", listed.Grants)
	}

	// A missing device_key is a 400, named.
	bad := h.do(http.MethodPost, "/api/v1/session/management-grants", "", strings.NewReader(`{}`))
	if bad.StatusCode != http.StatusBadRequest {
		t.Errorf("a grant with no device_key = %d, want 400", bad.StatusCode)
	}

	// Re-granting is idempotent — still one row.
	if again := h.do(http.MethodPost, "/api/v1/session/management-grants", "",
		strings.NewReader(`{"device_key":"ed25519:phone"}`)); again.StatusCode != http.StatusCreated {
		t.Errorf("re-grant = %d, want 201", again.StatusCode)
	}
	if n := h.countRows(t, `SELECT COUNT(*) FROM session_management_grants`); n != 1 {
		t.Errorf("re-granting must not add a row, got %d", n)
	}

	// Revoke it.
	del := h.do(http.MethodDelete, "/api/v1/session/management-grants/ed25519:phone", "", nil)
	if del.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE grant = %d: %s", del.StatusCode, h.body(del))
	}
	// Revoking a device that is not granted is a 404.
	gone := h.do(http.MethodDelete, "/api/v1/session/management-grants/ed25519:phone", "", nil)
	if gone.StatusCode != http.StatusNotFound {
		t.Errorf("revoking an ungranted device = %d, want 404", gone.StatusCode)
	}
}
