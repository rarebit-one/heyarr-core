//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The external identifiers ADR-0050 already stores are readable over REST on the
// two detail routes (#431). The shapes themselves are golden files; these are
// the assertions a golden cannot make.

// A work's stored identifiers are keyed by source, so a client reads
// `external_ids.tmdb` rather than scanning a list.
func TestWorkDetailCarriesExternalIDs(t *testing.T) {
	h := newHarness(t).seed()

	var got struct {
		ID          string            `json:"id"`
		ExternalIDs map[string]string `json:"external_ids"`
	}
	resp := h.get("/api/v1/works/" + work1ID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if err := json.Unmarshal(h.body(resp), &got); err != nil {
		t.Fatal(err)
	}
	if got.ExternalIDs["tmdb"] != "329865" || got.ExternalIDs["imdb"] != "tt2543164" {
		t.Errorf("external_ids = %v, want tmdb and imdb", got.ExternalIDs)
	}
}

// An edition is the other entity type external_ids rows may name, and the rows
// do not leak across entity types: the edition's tvdb id is not the work's.
func TestEditionDetailCarriesItsOwnExternalIDs(t *testing.T) {
	h := newHarness(t).seed()

	var edition struct {
		ExternalIDs map[string]string `json:"external_ids"`
	}
	resp := h.get("/api/v1/editions/" + edition1ID)
	if err := json.Unmarshal(h.body(resp), &edition); err != nil {
		t.Fatal(err)
	}
	if got := edition.ExternalIDs["tvdb"]; got != "424242" {
		t.Errorf("edition external_ids[tvdb] = %q, want 424242", got)
	}

	var work struct {
		ExternalIDs map[string]string `json:"external_ids"`
	}
	if err := json.Unmarshal(h.body(h.get("/api/v1/works/"+work1ID)), &work); err != nil {
		t.Fatal(err)
	}
	if _, leaked := work.ExternalIDs["tvdb"]; leaked {
		t.Errorf("the edition's tvdb id leaked onto its work: %v", work.ExternalIDs)
	}
}

// A work nobody has reconciled answers with an EMPTY object, never a null and
// never an omitted key: ADR-0025's "no match, never an error" applied to a
// catalogue identifier, and the shape a client can read without a nil check.
func TestExternalIDsAreEmptyRatherThanAbsent(t *testing.T) {
	h := newHarness(t).seed()

	raw := h.body(h.get("/api/v1/works/" + work2ID))
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	got, ok := doc["external_ids"]
	if !ok {
		t.Fatalf("external_ids is absent from %s", raw)
	}
	if string(got) != "{}" {
		t.Errorf("external_ids = %s, want {}", got)
	}
}

// Listing works does NOT carry external ids: they are a per-item join, and a
// listing that carried them would pay one lookup per row for a field the list
// screen does not show.
func TestWorkListingOmitsExternalIDs(t *testing.T) {
	h := newHarness(t).seed()

	var page struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(h.body(h.get("/api/v1/works")), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) == 0 {
		t.Fatal("no works listed")
	}
	for _, item := range page.Items {
		if _, present := item["external_ids"]; present {
			t.Errorf("a listed work carries external_ids: %v", item)
		}
	}
}
