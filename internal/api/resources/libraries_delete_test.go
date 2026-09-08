//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// A library that holds nothing goes, and its roots go with it — the roots are
// ON DELETE CASCADE, so the database performs the removal (#228). Logical in
// ADR-0018's sense: the catalog rows go and not one byte is unlinked.
func TestDeleteEmptyLibraryRemovesItAndItsRoots(t *testing.T) {
	h := newHarness(t).seed()

	// An empty library with a root and no assets — the shell a throwaway or a
	// mis-typed library leaves behind once its works have been removed.
	const emptyLibID = "01990000-0000-7000-8000-0000000000le"
	const emptyRootID = "01990000-0000-7000-8000-0000000000re"
	h.exec(`INSERT INTO libraries (id, name, content_type, enabled, created_at)
		VALUES (?, 'throwaway', 'movie', 1, ?)`, emptyLibID, seedTime)
	h.exec(`INSERT INTO library_roots (id, library_id, path, ingest_mode, enabled, created_at)
		VALUES (?, ?, '/srv/throwaway', 'reflink', 1, ?)`, emptyRootID, emptyLibID, seedTime)

	resp := h.doStable(http.MethodDelete, "/api/v1/libraries/"+emptyLibID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", resp.StatusCode, h.body(resp))
	}

	if n := h.countRows(t, `SELECT count(*) FROM libraries WHERE id = ?`, emptyLibID); n != 0 {
		t.Errorf("the library is still there")
	}
	if n := h.countRows(t, `SELECT count(*) FROM library_roots WHERE library_id = ?`, emptyLibID); n != 0 {
		t.Errorf("%d roots survived the library", n)
	}
	if n := h.eventsOfType(t, events.TypeLibraryDeleted); n != 1 {
		t.Errorf("emitted %d library.deleted, want 1", n)
	}

	if got := h.get("/api/v1/libraries/" + emptyLibID).StatusCode; got != http.StatusNotFound {
		t.Errorf("the deleted library still reads as %d", got)
	}
	if got := h.doStable(http.MethodDelete, "/api/v1/libraries/"+emptyLibID, nil).StatusCode; got != http.StatusNotFound {
		t.Errorf("deleting it twice = %d, want 404", got)
	}
}

// The refusal that keeps a library delete from silently orphaning content:
// assets.library_id is ON DELETE SET NULL, so a raw delete would strip a whole
// library's assets of their library rather than refuse. The seeded `films`
// library holds two assets, so its delete is refused and says how to proceed.
func TestDeleteLibraryRefusesWhileItHoldsContent(t *testing.T) {
	h := newHarness(t).seed()

	resp := h.doStable(http.MethodDelete, "/api/v1/libraries/"+libFilmsID, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("deleting a non-empty library = %d, want 409", resp.StatusCode)
	}
	if !strings.Contains(string(h.body(resp)), "works") {
		t.Error("the refusal does not say how to proceed")
	}
	if n := h.countRows(t, `SELECT count(*) FROM libraries WHERE id = ?`, libFilmsID); n != 1 {
		t.Error("the refused delete removed the library anyway")
	}
	// Crucially, no asset was orphaned by a SET NULL that should never have run.
	if n := h.countRows(t, `SELECT count(*) FROM assets WHERE library_id = ?`, libFilmsID); n != 2 {
		t.Errorf("the refused delete left %d assets under the library, want 2", n)
	}
	if n := h.eventsOfType(t, events.TypeLibraryDeleted); n != 0 {
		t.Error("a refused delete emitted a deletion")
	}
}

// Removing a root stops future scans of a directory; it does not remove content
// (#228). An asset belongs to its library, not to a root, so the seeded films
// asset is untouched when the films root goes.
func TestDeleteLibraryRootStopsScanningWithoutRemovingContent(t *testing.T) {
	h := newHarness(t).seed()

	resp := h.doStable(http.MethodDelete, "/api/v1/libraries/"+libFilmsID+"/roots/"+rootID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete root = %d: %s", resp.StatusCode, h.body(resp))
	}

	if n := h.countRows(t, `SELECT count(*) FROM library_roots WHERE id = ?`, rootID); n != 0 {
		t.Errorf("the root is still there")
	}
	if n := h.countRows(t, `SELECT count(*) FROM libraries WHERE id = ?`, libFilmsID); n != 1 {
		t.Errorf("removing a root removed its library")
	}
	if n := h.countRows(t, `SELECT count(*) FROM assets WHERE id = ?`, asset1ID); n != 1 {
		t.Errorf("removing a root removed content ingested through it")
	}
	if n := h.eventsOfType(t, events.TypeLibraryRootRemoved); n != 1 {
		t.Errorf("emitted %d library_root.removed, want 1", n)
	}
}

// A root is addressed under its own library. A root id that belongs to another
// library is a 404 here, never a cross-library delete.
func TestDeleteLibraryRootIsScopedToItsLibrary(t *testing.T) {
	h := newHarness(t).seed()

	// A root under `books`, deleted through the `films` path.
	const booksRootID = "01990000-0000-7000-8000-0000000000rb"
	h.exec(`INSERT INTO library_roots (id, library_id, path, ingest_mode, enabled, created_at)
		VALUES (?, ?, '/srv/books', 'reflink', 1, ?)`, booksRootID, libBooksID, seedTime)

	resp := h.doStable(http.MethodDelete, "/api/v1/libraries/"+libFilmsID+"/roots/"+booksRootID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-library root delete = %d, want 404", resp.StatusCode)
	}
	if n := h.countRows(t, `SELECT count(*) FROM library_roots WHERE id = ?`, booksRootID); n != 1 {
		t.Error("the root of another library was removed through the wrong path")
	}
}

// Both deletes need `write`. A read token browsing the library must not be able
// to remove it or its roots.
func TestLibraryDeletesNeedWriteScope(t *testing.T) {
	h := newHarness(t, withAuth).seed()
	readOnly := h.mint("reader", auth.ScopeRead).Secret

	for _, path := range []string{
		"/api/v1/libraries/" + libFilmsID,
		"/api/v1/libraries/" + libFilmsID + "/roots/" + rootID,
	} {
		resp := h.do(http.MethodDelete, path, readOnly, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("DELETE %s with a read token = %d, want 403", path, resp.StatusCode)
		}
	}
}
