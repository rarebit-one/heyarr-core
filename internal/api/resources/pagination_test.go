// Every HTTP response in this file is closed by the t.Cleanup that the harness
// registers, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"fmt"
	"net/http"
	"testing"
)

// workPage is the shape a client walks.
type workPage struct {
	Items []struct {
		ID        string `json:"id"`
		SortTitle string `json:"sort_title"`
	} `json:"items"`
	NextCursor string `json:"next_cursor"`
}

// The reason keyset pagination exists is concurrent inserts, so the test for it
// has to perform concurrent inserts. A keyset test that walks a static table
// passes just as happily with OFFSET, which is why the sabotage run for this
// test is the one that matters most.
//
// The inserts land *before* the cursor in sort order, which is the case OFFSET
// gets wrong: every row after the insertion point shifts forward by one, so the
// next page repeats rows the client already has and the end of the collection
// is never reached. A scan writing "Aliens" into a library while someone is
// browsing the Bs is exactly this.
func TestPaginationIsStableWhileTheTableIsWrittenTo(t *testing.T) {
	h := newHarness(t)

	const total = 60
	const pageSize = 10

	original := map[string]bool{}
	for i := range total {
		id := fmt.Sprintf("01990000-0000-7000-8000-0000000%05d", i)
		sortTitle := fmt.Sprintf("title-%03d", i)
		h.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
			VALUES (?, 'movie', ?, ?, ?, NULL, '{}', ?, ?)`,
			id, sortTitle, sortTitle, sortTitle, seedTime, seedTime)
		original[id] = true
	}

	seen := []string{}
	counts := map[string]int{}
	path := fmt.Sprintf("/api/v1/works?limit=%d", pageSize)
	interleaved := 0

	for pages := 0; ; pages++ {
		if pages > total {
			t.Fatalf("the walk did not terminate after %d pages, which means the cursor is not advancing", pages)
		}
		var page workPage
		decode(t, h, path, &page)
		for _, item := range page.Items {
			seen = append(seen, item.ID)
			counts[item.ID]++
		}
		if page.NextCursor == "" {
			break
		}

		// Write between pages. These sort before everything already returned,
		// so a correct implementation never shows them and never loses its
		// place; OFFSET shifts underneath the client and does both.
		for k := range 3 {
			id := fmt.Sprintf("01990000-0000-7000-8000-0000009%02d%03d", pages, k)
			sortTitle := fmt.Sprintf("aaa-%02d-%03d", pages, k)
			h.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
				VALUES (?, 'movie', ?, ?, ?, NULL, '{}', ?, ?)`,
				id, sortTitle, sortTitle, sortTitle, seedTime, seedTime)
			interleaved++
		}

		path = fmt.Sprintf("/api/v1/works?limit=%d&cursor=%s", pageSize, page.NextCursor)
	}

	if interleaved == 0 {
		t.Fatal("nothing was inserted during the walk, so this test is not testing what it says it is")
	}

	var duplicated, unexpected []string
	for id, n := range counts {
		if n > 1 {
			duplicated = append(duplicated, fmt.Sprintf("%s x%d", id, n))
		}
		if !original[id] {
			unexpected = append(unexpected, id)
		}
	}
	var skipped []string
	for id := range original {
		if counts[id] == 0 {
			skipped = append(skipped, id)
		}
	}

	if len(duplicated) > 0 {
		t.Errorf("%d row(s) were returned more than once: %v", len(duplicated), duplicated)
	}
	if len(skipped) > 0 {
		t.Errorf("%d row(s) that existed before the walk were never returned: %v", len(skipped), skipped)
	}
	if len(unexpected) > 0 {
		t.Errorf("%d row(s) inserted during the walk appeared before the cursor: %v", len(unexpected), unexpected)
	}
	if len(seen) != total {
		t.Errorf("the walk returned %d rows; the collection held %d when it started", len(seen), total)
	}
}

// A non-unique sort key is the second way to lose rows, and it is the one that
// looks correct in a demo: sort_title alone is not unique, so the boundary
// between two pages can fall in the middle of a group of equal titles.
func TestPagesDoNotSplitRowsThatShareASortTitle(t *testing.T) {
	h := newHarness(t)

	const total = 25
	original := map[string]bool{}
	for i := range total {
		id := fmt.Sprintf("01990000-0000-7000-8000-0000000%05d", i)
		// Every row has the SAME sort title.
		h.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
			VALUES (?, 'movie', ?, 'Untitled', 'untitled', NULL, '{}', ?, ?)`,
			id, fmt.Sprintf("key-%d", i), seedTime, seedTime)
		original[id] = true
	}

	counts := map[string]int{}
	path := "/api/v1/works?limit=7"
	for pages := 0; ; pages++ {
		if pages > total {
			t.Fatal("the walk did not terminate, so the cursor is not advancing past the shared sort title")
		}
		var page workPage
		decode(t, h, path, &page)
		for _, item := range page.Items {
			counts[item.ID]++
		}
		if page.NextCursor == "" {
			break
		}
		path = "/api/v1/works?limit=7&cursor=" + page.NextCursor
	}

	for id := range original {
		switch counts[id] {
		case 1:
		case 0:
			t.Errorf("%s was never returned", id)
		default:
			t.Errorf("%s was returned %d times", id, counts[id])
		}
	}
}

func TestCursorsAreOpaqueAndValidated(t *testing.T) {
	h := newHarness(t).seed()

	tests := []struct {
		name   string
		path   string
		status int
	}{
		{"a garbage cursor", "/api/v1/works?cursor=not-base64!!", http.StatusBadRequest},
		{"base64 that is not a cursor", "/api/v1/works?cursor=aGVsbG8", http.StatusBadRequest},
		{"a cursor with the wrong arity", "/api/v1/works?cursor=" +
			"eyJ2IjoxLCJjIjoid29ya3MiLCJrIjpbIm9uZSJdfQ", http.StatusBadRequest},
		{"a cursor from the future", "/api/v1/works?cursor=" +
			"eyJ2Ijo5OSwiYyI6IndvcmtzIiwiayI6WyJhIiwiYiJdfQ", http.StatusBadRequest},
		{"a negative limit", "/api/v1/works?limit=-1", http.StatusBadRequest},
		{"a limit that is not a number", "/api/v1/works?limit=lots", http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := h.get(tt.path)
			raw := h.body(resp)
			if resp.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tt.status, raw)
			}
			decodeProblem(t, resp, raw)
		})
	}
}

// A limit above the maximum is clamped rather than rejected, and the clamp has
// to actually apply: an unbounded limit is a client asking the server to hold
// the whole catalog in memory.
func TestLimitIsClamped(t *testing.T) {
	h := newHarness(t)
	const total = 250
	for i := range total {
		h.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
			VALUES (?, 'movie', ?, ?, ?, NULL, '{}', ?, ?)`,
			fmt.Sprintf("01990000-0000-7000-8000-0000000%05d", i),
			fmt.Sprintf("key-%d", i), fmt.Sprintf("title-%03d", i),
			fmt.Sprintf("title-%03d", i), seedTime, seedTime)
	}

	var page workPage
	decode(t, h, "/api/v1/works?limit=100000", &page)
	if len(page.Items) != 200 {
		t.Errorf("limit=100000 returned %d rows; it must be clamped to 200", len(page.Items))
	}
	if page.NextCursor == "" {
		t.Error("the clamped page reports no continuation, so the rest of the collection is unreachable")
	}
}
