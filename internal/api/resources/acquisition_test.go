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
)

// Acquisition state on the wire (§64, M3-03).
//
// The claim under test is ADR-0027's: §64's twelve names are a presentation of
// four independent facts, and a client must be able to see the facts and not
// only the name. Showing only the name reintroduces at the edge exactly the
// collapse the storage model exists to prevent.

func acquisitionOf(t *testing.T, h *harness, id string) map[string]any {
	t.Helper()
	resp := h.get("/api/v1/desired/" + id)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	var got map[string]any
	if err := json.Unmarshal(h.body(resp), &got); err != nil {
		t.Fatal(err)
	}
	acq, ok := got["acquisition"].(map[string]any)
	if !ok {
		t.Fatalf("no acquisition state on the want: %v", got)
	}
	return acq
}

// A want and its acquisition state are created together. A want with no
// acquisition row is one the reconciliation sweep cannot advance and nothing
// would notice — it would sit there, wanted and never searched for.
func TestWantingCreatesAcquisitionState(t *testing.T) {
	h := newHarness(t).seed()
	resp := postDesired(t, h, `{
		"work": {"content_type":"movie","title":"Solaris","year":1972},
		"quality_profile":"living-room"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	created := decodeDesired(t, h, resp)
	id, _ := created["id"].(string)

	acq, ok := created["acquisition"].(map[string]any)
	if !ok {
		t.Fatal("the create response must carry the acquisition state it just created")
	}
	if acq["state"] != "MISSING" {
		t.Errorf("a fresh want is MISSING, got %v", acq["state"])
	}
	if acq["phase"] != "idle" || acq["managed"] != false {
		t.Errorf("a fresh want holds nothing and has nothing in flight, got %v", acq)
	}
	// Unknown, not not_satisfied: nobody has looked yet, and the two lead to
	// different actions.
	if acq["content"] != "unknown" || acq["placement"] != "unknown" {
		t.Errorf("a fresh want has been evaluated by nothing, got %v", acq)
	}

	// And it is there on the read path too, not only in the create response.
	if got := acquisitionOf(t, h, id); got["state"] != "MISSING" {
		t.Errorf("reading it back gave %v", got["state"])
	}

	if n := h.countRows(t, `SELECT count(*) FROM acquisition_state WHERE desired_item_id = ?`, id); n != 1 {
		t.Errorf("expected exactly one acquisition row, found %d", n)
	}
}

// The name is derived and BOTH axes are exposed. A client that can see
// CONTENT_SATISFIED but not which of §56's two questions was answered cannot
// tell "we have it" from "we have it everywhere".
func TestBothAxesAreOnTheWireAlongsideTheName(t *testing.T) {
	h := newHarness(t).seed()

	converging := acquisitionOf(t, h, desired1ID)
	if converging["state"] != "PLACEMENT_CONVERGING" {
		t.Fatalf("state = %v, want PLACEMENT_CONVERGING", converging["state"])
	}
	for _, field := range []string{"phase", "managed", "content", "placement"} {
		if _, present := converging[field]; !present {
			t.Errorf("%q is missing — the name alone reintroduces the collapse", field)
		}
	}
	if converging["content"] != "satisfied" || converging["placement"] != "converging" {
		t.Errorf("the axes do not match the name: %v", converging)
	}

	searching := acquisitionOf(t, h, desired2ID)
	if searching["state"] != "SEARCHING" {
		t.Errorf("state = %v, want SEARCHING", searching["state"])
	}
	if searching["managed"] != false {
		t.Errorf("a want holding nothing must say so: %v", searching)
	}
}

// The distinction the milestone epic names, over HTTP: obtaining usable content
// and replicating it to every required peer are different answers.
func TestContentSatisfiedAndFullySatisfiedAreDifferentOnTheWire(t *testing.T) {
	h := newHarness(t).seed()

	// Content satisfied, placement not yet answered.
	h.exec(`UPDATE acquisition_state SET content = 'satisfied', placement = 'unknown'
	         WHERE desired_item_id = ?`, desired1ID)
	if got := acquisitionOf(t, h, desired1ID)["state"]; got != "CONTENT_SATISFIED" {
		t.Fatalf("state = %v, want CONTENT_SATISFIED", got)
	}

	// Placement answered too.
	h.exec(`UPDATE acquisition_state SET placement = 'satisfied' WHERE desired_item_id = ?`, desired1ID)
	if got := acquisitionOf(t, h, desired1ID)["state"]; got != "FULLY_SATISFIED" {
		t.Fatalf("state = %v, want FULLY_SATISFIED", got)
	}

	// And placement regressing — a peer going away long after the bytes
	// arrived — moves the name back without touching content.
	h.exec(`UPDATE acquisition_state SET placement = 'converging' WHERE desired_item_id = ?`, desired1ID)
	after := acquisitionOf(t, h, desired1ID)
	if after["state"] != "PLACEMENT_CONVERGING" {
		t.Fatalf("state = %v, want PLACEMENT_CONVERGING", after["state"])
	}
	if after["content"] != "satisfied" {
		t.Error("placement regressing must not disturb the content answer")
	}
}

// AVAILABLE and CONTENT_SATISFIED are different: a 480p rip under a
// 1080p-minimum profile is available and not satisfied, and conflating them
// makes the upgrade workflow unreachable.
func TestAvailableIsNotContentSatisfiedOnTheWire(t *testing.T) {
	h := newHarness(t).seed()
	h.exec(`UPDATE acquisition_state SET phase = 'idle', managed = 1,
	          content = 'not_satisfied', placement = 'unknown' WHERE desired_item_id = ?`, desired1ID)
	got := acquisitionOf(t, h, desired1ID)
	if got["state"] != "AVAILABLE" {
		t.Fatalf("state = %v, want AVAILABLE", got["state"])
	}
	if got["managed"] != true {
		t.Error("AVAILABLE means bytes are held")
	}
	if got["content"] != "not_satisfied" {
		t.Error("and that they are not good enough")
	}
}

// A list of wants carries acquisition state without an N+1: fifty wants must
// not be fifty-one queries.
func TestListingWantsCarriesAcquisitionState(t *testing.T) {
	h := newHarness(t).seed()
	resp := h.get("/api/v1/desired")
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(h.body(resp), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) == 0 {
		t.Fatal("the fixture has wants")
	}
	for _, item := range page.Items {
		if _, ok := item["acquisition"].(map[string]any); !ok {
			t.Errorf("want %v has no acquisition state in the listing", item["id"])
		}
	}
}

// A want whose acquisition row is missing is still readable. The API says
// nothing about its state rather than hiding the want or inventing one.
func TestAWantWithNoAcquisitionRowIsStillReadable(t *testing.T) {
	h := newHarness(t).seed()
	h.exec(`DELETE FROM acquisition_state WHERE desired_item_id = ?`, desired1ID)

	resp := h.get("/api/v1/desired/" + desired1ID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a missing acquisition row must not hide the want",
			resp.StatusCode)
	}
	var got map[string]any
	if err := json.Unmarshal(h.body(resp), &got); err != nil {
		t.Fatal(err)
	}
	if _, present := got["acquisition"]; present {
		t.Error("with no row, the API should say nothing rather than invent a state")
	}
}

// Deleting a want takes its acquisition state with it.
func TestDeletingAWantCascadesToItsAcquisitionState(t *testing.T) {
	h := newHarness(t).seed()
	resp := postDesired(t, h, fmt.Sprintf(`{"work_id": %q, "quality_profile":"living-room"}`, work2ID))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	id, _ := decodeDesired(t, h, resp)["id"].(string)

	if d := h.doStable(http.MethodDelete, "/api/v1/desired/"+id, nil); d.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", d.StatusCode)
	}
	if n := h.countRows(t, `SELECT count(*) FROM acquisition_state WHERE desired_item_id = ?`, id); n != 0 {
		t.Errorf("%d acquisition row(s) outlived their want", n)
	}
}

// The impossible combination §56 forbids is refused by the DATABASE, not only
// by the domain — a repair script does not go through the validator.
func TestTheDatabaseRefusesPlacementWithoutContent(t *testing.T) {
	h := newHarness(t).seed()
	err := h.execErr(`UPDATE acquisition_state SET content = 'not_satisfied', placement = 'satisfied'
	                   WHERE desired_item_id = ?`, desired1ID)
	if err == nil {
		t.Fatal("bytes cannot be placed on peers before there are bytes worth placing (§56)")
	}
	if !strings.Contains(strings.ToUpper(err.Error()), "CONSTRAINT") {
		t.Errorf("expected a constraint failure, got %v", err)
	}
}
