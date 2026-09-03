//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The followed-source detail read and its archive (#430). A detail screen used
// to re-read the whole list and pick its id out of it, and approximated "what
// this source archived" with every want for the work.

type followedItem struct {
	ID          string `json:"id"`
	WorkID      string `json:"work_id"`
	EditionID   string `json:"edition_id"`
	ItemKey     string `json:"item_key"`
	Title       string `json:"title"`
	PublishedAt string `json:"published_at"`
	Archived    bool   `json:"archived"`
	Want        *struct {
		DesiredItemID string `json:"desired_item_id"`
		Phase         string `json:"phase"`
		Content       string `json:"content"`
		Placement     string `json:"placement"`
	} `json:"want"`
}

type followedItemsPage struct {
	Items      []followedItem `json:"items"`
	NextCursor string         `json:"next_cursor"`
}

// followASeries follows a series and returns the created source's view.
func followASeries(t *testing.T, h *harness) followedView {
	t.Helper()
	resp := follow(h, `{"tvdb_id":"12345","title":"The Series","quality_profile":"living-room"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("follow = %d: %s", resp.StatusCode, h.body(resp))
	}
	var v followedView
	if err := json.Unmarshal(h.body(resp), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// seedProjectedItems gives a followed work three items: one archived (a want
// whose content is satisfied), one wanted but not yet held, and one the source
// merely knows about — the three states the archive has to tell apart.
func seedProjectedItems(h *harness, workID string) {
	h.exec(`INSERT INTO items (id, work_id, item_key, title, published_at, attributes, created_at, updated_at) VALUES
		('it-1', ?, 'S01E01', 'The First', '2026-07-01T00:00:00Z', '{"season":1,"episode":1}', ?, ?),
		('it-2', ?, 'S01E02', 'The Second', '2026-07-08T00:00:00Z', '{}', ?, ?),
		('it-3', ?, 'S01E03', 'The Third', NULL, '{}', ?, ?)`,
		workID, seedTime, seedTime, workID, seedTime, seedTime, workID, seedTime, seedTime)
	h.exec(`INSERT INTO desired_items
		(id, scope, work_id, edition_id, item_id, quality_profile_id, monitor, reason, created_at, updated_at) VALUES
		('di-1', 'item', ?, NULL, 'it-1', ?, 1, 'followed', ?, ?),
		('di-2', 'item', ?, NULL, 'it-2', ?, 1, 'followed', ?, ?)`,
		workID, profile1ID, seedTime, seedTime,
		workID, profile1ID, seedTime, seedTime)
	h.exec(`INSERT INTO acquisition_state
		(desired_item_id, phase, managed, content, placement, detail, phase_entered_at, created_at, updated_at) VALUES
		('di-1', 'idle', 1, 'satisfied', 'converging', '', ?, ?, ?),
		('di-2', 'searching', 1, 'unknown', 'unknown', '', ?, ?, ?)`,
		seedTime, seedTime, seedTime, seedTime, seedTime, seedTime)
}

func (h *harness) followedItems(t *testing.T, path string) followedItemsPage {
	t.Helper()
	resp := h.get(path)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, resp.StatusCode)
	}
	var page followedItemsPage
	if err := json.Unmarshal(h.body(resp), &page); err != nil {
		t.Fatal(err)
	}
	return page
}

// A subscription is readable by id, and carries the followed work's title so a
// list of subscriptions reads as titles rather than as work ids.
func TestFollowedSourceIsReadableByID(t *testing.T) {
	h := newHarness(t).seed()
	created := followASeries(t, h)

	resp := h.get("/api/v1/followed-sources/" + created.ID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got followedView
	if err := json.Unmarshal(h.body(resp), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != created.ID || got.FeedRef != "12345" || got.Type != "tv_series" {
		t.Errorf("detail = %+v, want the created source", got)
	}
	if got.Title != "The Series" {
		t.Errorf("title = %q, want the followed work's title", got.Title)
	}
}

// An unknown subscription is a 404, not the 500 the catalog's own sentinel
// would otherwise become.
func TestUnknownFollowedSourceIsA404(t *testing.T) {
	h := newHarness(t).seed()

	for _, path := range []string{
		"/api/v1/followed-sources/nope",
		"/api/v1/followed-sources/nope/items",
	} {
		if got := h.get(path).StatusCode; got != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, got)
		}
	}
}

// The archive lists everything the feed yielded, in item_key order, and says of
// each whether heyarr actually holds it — including the item no want was
// projected for, which is what makes it an archive rather than a queue.
func TestFollowedSourceItemsReportTheThreeStates(t *testing.T) {
	h := newHarness(t).seed()
	created := followASeries(t, h)
	seedProjectedItems(h, created.WorkID)

	page := h.followedItems(t, "/api/v1/followed-sources/"+created.ID+"/items")
	if len(page.Items) != 3 {
		t.Fatalf("got %d items, want 3", len(page.Items))
	}
	if page.Items[0].ItemKey != "S01E01" || page.Items[2].ItemKey != "S01E03" {
		t.Errorf("items are not in item_key order: %+v", page.Items)
	}

	archived := page.Items[0]
	if !archived.Archived || archived.Want == nil || archived.Want.Content != "satisfied" {
		t.Errorf("the first item should be archived: %+v", archived)
	}
	if archived.Title != "The First" || archived.PublishedAt == "" {
		t.Errorf("the item carries no feed detail: %+v", archived)
	}

	wanted := page.Items[1]
	if wanted.Archived || wanted.Want == nil || wanted.Want.Phase != "searching" {
		t.Errorf("the second item should be wanted and unheld: %+v", wanted)
	}

	known := page.Items[2]
	if known.Want != nil || known.Archived {
		t.Errorf("the third item has no projected want and must say so: %+v", known)
	}
	// "the source did not say" is absent rather than a zero instant.
	if known.PublishedAt != "" {
		t.Errorf("published_at = %q, want absent when the source did not say", known.PublishedAt)
	}
}

// The archive pages by item_key, so a re-poll that inserts an older episode
// cannot shuffle the pages under a reader.
func TestFollowedSourceItemsPageByItemKey(t *testing.T) {
	h := newHarness(t).seed()
	created := followASeries(t, h)
	seedProjectedItems(h, created.WorkID)

	first := h.followedItems(t, "/api/v1/followed-sources/"+created.ID+"/items?limit=2")
	if len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %d items, cursor %q", len(first.Items), first.NextCursor)
	}
	second := h.followedItems(t,
		"/api/v1/followed-sources/"+created.ID+"/items?limit=2&cursor="+first.NextCursor)
	if len(second.Items) != 1 || second.Items[0].ItemKey != "S01E03" {
		t.Fatalf("second page = %+v", second.Items)
	}
	if second.NextCursor != "" {
		t.Error("the last page still offers a cursor")
	}

	// A cursor from another collection is refused rather than read as a
	// position in a different sort order.
	if got := h.get("/api/v1/followed-sources/" + created.ID +
		"/items?cursor=" + worksCursor(t, h)).StatusCode; got != http.StatusBadRequest {
		t.Errorf("a foreign cursor = %d, want 400", got)
	}
}

// A source with no items yet is an empty page, not a 404: the subscription
// exists and has simply not been polled.
func TestFollowedSourceItemsAreEmptyBeforeTheFirstPoll(t *testing.T) {
	h := newHarness(t).seed()
	created := followASeries(t, h)

	raw := h.body(h.get("/api/v1/followed-sources/" + created.ID + "/items"))
	var doc struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Items) != 0 {
		t.Errorf("items = %s, want []", raw)
	}
	if !strings.Contains(string(raw), `"items":[]`) {
		t.Errorf("an empty archive must be [] and never null: %s", raw)
	}
}
