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

// Editing and removing a work (#428).

func (h *harness) patchWork(t *testing.T, id, body string) *http.Response {
	t.Helper()
	return h.doStable(http.MethodPatch, "/api/v1/works/"+id, strings.NewReader(body))
}

// A correction changes what a person reads and the order they read it in, and
// leaves the identity a rescan converges on alone.
func TestPatchWorkCorrectsTheDisplayFactsNotTheIdentity(t *testing.T) {
	h := newHarness(t).seed()

	before := h.work(t, work1ID)
	resp := h.patchWork(t, work1ID, `{"title":"Story of Your Life","year":2017}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d: %s", resp.StatusCode, h.body(resp))
	}
	var got workDoc
	if err := json.Unmarshal(h.body(resp), &got); err != nil {
		t.Fatal(err)
	}
	if got.Title != "Story of Your Life" {
		t.Errorf("title = %q, want the corrected one", got.Title)
	}
	if got.Year == nil || *got.Year != 2017 {
		t.Errorf("year = %v, want 2017", got.Year)
	}
	if got.SortTitle == before.SortTitle {
		t.Errorf("sort_title stayed %q after a rename; the listing would sort it wrong",
			got.SortTitle)
	}
	// The identity a rescan converges on must NOT move, or the next scan
	// creates a second work rather than finding this one.
	if got.WorkKey != before.WorkKey {
		t.Errorf("work_key changed from %q to %q", before.WorkKey, got.WorkKey)
	}
	if n := h.eventsOfType(t, events.TypeWorkUpdated); n != 1 {
		t.Errorf("emitted %d work.updated events, want 1", n)
	}
}

// Every patch field is optional, and omitting one leaves it alone — the
// distinction a plain (non-pointer) body would lose.
func TestPatchWorkLeavesOmittedFieldsAlone(t *testing.T) {
	h := newHarness(t).seed()

	if resp := h.patchWork(t, work1ID, `{"title":"Arrival"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d", resp.StatusCode)
	}
	got := h.work(t, work1ID)
	if got.Year == nil || *got.Year != 2016 {
		t.Errorf("year = %v, want the untouched 2016", got.Year)
	}
	if got.ContentType != "movie" {
		t.Errorf("content_type = %q, want the untouched movie", got.ContentType)
	}
}

// A year of 0 is "there is no year", which is a different request from omitting
// the field.
func TestPatchWorkClearsAYear(t *testing.T) {
	h := newHarness(t).seed()

	if resp := h.patchWork(t, work1ID, `{"year":0}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d", resp.StatusCode)
	}
	if got := h.work(t, work1ID); got.Year != nil {
		t.Errorf("year = %v, want null", *got.Year)
	}
}

func TestPatchWorkRefusesNonsense(t *testing.T) {
	h := newHarness(t).seed()

	cases := []struct{ name, body string }{
		{"nothing to change", `{}`},
		{"a blank title", `{"title":"   "}`},
		{"a year that is not one", `{"year":90210}`},
		{"a content type this node does not know", `{"content_type":"hologram"}`},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := h.patchWork(t, work1ID, tt.body).StatusCode; got != http.StatusBadRequest {
				t.Errorf("patch %s = %d, want 400", tt.body, got)
			}
		})
	}
	if got := h.patchWork(t, "nope", `{"title":"x"}`).StatusCode; got != http.StatusNotFound {
		t.Errorf("patching an unknown work = %d, want 404", got)
	}
}

// The delete is ADR-0018 logical: the catalog rows go, and not one byte is
// unlinked — the blobs stay for the GC sweeper to reclaim behind its grace
// window.
func TestDeleteWorkRemovesTheCatalogRowsAndNoBytes(t *testing.T) {
	h := newHarness(t).seed()

	resp := h.doStable(http.MethodDelete, "/api/v1/works/"+work1ID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", resp.StatusCode, h.body(resp))
	}

	if n := h.countRows(t, `SELECT count(*) FROM works WHERE id = ?`, work1ID); n != 0 {
		t.Errorf("the work is still there")
	}
	if n := h.countRows(t, `SELECT count(*) FROM editions WHERE work_id = ?`, work1ID); n != 0 {
		t.Errorf("%d editions survived the work", n)
	}
	if n := h.countRows(t, `SELECT count(*) FROM assets WHERE id = ?`, asset1ID); n != 0 {
		t.Errorf("the asset survived the work")
	}
	if n := h.countRows(t, `SELECT count(*) FROM desired_items WHERE work_id = ?`, work1ID); n != 0 {
		t.Errorf("%d wants survived the work they wanted", n)
	}
	// The whole point: the bytes are untouched.
	if n := h.countRows(t, `SELECT count(*) FROM blobs WHERE hash = ?`, blob1Hash); n != 1 {
		t.Errorf("the blob was removed with the catalog row — ADR-0018 says never")
	}

	if got := h.get("/api/v1/works/" + work1ID).StatusCode; got != http.StatusNotFound {
		t.Errorf("the deleted work still reads as %d", got)
	}
	if got := h.doStable(http.MethodDelete, "/api/v1/works/"+work1ID, nil).StatusCode; got != http.StatusNotFound {
		t.Errorf("deleting it twice = %d, want 404", got)
	}
}

// Every removal is on the log (invariant 7), and an asset removed with its work
// is emitted exactly as one removed on its own — a subscriber must not have to
// special-case "unless the whole work went".
func TestDeleteWorkEmitsEveryRemoval(t *testing.T) {
	h := newHarness(t).seed()

	if got := h.doStable(http.MethodDelete, "/api/v1/works/"+work1ID, nil).StatusCode; got != http.StatusNoContent {
		t.Fatalf("delete = %d", got)
	}
	if n := h.eventsOfType(t, events.TypeWorkDeleted); n != 1 {
		t.Errorf("emitted %d work.deleted, want 1", n)
	}
	if n := h.eventsOfType(t, events.TypeAssetDeleted); n != 1 {
		t.Errorf("emitted %d asset.deleted, want 1 (the work's one asset)", n)
	}
	// Two wants over this work, both cancelled with it.
	if n := h.eventsOfType(t, events.TypeDesiredRemoved); n != 2 {
		t.Errorf("emitted %d desired.removed, want 2", n)
	}
}

// A standing subscription is not a want: deleting a followed work is refused
// rather than silently dropping the subscription, and the refusal says how to
// proceed.
func TestDeleteWorkRefusesAFollowedWork(t *testing.T) {
	h := newHarness(t).seed()

	resp := follow(h, `{"tvdb_id":"12345","work_id":"`+work1ID+`","quality_profile":"living-room"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("follow = %d: %s", resp.StatusCode, h.body(resp))
	}

	del := h.doStable(http.MethodDelete, "/api/v1/works/"+work1ID, nil)
	if del.StatusCode != http.StatusConflict {
		t.Fatalf("deleting a followed work = %d, want 409", del.StatusCode)
	}
	if !strings.Contains(string(h.body(del)), "followed-sources") {
		t.Error("the refusal does not say how to proceed")
	}
	if n := h.countRows(t, `SELECT count(*) FROM works WHERE id = ?`, work1ID); n != 1 {
		t.Error("the refused delete removed the work anyway")
	}
	if n := h.eventsOfType(t, events.TypeWorkDeleted); n != 0 {
		t.Error("a refused delete emitted a deletion")
	}
}

// Both mutations need `write`. A read token browsing the library must not be
// able to empty it.
func TestWorkMutationsNeedWriteScope(t *testing.T) {
	h := newHarness(t, withAuth).seed()
	readOnly := h.mint("reader", auth.ScopeRead).Secret

	for _, tt := range []struct{ method, body string }{
		{http.MethodPatch, `{"title":"x"}`},
		{http.MethodDelete, ""},
	} {
		var body *strings.Reader
		if tt.body != "" {
			body = strings.NewReader(tt.body)
		}
		var resp *http.Response
		if body == nil {
			resp = h.do(tt.method, "/api/v1/works/"+work1ID, readOnly, nil)
		} else {
			resp = h.do(tt.method, "/api/v1/works/"+work1ID, readOnly, body)
		}
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s with a read token = %d, want 403", tt.method, resp.StatusCode)
		}
	}
}

// workDoc is the fields these tests read off a work.
type workDoc struct {
	ID          string `json:"id"`
	ContentType string `json:"content_type"`
	WorkKey     string `json:"work_key"`
	Title       string `json:"title"`
	SortTitle   string `json:"sort_title"`
	Year        *int   `json:"year"`
}

func (h *harness) work(t *testing.T, id string) workDoc {
	t.Helper()
	var doc workDoc
	if err := json.Unmarshal(h.body(h.get("/api/v1/works/"+id)), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}
