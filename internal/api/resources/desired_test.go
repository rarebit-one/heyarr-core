// Every HTTP response in this file is closed by the t.Cleanup that the harness
// registers, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/auth"
)

// Desired items (§55, M3-02).
//
// The claim under test throughout is the one most easily lost: a want must be
// expressible for content with NO asset, no blob and no bytes anywhere. Every
// fixture in this repository has assets, so a design that only works once
// something exists passes every other test in this package.

func postDesired(t *testing.T, h *harness, body string) *http.Response {
	t.Helper()
	return h.doStable(http.MethodPost, "/api/v1/desired", strings.NewReader(body))
}

func decodeDesired(t *testing.T, h *harness, resp *http.Response) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(h.body(resp), &got); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	return got
}

// THE test for this issue. Wanting something Heyarr has never seen, naming it
// only by what it is, and getting a want back.
func TestWantingContentThatDoesNotExist(t *testing.T) {
	h := newHarness(t).seed()

	resp := postDesired(t, h, `{
		"work": {"content_type":"movie","title":"The Conversation","year":1974},
		"quality_profile": "living-room",
		"reason": "nothing on disk, nothing scanned, nothing anywhere"
	}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, h.body(resp))
	}
	got := decodeDesired(t, h, resp)

	workID, _ := got["work_id"].(string)
	if workID == "" {
		t.Fatal("the want must anchor to a work, created on demand if need be")
	}
	if got["scope"] != "work" {
		t.Errorf("scope defaulted to %v, want work", got["scope"])
	}
	if got["monitor"] != true {
		t.Errorf("monitor defaulted to %v, want true", got["monitor"])
	}

	// And the Work it created genuinely has nothing behind it. Asserted
	// against the database rather than through /assets, which has no work_id
	// filter — an unsupported query parameter is ignored, so that route would
	// have counted every asset in the fixture and "passed" at 3.
	if n := h.countRows(t,
		`SELECT count(*) FROM assets a JOIN editions e ON e.id = a.edition_id WHERE e.work_id = ?`,
		workID); n != 0 {
		t.Errorf("the wanted work should have no assets, found %d", n)
	}
	if n := h.countRows(t, `SELECT count(*) FROM editions WHERE work_id = ?`, workID); n != 0 {
		t.Errorf("the wanted work should have no editions either, found %d", n)
	}
}

// The convergence that makes the descriptor safe. If wanting used a different
// normalisation from scanning, everything would appear to work and the library
// would slowly fill with pairs of works that are the same thing.
func TestWantingThenWantingAgainConvergesOnOneWork(t *testing.T) {
	h := newHarness(t).seed()

	first := postDesired(t, h, `{
		"work": {"content_type":"movie","title":"The Conversation","year":1974},
		"quality_profile": "living-room"}`)
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", first.StatusCode, h.body(first))
	}
	firstWork, _ := decodeDesired(t, h, first)["work_id"].(string)

	// Spelled differently, with the year in the title, under a different
	// profile so the want itself is legal.
	second := postDesired(t, h, `{
		"work": {"content_type":"movie","title":"the conversation (1974)"},
		"quality_profile": "archival"}`)
	if second.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", second.StatusCode, h.body(second))
	}
	secondWork, _ := decodeDesired(t, h, second)["work_id"].(string)

	if firstWork != secondWork {
		t.Errorf("two spellings of one film produced two works (%s, %s) — "+
			"the descriptor must use the scanner's normalisation",
			firstWork, secondWork)
	}
}

// §61: never one version per title. The living-room copy and the phone-sized
// copy are two wants and both must exist.
func TestTwoProfilesOverOneWorkAreTwoWants(t *testing.T) {
	h := newHarness(t).seed()

	first := postDesired(t, h, `{
		"work": {"content_type":"movie","title":"Stalker","year":1979},
		"quality_profile": "living-room"}`)
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first: status = %d: %s", first.StatusCode, h.body(first))
	}
	workID, _ := decodeDesired(t, h, first)["work_id"].(string)

	second := postDesired(t, h, fmt.Sprintf(`{
		"work_id": %q, "quality_profile": "archival"}`, workID))
	if second.StatusCode != http.StatusCreated {
		t.Fatalf("a second profile over one work is the §61 rule, got %d: %s",
			second.StatusCode, h.body(second))
	}

	// The same profile twice is one want written twice.
	third := postDesired(t, h, fmt.Sprintf(`{
		"work_id": %q, "quality_profile": "living-room"}`, workID))
	if third.StatusCode != http.StatusConflict {
		t.Fatalf("a duplicate want should be 409, got %d: %s", third.StatusCode, h.body(third))
	}
	if detail := problemDetail(t, h, third); !strings.Contains(detail, "different profile") {
		t.Errorf("the conflict should say how to want a second copy; got: %s", detail)
	}
}

// A profile is required, because §56 has nothing to evaluate without one.
func TestAWantMustNameAQualityProfile(t *testing.T) {
	h := newHarness(t).seed()
	resp := postDesired(t, h, `{"work": {"content_type":"movie","title":"Solaris"}}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, h.body(resp))
	}
	if detail := problemDetail(t, h, resp); !strings.Contains(detail, "quality profile") {
		t.Errorf("the refusal should name what is missing; got: %s", detail)
	}
}

