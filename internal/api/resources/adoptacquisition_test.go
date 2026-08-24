// Every HTTP response in this file is closed by the t.Cleanup that the harness
// registers, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// postAcquisition adopts a transfer for a want, the way §65's "bytes arrived
// some other way" path is meant to be used.
func postAcquisition(t *testing.T, h *harness, id, body string) map[string]any {
	t.Helper()
	resp := h.doStable(http.MethodPost, "/api/v1/desired/"+id+"/acquisitions",
		strings.NewReader(body))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	var got map[string]any
	if err := json.Unmarshal(h.body(resp), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// newWant creates a want that has never searched for anything.
func newWant(t *testing.T, h *harness, title string) string {
	t.Helper()
	resp := postDesired(t, h, `{
		"work": {"content_type":"movie","title":"`+title+`","year":1972},
		"quality_profile":"living-room"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	id, _ := decodeDesired(t, h, resp)["id"].(string)
	if id == "" {
		t.Fatal("the want has no id")
	}
	return id
}

// THE assertion #240 exists for.
//
// A want that never selected anything rests in `idle`, and this endpoint's own
// description names that case as one it exists for — "something fetched by
// hand" is precisely a want nobody searched. It answered 202, the job it queued
// reported success, and NOTHING was ingested, because every transition it tried
// was illegal from idle and the loop skipped all of them in silence.
//
// It was reachable in practice: when the indexers return nothing (#239), the
// want stays MISSING/idle, and an operator who then fetches the release
// themselves and posts it here got a success and no result.
func TestAdoptingBytesForAWantThatNeverSearchedActuallyAdvancesIt(t *testing.T) {
	h := newHarness(t).seed()
	id := newWant(t, h, "Solaris")

	if got := acquisitionOf(t, h, id)["phase"]; got != "idle" {
		t.Fatalf("phase = %v, want idle — this test is about a want that never started", got)
	}

	body := postAcquisition(t, h, id, `{
		"provider":"by-hand","external_id":"an-infohash",
		"external_name":"Solaris.1972.mkv","local_path":"/downloads/Solaris.1972.mkv"}`)

	// The response must not claim to have accepted work it will not do. Before
	// this it answered phase "idle", which a caller had to know meant "this
	// will silently do nothing".
	if body["phase"] != "verifying" {
		t.Errorf("the endpoint answered phase %v; a 202 that leaves the want where "+
			"it was is a success that did nothing", body["phase"])
	}
	if got := acquisitionOf(t, h, id)["phase"]; got != "verifying" {
		t.Errorf("the want is in phase %v, want verifying so the ingest can run", got)
	}
}

// Adoption is recorded as ADOPTION, not as a search nobody ran.
//
// The alternative fix — walking idle → searching → candidates_found → select →
// queue → start_download → downloaded — would have reached verifying too, and
// written six events for things that did not happen. That is the same objection
// this endpoint already makes to jumping straight to VERIFYING: a history that
// does not describe what happened.
func TestAdoptionIsRecordedAsItselfRatherThanAsAFabricatedSearch(t *testing.T) {
	h := newHarness(t).seed()
	id := newWant(t, h, "Stalker")

	postAcquisition(t, h, id, `{
		"provider":"by-hand","external_id":"another-infohash",
		"external_name":"Stalker.1979.mkv","local_path":"/downloads/Stalker.1979.mkv"}`)

	// No search was invented. release_candidates is where a search leaves its
	// answers, and a want nobody searched for must have none.
	if n := h.countRows(t,
		`SELECT count(*) FROM release_candidates WHERE desired_item_id = ?`, id); n != 0 {
		t.Errorf("%d candidate rows for a want nobody searched", n)
	}
	// Exactly one transition happened, and it is the adoption. More than one
	// would mean the want was walked through phases it was never in.
	if n := h.countRows(t,
		`SELECT count(*) FROM events WHERE subject_id = ? AND payload LIKE '%"transition":"adopt"%'`,
		id); n != 1 {
		t.Errorf("%d adopt events, want exactly 1", n)
	}
	if n := h.countRows(t,
		`SELECT count(*) FROM events WHERE subject_id = ? AND payload LIKE '%"transition":"search"%'`,
		id); n != 0 {
		t.Errorf("%d search events for a want nobody searched", n)
	}
}

// A want with a transfer genuinely in flight still adopts through the ordinary
// edges, because for that want they describe what actually happened.
//
// The control: without it, a fix that always adopted would erase the real
// history of the polled path, which is the majority case.
func TestAWantAlreadyInFlightStillWalksTheOrdinaryEdges(t *testing.T) {
	h := newHarness(t).seed()
	id := newWant(t, h, "Mirror")

	// Put the want where a search would: idle -> searching -> candidates_found
	// -> selected, through the real machine rather than by writing the row.
	for _, tr := range []string{"search", "candidates_found", "select"} {
		h.exec(`UPDATE acquisition_state SET phase = ? WHERE desired_item_id = ?`,
			map[string]string{
				"search": "searching", "candidates_found": "candidates_found",
				"select": "selected",
			}[tr], id)
	}

	body := postAcquisition(t, h, id, `{
		"provider":"a-client","external_id":"third-infohash",
		"external_name":"Mirror.1975.mkv","local_path":"/downloads/Mirror.1975.mkv"}`)
	if body["phase"] != "verifying" {
		t.Fatalf("phase = %v, want verifying", body["phase"])
	}

	// It walked queue -> start_download -> downloaded, and did NOT adopt: for
	// this want a transfer really was queued and really did run.
	if n := h.countRows(t,
		`SELECT count(*) FROM events WHERE subject_id = ? AND payload LIKE '%"transition":"adopt"%'`,
		id); n != 0 {
		t.Errorf("a want that was mid-pipeline adopted instead of walking its real edges")
	}
	if n := h.countRows(t,
		`SELECT count(*) FROM events WHERE subject_id = ? AND payload LIKE '%"transition":"queue"%'`,
		id); n != 1 {
		t.Errorf("%d queue events, want 1", n)
	}
}
