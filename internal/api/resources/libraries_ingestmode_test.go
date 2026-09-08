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

// Changing a root's ingest mode after creation is the supported way to apply
// #222's fix: a store that shipped on `reflink` and lands a byte copy on a
// filesystem that cannot block-clone is switched to `hardlink` in place. The
// seeded films root is `reflink`; this flips it and observes the row, the
// returned representation and the event all agree.
func TestUpdateLibraryRootChangesIngestMode(t *testing.T) {
	h := newHarness(t).seed()

	resp := h.doStable(http.MethodPatch, "/api/v1/libraries/"+libFilmsID+"/roots/"+rootID,
		strings.NewReader(`{"ingest_mode":"hardlink"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d: %s", resp.StatusCode, h.body(resp))
	}

	var got struct {
		ID         string `json:"id"`
		IngestMode string `json:"ingest_mode"`
	}
	if err := json.Unmarshal(h.body(resp), &got); err != nil {
		t.Fatalf("decoding the patched root: %v", err)
	}
	if got.ID != rootID || got.IngestMode != "hardlink" {
		t.Errorf("patched root = %+v, want id %s mode hardlink", got, rootID)
	}

	// The ground truth is the row the next ingest reads, not the echoed body.
	if n := h.countRows(t, `SELECT count(*) FROM library_roots WHERE id = ? AND ingest_mode = 'hardlink'`, rootID); n != 1 {
		t.Errorf("the row still does not read hardlink")
	}
	if n := h.eventsOfType(t, events.TypeLibraryRootUpdated); n != 1 {
		t.Errorf("emitted %d library_root.updated, want 1", n)
	}
}

// A mode outside the closed set is refused, and the refused patch changes
// nothing — the CHECK constraint would reject it at the database anyway, but the
// operator gets a 400 naming the valid modes rather than a 500.
func TestUpdateLibraryRootRejectsUnknownMode(t *testing.T) {
	h := newHarness(t).seed()

	for _, body := range []string{`{"ingest_mode":"symlink"}`, `{"ingest_mode":""}`, `{}`} {
		resp := h.doStable(http.MethodPatch, "/api/v1/libraries/"+libFilmsID+"/roots/"+rootID,
			strings.NewReader(body))
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("patch %s = %d, want 400", body, resp.StatusCode)
		}
	}
	// Untouched: still the seeded reflink.
	if n := h.countRows(t, `SELECT count(*) FROM library_roots WHERE id = ? AND ingest_mode = 'reflink'`, rootID); n != 1 {
		t.Errorf("a refused patch changed the mode anyway")
	}
	if n := h.eventsOfType(t, events.TypeLibraryRootUpdated); n != 0 {
		t.Errorf("a refused patch emitted %d updates, want 0", n)
	}
}

// A root is amended under its own library. A root id belonging to another
// library is a 404 through the wrong path, never a cross-library edit.
func TestUpdateLibraryRootIsScopedToItsLibrary(t *testing.T) {
	h := newHarness(t).seed()

	const booksRootID = "01990000-0000-7000-8000-0000000000rb"
	h.exec(`INSERT INTO library_roots (id, library_id, path, ingest_mode, enabled, created_at)
		VALUES (?, ?, '/srv/books', 'reflink', 1, ?)`, booksRootID, libBooksID, seedTime)

	resp := h.doStable(http.MethodPatch, "/api/v1/libraries/"+libFilmsID+"/roots/"+booksRootID,
		strings.NewReader(`{"ingest_mode":"hardlink"}`))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-library root patch = %d, want 404", resp.StatusCode)
	}
	if n := h.countRows(t, `SELECT count(*) FROM library_roots WHERE id = ? AND ingest_mode = 'reflink'`, booksRootID); n != 1 {
		t.Error("the root of another library was amended through the wrong path")
	}
}

// The patch needs `write`. A read token browsing the library must not be able to
// change how it ingests.
func TestUpdateLibraryRootNeedsWriteScope(t *testing.T) {
	h := newHarness(t, withAuth).seed()
	readOnly := h.mint("reader", auth.ScopeRead).Secret

	resp := h.do(http.MethodPatch, "/api/v1/libraries/"+libFilmsID+"/roots/"+rootID, readOnly,
		strings.NewReader(`{"ingest_mode":"hardlink"}`))
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("PATCH with a read token = %d, want 403", resp.StatusCode)
	}
}
