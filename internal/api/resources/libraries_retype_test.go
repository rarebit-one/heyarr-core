//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

const libLegacyID = "01990000-0000-7000-8000-0000000000l9"

// seedLegacy adds a library created before content types were checked: `show`,
// with no roots, holding one of the seeded editions' assets.
func (h *harness) seedLegacy() *harness {
	h.t.Helper()
	h.exec(`INSERT INTO libraries (id, name, content_type, enabled, created_at)
		VALUES (?, 'tv', 'show', 1, ?)`, libLegacyID, seedTime)
	h.exec(`UPDATE assets SET library_id = ? WHERE id = ?`, libLegacyID, asset3ID)
	return h
}

// An unknown type becomes a known one in place (#896, ADR-0127): the same id,
// the same assets, a new type, and the change on the log with both types.
func TestRetypeLibraryFromAnUnknownType(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedLegacy()

	resp := h.doStable(http.MethodPost, "/api/v1/libraries/"+libLegacyID+"/retype",
		strings.NewReader(`{"content_type":" series "}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retype = %d: %s", resp.StatusCode, h.body(resp))
	}
	var got struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		ContentType string `json:"content_type"`
		Roots       []any  `json:"roots"`
	}
	if err := json.Unmarshal(h.body(resp), &got); err != nil {
		t.Fatalf("decoding the retyped library: %v", err)
	}
	if got.ID != libLegacyID || got.Name != "tv" || got.ContentType != "series" || len(got.Roots) != 0 {
		t.Errorf("retyped library = %+v, want id %s, tv, series (trimmed), no roots", got, libLegacyID)
	}
	if n := h.countRows(t, `SELECT count(*) FROM assets WHERE library_id = ?`, libLegacyID); n != 1 {
		t.Errorf("%d assets under the retyped library, want 1", n)
	}
	if n := h.countRows(t, `SELECT count(*) FROM events WHERE type = ?
		AND json_extract(payload, '$.library_id') = ?
		AND json_extract(payload, '$.content_type') = 'series'
		AND json_extract(payload, '$.previous_content_type') = 'show'`, events.TypeLibraryRetyped, libLegacyID); n != 1 {
		t.Errorf("emitted %d library.retyped carrying both types, want 1", n)
	}
}

// A known type is not retyped (409), an unknown target is refused (400), a
// missing library is a 404, and the body is checked as strictly as any other.
// Every refusal leaves the rows and the log alone.
func TestRetypeLibraryRefusals(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedLegacy()

	cases := []struct {
		name, id, body string
		want           int
		detail         string
	}{
		{"known to known", libFilmsID, `{"content_type":"series"}`, http.StatusConflict, "already movie"},
		{"unknown to unknown", libLegacyID, `{"content_type":"tv"}`, http.StatusBadRequest, "use one of: movie, series"},
		{"known to unknown", libFilmsID, `{"content_type":"show"}`, http.StatusBadRequest, `show\" is not one Heyarr understands`},
		{"empty", libLegacyID, `{"content_type":""}`, http.StatusBadRequest, "content_type"},
		{"absent", libLegacyID, `{}`, http.StatusBadRequest, "content_type"},
		{"unknown field", libLegacyID, `{"content_type":"series","name":"shows"}`, http.StatusBadRequest, ""},
		{"missing library", "01990000-0000-7000-8000-00000000nope", `{"content_type":"series"}`, http.StatusNotFound, "no library"},
	}
	for _, tc := range cases {
		resp := h.doStable(http.MethodPost, "/api/v1/libraries/"+tc.id+"/retype", strings.NewReader(tc.body))
		body := string(h.body(resp))
		if resp.StatusCode != tc.want {
			t.Errorf("%s: retype %s = %d, want %d: %s", tc.name, tc.body, resp.StatusCode, tc.want, body)
		}
		if !strings.Contains(body, tc.detail) {
			t.Errorf("%s: problem %s does not contain %q", tc.name, body, tc.detail)
		}
	}
	if n := h.countRows(t, `SELECT count(*) FROM libraries WHERE id = ? AND content_type = 'show'`, libLegacyID); n != 1 {
		t.Error("a refused retype changed the legacy library")
	}
	if n := h.countRows(t, `SELECT count(*) FROM libraries WHERE id = ? AND content_type = 'movie'`, libFilmsID); n != 1 {
		t.Error("a refused retype changed the films library")
	}
	if n := h.eventsOfType(t, events.TypeLibraryRetyped); n != 0 {
		t.Errorf("refused retypes emitted %d events, want 0", n)
	}
}

// Retyping to the type a library already has is not a transition — including
// a known type that was never unknown — so it succeeds and emits nothing. A
// second run of the operator's command is therefore not an error.
func TestRetypeLibraryToItsOwnTypeEmitsNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed().seedLegacy()

	for _, id := range []string{libLegacyID, libLegacyID} {
		resp := h.doStable(http.MethodPost, "/api/v1/libraries/"+id+"/retype", strings.NewReader(`{"content_type":"series"}`))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("retype = %d: %s", resp.StatusCode, h.body(resp))
		}
	}
	resp := h.doStable(http.MethodPost, "/api/v1/libraries/"+libFilmsID+"/retype", strings.NewReader(`{"content_type":"movie"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retype films to movie = %d: %s", resp.StatusCode, h.body(resp))
	}
	if n := h.eventsOfType(t, events.TypeLibraryRetyped); n != 1 {
		t.Errorf("one real retype and two no-ops emitted %d events, want 1", n)
	}
}

// The retype needs `write`, like every other library change.
func TestRetypeLibraryNeedsWriteScope(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withAuth).seed().seedLegacy()
	readOnly := h.mint("reader", auth.ScopeRead).Secret
	writer := h.mint("writer", auth.ScopeWrite).Secret

	path := "/api/v1/libraries/" + libLegacyID + "/retype"
	if got := h.do(http.MethodPost, path, readOnly, strings.NewReader(`{"content_type":"series"}`)).StatusCode; got != http.StatusForbidden {
		t.Errorf("retype with a read token = %d, want 403", got)
	}
	if got := h.do(http.MethodPost, path, "", strings.NewReader(`{"content_type":"series"}`)).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("retype with no token = %d, want 401", got)
	}
	if got := h.do(http.MethodPost, path, writer, strings.NewReader(`{"content_type":"series"}`)).StatusCode; got != http.StatusOK {
		t.Errorf("retype with a write token = %d, want 200", got)
	}
}
