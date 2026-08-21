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

// The upgrade workflow on the wire (§60, M3-06).
//
// The decision is table-tested in the domain and the queries in the catalog.
// What these add is the join: a real want, a real profile, and the two
// questions a client actually asks — "which of my wants could be better" and
// "why is this one not upgrading".

// upgradeOf reads the upgrade block from the satisfaction endpoint.
func upgradeOf(t *testing.T, h *harness, id string) map[string]any {
	t.Helper()
	resp := h.get("/api/v1/desired/" + id + "/satisfaction")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	var got map[string]any
	if err := json.Unmarshal(h.body(resp), &got); err != nil {
		t.Fatal(err)
	}
	up, ok := got["upgrade"].(map[string]any)
	if !ok {
		t.Fatalf("no upgrade block on the satisfaction response: %v", got)
	}
	return up
}

// listUpgradable returns the ids the upgradable filter selects.
func listUpgradable(t *testing.T, h *harness, value string) []string {
	t.Helper()
	resp := h.get("/api/v1/desired?upgradable=" + value)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	var page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(h.body(resp), &page); err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(page.Items))
	for _, i := range page.Items {
		out = append(out, i.ID)
	}
	return out
}

// The four disqualifying reasons, each reported distinctly. A client that only
// learns "not upgradable" cannot tell an operator anything useful.
func TestUpgradeStatusIsReportedPerWant(t *testing.T) {
	h := newHarness(t).seed()

	// The seeded fixture has two wants over work1: desired1ID is monitored
	// with content satisfied and placement converging; desired2ID is
	// unmonitored and searching.
	t.Run("an unmonitored want reports the operator's decision", func(t *testing.T) {
		got := upgradeOf(t, h, desired2ID)
		if got["status"] != "not_monitored" {
			t.Errorf("status = %v, want not_monitored (%v)", got["status"], got["detail"])
		}
		if got["eligible"] != false {
			t.Error("an unmonitored want is not eligible")
		}
	})

	t.Run("every status carries an explanation", func(t *testing.T) {
		for _, id := range []string{desired1ID, desired2ID} {
			got := upgradeOf(t, h, id)
			if d, _ := got["detail"].(string); d == "" {
				t.Errorf("%s: status %v with no explanation", id, got["status"])
			}
		}
	})
}

// A want with nothing acceptable held is an ACQUISITION, not an upgrade.
func TestAnUnsatisfiedWantIsNotAnUpgrade(t *testing.T) {
	h := newHarness(t).seed()

	// A fresh want for content nothing holds.
	resp := postDesired(t, h, `{
		"work": {"content_type":"movie","title":"Nothing Here","year":1970},
		"quality_profile":"living-room"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	id, _ := decodeDesired(t, h, resp)["id"].(string)

	got := upgradeOf(t, h, id)
	if got["status"] != "not_satisfied" {
		t.Errorf("status = %v, want not_satisfied (%v)", got["status"], got["detail"])
	}
	if got["eligible"] != false {
		t.Error("a want holding nothing is not upgradable — it is an acquisition")
	}
}

// The listing filter §71's get_upgrade_candidates will expose.
func TestUpgradableFilter(t *testing.T) {
	h := newHarness(t).seed()

	upgradable := listUpgradable(t, h, "true")
	notUpgradable := listUpgradable(t, h, "false")

	// desired1ID is monitored, satisfied and managed; desired2ID is
	// unmonitored and holds nothing.
	if !contains(upgradable, desired1ID) {
		t.Errorf("a monitored, satisfied want is missing from upgradable=true: %v", upgradable)
	}
	if contains(upgradable, desired2ID) {
		t.Errorf("an unmonitored want appeared in upgradable=true: %v", upgradable)
	}
	if !contains(notUpgradable, desired2ID) {
		t.Errorf("an unmonitored want is missing from upgradable=false: %v", notUpgradable)
	}

	// The two halves partition the library: every want is in exactly one.
	total := h.countRows(t, `SELECT count(*) FROM desired_items`)
	if len(upgradable)+len(notUpgradable) != total {
		t.Errorf("upgradable=true (%d) and =false (%d) do not partition %d wants",
			len(upgradable), len(notUpgradable), total)
	}
	for _, id := range upgradable {
		if contains(notUpgradable, id) {
			t.Errorf("%s is in both halves", id)
		}
	}
}

// Turning monitoring off takes a want out of the upgradable set immediately —
// the operator's instruction, honoured without waiting for a beat.
func TestUnmonitoringRemovesAWantFromTheUpgradableSet(t *testing.T) {
	h := newHarness(t).seed()

	if !contains(listUpgradable(t, h, "true"), desired1ID) {
		t.Fatal("setup: the seeded want should be upgradable")
	}

	resp := h.doStable(http.MethodPatch, "/api/v1/desired/"+desired1ID,
		strings.NewReader(`{"monitor":false}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}

	if contains(listUpgradable(t, h, "true"), desired1ID) {
		t.Error("an unmonitored want is still reported upgradable")
	}
	if got := upgradeOf(t, h, desired1ID); got["status"] != "not_monitored" {
		t.Errorf("status = %v, want not_monitored", got["status"])
	}
}

// An unknown value is refused rather than silently ignored — an ignored filter
// silently returns everything, which reads as "nothing is upgradable" or
// "everything is", depending on which way the caller was hoping.
func TestUpgradableRefusesAnUnknownValue(t *testing.T) {
	h := newHarness(t).seed()
	resp := h.doStable(http.MethodGet, "/api/v1/desired?upgradable=maybe", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, h.body(resp))
	}
	if detail := problemDetail(t, h, resp); !strings.Contains(detail, "upgradable") {
		t.Errorf("the refusal should name the parameter; got: %s", detail)
	}
}

// Reading the upgrade block writes nothing. It is a question, not an
// instruction.
func TestReadingTheUpgradeBlockEmitsNothing(t *testing.T) {
	h := newHarness(t).seed()
	before := h.eventsOfType(t, "acquisition.upgrade_found", "acquisition.upgrade_superseded")
	for range 3 {
		upgradeOf(t, h, desired1ID)
	}
	if got := h.eventsOfType(t,
		"acquisition.upgrade_found", "acquisition.upgrade_superseded"); got != before {
		t.Errorf("reading the upgrade block emitted %d event(s)", got-before)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