func TestAmbiguousAndMissingTargetsAreRefused(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{
			"neither a work id nor a descriptor",
			`{"quality_profile":"living-room"}`,
			"must name a work",
		},
		{
			"both a work id and a descriptor",
			`{"work_id":"w","work":{"content_type":"movie","title":"x"},"quality_profile":"living-room"}`,
			"not both",
		},
		{
			"both a profile id and a profile name",
			`{"work":{"content_type":"movie","title":"x"},
			  "quality_profile_id":"q","quality_profile":"living-room"}`,
			"not both",
		},
		{
			"a descriptor with no content type",
			`{"work":{"title":"x"},"quality_profile":"living-room"}`,
			"content_type",
		},
		{
			"a descriptor with no title",
			`{"work":{"content_type":"movie"},"quality_profile":"living-room"}`,
			"title",
		},
		{
			"an edition scope with no edition",
			`{"work":{"content_type":"movie","title":"x"},"scope":"edition",
			  "quality_profile":"living-room"}`,
			"must name the edition",
		},
		{
			"a quality profile that does not exist",
			`{"work":{"content_type":"movie","title":"x"},"quality_profile":"nonexistent"}`,
			"nonexistent",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t).seed()
			resp := postDesired(t, h, tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, h.body(resp))
			}
			if detail := problemDetail(t, h, resp); !strings.Contains(detail, tc.want) {
				t.Errorf("the refusal should mention %q; got: %s", tc.want, detail)
			}
		})
	}
}

// A work-scoped want carrying an edition id is refused, because an unused id is
// exactly the kind of field something later reads without checking the scope.
func TestWorkScopeMustNotCarryAnEdition(t *testing.T) {
	h := newHarness(t).seed()
	resp := postDesired(t, h, fmt.Sprintf(`{
		"work_id": %q, "edition_id": %q, "scope": "work",
		"quality_profile": "living-room"}`, work2ID, edition2ID))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, h.body(resp))
	}
	if detail := problemDetail(t, h, resp); !strings.Contains(detail, "must not name an edition") {
		t.Errorf("the refusal should explain the scope; got: %s", detail)
	}
}

func TestEditionScopedWant(t *testing.T) {
	h := newHarness(t).seed()
	resp := postDesired(t, h, fmt.Sprintf(`{
		"work_id": %q, "edition_id": %q, "scope": "edition",
		"quality_profile": "living-room"}`, work2ID, edition2ID))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, h.body(resp))
	}
	got := decodeDesired(t, h, resp)
	if got["edition_id"] != edition2ID || got["scope"] != "edition" {
		t.Errorf("edition scope did not round-trip: %v", got)
	}
}

