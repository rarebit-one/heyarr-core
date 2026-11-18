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

// A rename changes the label and nothing that hangs off the id (ADR-0124). The
// seeded films library holds two assets and one root; after the rename the
// same id carries the new name, the root and assets are still its own, and the
// change is on the log with both names.
func TestRenameLibraryKeepsItsIdRootsAndAssets(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed()

	resp := h.doStable(http.MethodPatch, "/api/v1/libraries/"+libFilmsID,
		strings.NewReader(`{"name":"  Movies  "}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rename = %d: %s", resp.StatusCode, h.body(resp))
	}
	var got struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Roots []struct {
			ID string `json:"id"`
		} `json:"roots"`
	}
	if err := json.Unmarshal(h.body(resp), &got); err != nil {
		t.Fatalf("decoding the renamed library: %v", err)
	}
	if got.ID != libFilmsID || got.Name != "Movies" {
		t.Errorf("renamed library = %+v, want id %s name Movies (trimmed)", got, libFilmsID)
	}
	if len(got.Roots) != 1 || got.Roots[0].ID != rootID {
		t.Errorf("renamed library roots = %+v, want the seeded root %s", got.Roots, rootID)
	}

	if n := h.countRows(t, `SELECT count(*) FROM libraries WHERE id = ? AND name = 'Movies'`, libFilmsID); n != 1 {
		t.Error("the row does not carry the new name")
	}
	if n := h.countRows(t, `SELECT count(*) FROM assets WHERE library_id = ?`, libFilmsID); n != 2 {
		t.Errorf("%d assets under the renamed library, want the seeded 2", n)
	}
	if n := h.countRows(t, `SELECT count(*) FROM events WHERE type = ?
		AND json_extract(payload, '$.name') = 'Movies'
		AND json_extract(payload, '$.previous_name') = 'films'`, events.TypeLibraryRenamed); n != 1 {
		t.Errorf("emitted %d library.renamed carrying both names, want 1", n)
	}
}

// The name is validated as `library add` validates it: a name another library
// holds is a 409, and an empty, blank or absent one is a 400. content_type is
// not patchable. Every refusal leaves the row and the log alone.
func TestRenameLibraryRefusals(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed()

	cases := []struct {
		body string
		want int
	}{
		{`{"name":"books"}`, http.StatusConflict},
		{`{"name":""}`, http.StatusBadRequest},
		{`{"name":"   "}`, http.StatusBadRequest},
		{`{}`, http.StatusBadRequest},
		{`{"content_type":"series"}`, http.StatusBadRequest},
		{`{"name":"Movies","content_type":"series"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		resp := h.doStable(http.MethodPatch, "/api/v1/libraries/"+libFilmsID, strings.NewReader(tc.body))
		if resp.StatusCode != tc.want {
			t.Errorf("patch %s = %d, want %d", tc.body, resp.StatusCode, tc.want)
		}
	}
	if n := h.countRows(t, `SELECT count(*) FROM libraries WHERE id = ? AND name = 'films' AND content_type = 'movie'`, libFilmsID); n != 1 {
		t.Error("a refused patch changed the library anyway")
	}
	if n := h.eventsOfType(t, events.TypeLibraryRenamed); n != 0 {
		t.Errorf("refused patches emitted %d renames, want 0", n)
	}

	if got := h.doStable(http.MethodPatch, "/api/v1/libraries/01990000-0000-7000-8000-00000000nope",
		strings.NewReader(`{"name":"Movies"}`)).StatusCode; got != http.StatusNotFound {
		t.Errorf("renaming a missing library = %d, want 404", got)
	}
}

// Renaming to the name it already has is not a transition, so it succeeds and
// puts nothing on the log.
func TestRenameLibraryToItsOwnNameEmitsNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed()

	resp := h.doStable(http.MethodPatch, "/api/v1/libraries/"+libFilmsID, strings.NewReader(`{"name":"films"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("no-op rename = %d: %s", resp.StatusCode, h.body(resp))
	}
	if n := h.eventsOfType(t, events.TypeLibraryRenamed); n != 0 {
		t.Errorf("a no-op rename emitted %d events, want 0", n)
	}
}

// The rename needs `write`, like every other library change.
func TestRenameLibraryNeedsWriteScope(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withAuth).seed()
	readOnly := h.mint("reader", auth.ScopeRead).Secret

	resp := h.do(http.MethodPatch, "/api/v1/libraries/"+libFilmsID, readOnly, strings.NewReader(`{"name":"Movies"}`))
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("PATCH with a read token = %d, want 403", resp.StatusCode)
	}
}
