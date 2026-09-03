//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// Deleting an edition is logical in ADR-0018's sense: its catalog rows go, its
// bytes stay, and its parent work is untouched — an edition is a subordinate,
// scanner-recreatable grouping.
func TestDeleteEditionRemovesTheCatalogRowsAndNoBytes(t *testing.T) {
	h := newHarness(t).seed()

	resp := h.doStable(http.MethodDelete, "/api/v1/editions/"+edition1ID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", resp.StatusCode, h.body(resp))
	}

	if n := h.countRows(t, `SELECT count(*) FROM editions WHERE id = ?`, edition1ID); n != 0 {
		t.Errorf("the edition is still there")
	}
	if n := h.countRows(t, `SELECT count(*) FROM assets WHERE id = ?`, asset1ID); n != 0 {
		t.Errorf("the asset survived the edition")
	}
	// The whole point: the bytes are untouched.
	if n := h.countRows(t, `SELECT count(*) FROM blobs WHERE hash = ?`, blob1Hash); n != 1 {
		t.Errorf("the blob was removed with the catalog row — ADR-0018 says never")
	}
	// The parent work stays; only the grouping went.
	if n := h.countRows(t, `SELECT count(*) FROM works WHERE id = ?`, work1ID); n != 1 {
		t.Errorf("the parent work was removed with its edition")
	}
	// A work-scoped want over the parent is NOT edition-scoped, so it survives.
	if n := h.countRows(t, `SELECT count(*) FROM desired_items WHERE id = ?`, desired1ID); n != 1 {
		t.Errorf("a work-scoped want was cancelled by an edition delete")
	}
	// A sibling edition of another work is untouched.
	if n := h.countRows(t, `SELECT count(*) FROM editions WHERE id = ?`, edition2ID); n != 1 {
		t.Errorf("an unrelated edition went with this one")
	}

	if got := h.get("/api/v1/editions/" + edition1ID).StatusCode; got != http.StatusNotFound {
		t.Errorf("the deleted edition still reads as %d", got)
	}
	if got := h.doStable(http.MethodDelete, "/api/v1/editions/"+edition1ID, nil).StatusCode; got != http.StatusNotFound {
		t.Errorf("deleting it twice = %d, want 404", got)
	}
}

// Every removal is on the log (invariant 7): the assets exactly as the per-asset
// route emits them, a desired.removed per cancelled want — both an edition-scoped
// want and one scoped to an item the edition owns — and one edition.deleted.
func TestDeleteEditionEmitsEveryRemoval(t *testing.T) {
	h := newHarness(t).seed()

	// An edition-scoped want directly on edition1, and an item belonging to
	// edition1 with an item-scoped want on it. Both cascade with the edition.
	h.exec(`INSERT INTO desired_items
		(id, scope, work_id, edition_id, item_id, quality_profile_id, monitor, reason,
		 created_at, updated_at)
		VALUES ('01990000-0000-7000-8000-0000000000d9', 'edition', ?, ?, NULL, ?, 1, '', ?, ?)`,
		work1ID, edition1ID, profile1ID, seedTime, seedTime)
	h.exec(`INSERT INTO items
		(id, work_id, edition_id, item_key, title, published_at, attributes, created_at, updated_at)
		VALUES ('01990000-0000-7000-8000-000000000it1', ?, ?, 'S01E01', 'Pilot', NULL, '{}', ?, ?)`,
		work1ID, edition1ID, seedTime, seedTime)
	h.exec(`INSERT INTO desired_items
		(id, scope, work_id, edition_id, item_id, quality_profile_id, monitor, reason,
		 created_at, updated_at)
		VALUES ('01990000-0000-7000-8000-0000000000d8', 'item', ?, NULL,
		 '01990000-0000-7000-8000-000000000it1', ?, 1, '', ?, ?)`,
		work1ID, profile1ID, seedTime, seedTime)

	if got := h.doStable(http.MethodDelete, "/api/v1/editions/"+edition1ID, nil).StatusCode; got != http.StatusNoContent {
		t.Fatalf("delete = %d", got)
	}
	if n := h.eventsOfType(t, events.TypeEditionDeleted); n != 1 {
		t.Errorf("emitted %d edition.deleted, want 1", n)
	}
	if n := h.eventsOfType(t, events.TypeAssetDeleted); n != 1 {
		t.Errorf("emitted %d asset.deleted, want 1 (the edition's one asset)", n)
	}
	// The edition-scoped want and the item-scoped want under it, both cancelled.
	if n := h.eventsOfType(t, events.TypeDesiredRemoved); n != 2 {
		t.Errorf("emitted %d desired.removed, want 2", n)
	}
}

// A follow source is standing configuration anchored to the WORK, and an
// edition is one of the seasons it projects: deleting an edition whose work is
// still followed is refused, and the refusal says how to proceed.
func TestDeleteEditionRefusesAFollowedWork(t *testing.T) {
	h := newHarness(t).seed()

	resp := follow(h, `{"tvdb_id":"12345","work_id":"`+work1ID+`","quality_profile":"living-room"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("follow = %d: %s", resp.StatusCode, h.body(resp))
	}

	del := h.doStable(http.MethodDelete, "/api/v1/editions/"+edition1ID, nil)
	if del.StatusCode != http.StatusConflict {
		t.Fatalf("deleting an edition of a followed work = %d, want 409", del.StatusCode)
	}
	if !strings.Contains(string(h.body(del)), "followed-sources") {
		t.Error("the refusal does not say how to proceed")
	}
	if n := h.countRows(t, `SELECT count(*) FROM editions WHERE id = ?`, edition1ID); n != 1 {
		t.Error("the refused delete removed the edition anyway")
	}
	if n := h.eventsOfType(t, events.TypeEditionDeleted); n != 0 {
		t.Error("a refused delete emitted a deletion")
	}
}

// Deleting an edition needs `write`. A read token browsing the library must not
// be able to empty it.
func TestDeleteEditionNeedsWriteScope(t *testing.T) {
	h := newHarness(t, withAuth).seed()
	readOnly := h.mint("reader", auth.ScopeRead).Secret

	resp := h.do(http.MethodDelete, "/api/v1/editions/"+edition1ID, readOnly, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("delete with a read token = %d, want 403", resp.StatusCode)
	}
}