// Monitored and wanted are two axes (§60 keeps both words).
func TestMonitorIsSeparateFromWanting(t *testing.T) {
	h := newHarness(t).seed()
	resp := postDesired(t, h, `{
		"work": {"content_type":"movie","title":"Le Samourai","year":1967},
		"quality_profile": "living-room", "monitor": false}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	created := decodeDesired(t, h, resp)
	if created["monitor"] != false {
		t.Fatalf("monitor=false should round-trip, got %v", created["monitor"])
	}
	id, _ := created["id"].(string)

	// And it is filterable, because the upgrade workflow (M3-06) selects on it.
	page := h.get("/api/v1/desired?monitor=false")
	var got struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(h.body(page), &got); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, item := range got.Items {
		if item["id"] == id {
			found = true
		}
		if item["monitor"] != false {
			t.Errorf("monitor=false returned a monitored want: %v", item)
		}
	}
	if !found {
		t.Errorf("monitor=false did not return the unmonitored want (%d returned)", len(got.Items))
	}
	// And the filter genuinely filters: the monitored fixture want must not be
	// in there. Asserting only "mine came back" would pass against no filter
	// at all.
	if len(got.Items) >= h.countRows(t, `SELECT count(*) FROM desired_items`) {
		t.Error("monitor=false returned every want, so the filter is not filtering")
	}
}

// Repointing a want at different content is not an edit — it is a different
// want, and allowing it would make the acquisition history describe something
// else.
func TestTheTargetCannotBeChanged(t *testing.T) {
	h := newHarness(t).seed()
	resp := postDesired(t, h, fmt.Sprintf(`{"work_id": %q, "quality_profile":"living-room"}`, work2ID))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	id, _ := decodeDesired(t, h, resp)["id"].(string)

	// work_id is not a field of the PATCH body at all, so an attempt to send
	// it is rejected as an unknown field rather than silently ignored.
	patch := h.doStable(http.MethodPatch, "/api/v1/desired/"+id,
		strings.NewReader(`{"work_id":"something-else"}`))
	if patch.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", patch.StatusCode, h.body(patch))
	}
}

func TestPatchChangesProfileMonitorAndReason(t *testing.T) {
	h := newHarness(t).seed()
	resp := postDesired(t, h, fmt.Sprintf(`{"work_id": %q, "quality_profile":"living-room"}`, work2ID))
	id, _ := decodeDesired(t, h, resp)["id"].(string)

	patch := h.doStable(http.MethodPatch, "/api/v1/desired/"+id,
		strings.NewReader(`{"quality_profile":"archival","monitor":false,"reason":"changed my mind"}`))
	if patch.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", patch.StatusCode, h.body(patch))
	}
	got := decodeDesired(t, h, patch)
	if got["quality_profile_id"] != profile2ID {
		t.Errorf("the profile should have changed to archival, got %v", got["quality_profile_id"])
	}
	if got["monitor"] != false || got["reason"] != "changed my mind" {
		t.Errorf("the patch did not apply: %v", got)
	}

	// An omitted field is left alone.
	patch = h.doStable(http.MethodPatch, "/api/v1/desired/"+id, strings.NewReader(`{"reason":"again"}`))
	got = decodeDesired(t, h, patch)
	if got["monitor"] != false {
		t.Errorf("an omitted field must be left alone, monitor became %v", got["monitor"])
	}
}

// Invariant 7, and its converse.
func TestDesiredEventsFireForChangesAndOnlyForChanges(t *testing.T) {
	h := newHarness(t).seed()

	before := h.eventCount(t)
	resp := postDesired(t, h, fmt.Sprintf(`{"work_id": %q, "quality_profile":"living-room"}`, work2ID))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	id, _ := decodeDesired(t, h, resp)["id"].(string)
	if got := h.eventCount(t); got != before+1 {
		t.Fatalf("creating a want should emit one event, got %d", got-before)
	}

	// A no-op patch emits nothing.
	afterCreate := h.eventCount(t)
	if p := h.doStable(http.MethodPatch, "/api/v1/desired/"+id,
		strings.NewReader(`{"monitor":true}`)); p.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", p.StatusCode)
	}
	if got := h.eventCount(t); got != afterCreate {
		t.Errorf("a patch that changes nothing emitted %d event(s)", got-afterCreate)
	}

	// A real change emits.
	if p := h.doStable(http.MethodPatch, "/api/v1/desired/"+id,
		strings.NewReader(`{"monitor":false}`)); p.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", p.StatusCode)
	}
	if got := h.eventCount(t); got != afterCreate+1 {
		t.Errorf("a real change should emit one event, got %d", got-afterCreate)
	}

	// Deleting emits, and the event carries the target so the log still says
	// what was wanted after the row is gone.
	afterPatch := h.eventCount(t)
	if d := h.doStable(http.MethodDelete, "/api/v1/desired/"+id, nil); d.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", d.StatusCode)
	}
	if got := h.eventCount(t); got != afterPatch+1 {
		t.Errorf("deleting should emit one event, got %d", got-afterPatch)
	}
	if got := h.get("/api/v1/desired/" + id); got.StatusCode != http.StatusNotFound {
		t.Errorf("a deleted want should be gone, status = %d", got.StatusCode)
	}
}

// Creating a Work because somebody wanted it is a catalog transition, and a
// subscriber watching the catalog grow should see it however it was created.
func TestWantingUnknownContentEmitsAWorkCreatedEvent(t *testing.T) {
	h := newHarness(t).seed()
	before := h.eventCount(t)

	resp := postDesired(t, h, `{
		"work": {"content_type":"movie","title":"Wings of Desire","year":1987},
		"quality_profile":"living-room"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	// One for the work, one for the want.
	if got := h.eventCount(t); got != before+2 {
		t.Errorf("wanting unknown content should emit a work and a want, got %d", got-before)
	}

	// Wanting a work that already exists creates no second work.
	before = h.eventCount(t)
	if r := postDesired(t, h, fmt.Sprintf(`{"work_id": %q, "quality_profile":"archival"}`,
		work2ID)); r.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", r.StatusCode, h.body(r))
	}
	if got := h.eventCount(t); got != before+1 {
		t.Errorf("wanting an existing work should emit only the want, got %d", got-before)
	}
}

// A profile still measuring a want cannot be deleted: deleting the standard and
// leaving the desire makes satisfaction unanswerable (§56).
func TestAProfileInUseCannotBeDeleted(t *testing.T) {
	h := newHarness(t).seed()
	if r := postDesired(t, h, fmt.Sprintf(`{"work_id": %q, "quality_profile":"living-room"}`,
		work2ID)); r.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", r.StatusCode, h.body(r))
	}

	resp := h.doStable(http.MethodDelete, "/api/v1/quality-profiles/"+profile1ID, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, h.body(resp))
	}
	if detail := problemDetail(t, h, resp); !strings.Contains(detail, "still in use") {
		t.Errorf("the conflict should say why; got: %s", detail)
	}
}

// Deleting a Work takes its wants with it — a want for something that no longer
// exists is a dangling reference every read path would have to special-case.
func TestDeletingAWorkCascadesToItsWants(t *testing.T) {
	h := newHarness(t).seed()
	resp := postDesired(t, h, fmt.Sprintf(`{"work_id": %q, "quality_profile":"living-room"}`, work2ID))
	id, _ := decodeDesired(t, h, resp)["id"].(string)

	h.exec(`DELETE FROM works WHERE id = ?`, work2ID)

	if got := h.get("/api/v1/desired/" + id); got.StatusCode != http.StatusNotFound {
		t.Errorf("the want should have gone with its work, status = %d", got.StatusCode)
	}
}

func TestDesiredPaginates(t *testing.T) {
	h := newHarness(t).seed()
	for i, profile := range []string{"living-room", "archival"} {
		body := fmt.Sprintf(`{"work":{"content_type":"movie","title":"Film %d","year":%d},
			"quality_profile":%q}`, i, 2000+i, profile)
		if r := postDesired(t, h, body); r.StatusCode != http.StatusCreated {
			t.Fatalf("seeding want %d: status = %d: %s", i, r.StatusCode, h.body(r))
		}
	}

	resp := h.get("/api/v1/desired?limit=1")
	var first struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"next_cursor"`
	}
	if err := json.Unmarshal(h.body(resp), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatalf("expected one item and a cursor, got %d items, cursor %q",
			len(first.Items), first.NextCursor)
	}
	resp = h.get("/api/v1/desired?limit=1&cursor=" + first.NextCursor)
	var second struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(h.body(resp), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 {
		t.Fatalf("expected a second page, got %d", len(second.Items))
	}
	if second.Items[0]["id"] == first.Items[0]["id"] {
		t.Error("the second page repeated the first page's row")
	}
}

func TestUnknownDesiredItemIs404(t *testing.T) {
	h := newHarness(t).seed()
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		if resp := h.doStable(method, "/api/v1/desired/nope", nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", method, resp.StatusCode)
		}
	}
	resp := h.doStable(http.MethodPatch, "/api/v1/desired/nope", strings.NewReader(`{"monitor":false}`))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("PATCH: status = %d, want 404", resp.StatusCode)
	}
}

// Wanting is a write, not an admin action: it is ordinary operator traffic.
func TestWantingNeedsAWriteToken(t *testing.T) {
	h := newHarness(t, withAuth).seed()
	readToken := h.mint("reader", auth.ScopeRead)

	resp := h.do(http.MethodPost, "/api/v1/desired", readToken.Secret,
		strings.NewReader(fmt.Sprintf(`{"work_id":%q,"quality_profile":"living-room"}`, work2ID)))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a read token must not be able to want things, status = %d", resp.StatusCode)
	}

	writeToken := h.mint("writer", auth.ScopeRead, auth.ScopeWrite)
	resp = h.do(http.MethodPost, "/api/v1/desired", writeToken.Secret,
		strings.NewReader(fmt.Sprintf(`{"work_id":%q,"quality_profile":"living-room"}`, work2ID)))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("a write token should be enough, status = %d: %s", resp.StatusCode, h.body(resp))
	}
}
