// Every HTTP response in this file is closed by the t.Cleanup that the harness
// registers, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/catalogop"
)

// Fan-out on the HTTP surface (ADR-0119, #799): a want a sibling's choice
// created says so, the listing filters on it, and GET /desired/{id}/acquisition
// says what this site did with the sibling's acquire op.

// seedFanOut makes desired1ID a want fan-out created from site-b's acquire op,
// which this site could not source.
func seedFanOut(t *testing.T, h *harness) catalogop.Op {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
	tok, err := catalogop.SignAcquire(priv, "movie", "arrival|2016", catalogop.Acquire{
		Scope: catalogop.ScopeWork, Aspect: catalogop.AspectPrimary, Profile: "living-room",
		Infohash: "0123456789abcdef0123456789abcdef01234567", Title: "Arrival 2016 2160p", Private: true,
		WorkTitle: "Arrival", Year: 2016, Size: 12_000_000_000, SelectedAt: at, Site: "site-b",
	}, nil, at)
	if err != nil {
		t.Fatal(err)
	}
	op, err := catalogop.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	stamp := at.Format(time.RFC3339)
	h.exec(`INSERT INTO catalog_ops (op_hash, content_type, work_key, op, signer, prev, iat, token, received_at)
		VALUES (?, 'movie', 'arrival|2016', 'acquire', ?, '[]', ?, ?, ?)`, op.Hash, op.By, stamp, op.Token, stamp)
	h.exec(`INSERT INTO catalog_op_applications (op_hash, desired_item_id, outcome, detail, applied_at)
		VALUES (?, ?, 'no_site_source', 'no indexer here lists this private release from site site-b', ?)`,
		op.Hash, desired1ID, stamp)
	h.exec(`INSERT INTO fan_out_wants (desired_item_id, origin_site, created_at) VALUES (?, 'site-b', ?)`,
		desired1ID, stamp)
	return op
}

func TestAWantFanOutCreatedNamesItsOriginSite(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed()
	seedFanOut(t, h)

	for id, want := range map[string]any{desired1ID: "site-b", desired2ID: nil} {
		resp := h.get("/api/v1/desired/" + id)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
		}
		var got map[string]any
		if err := json.Unmarshal(h.body(resp), &got); err != nil {
			t.Fatal(err)
		}
		if got["origin_site"] != want {
			t.Errorf("%s: origin_site = %v, want %v", id, got["origin_site"], want)
		}
	}
}

func TestListingWantsFiltersByFanOutOrigin(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed()
	seedFanOut(t, h)

	for _, tc := range []struct {
		query string
		has   map[string]bool
	}{
		{"fan_out=true", map[string]bool{desired1ID: true, desired2ID: false}},
		{"fan_out=false", map[string]bool{desired1ID: false, desired2ID: true}},
		{"origin_site=site-b", map[string]bool{desired1ID: true, desired2ID: false}},
		{"origin_site=site-c", map[string]bool{desired1ID: false, desired2ID: false}},
	} {
		resp := h.get("/api/v1/desired?" + tc.query)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", tc.query, resp.StatusCode, h.body(resp))
		}
		var page struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		if err := json.Unmarshal(h.body(resp), &page); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, it := range page.Items {
			seen[it.ID] = true
		}
		for id, want := range tc.has {
			if seen[id] != want {
				t.Errorf("%s: listed %s = %v, want %v", tc.query, id, seen[id], want)
			}
		}
	}

	if resp := h.get("/api/v1/desired?fan_out=maybe"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("fan_out=maybe: status = %d, want 400", resp.StatusCode)
	}
}

func TestAcquisitionStatusOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed()
	op := seedFanOut(t, h)

	type status struct {
		DesiredItemID string `json:"desired_item_id"`
		Phase         string `json:"phase"`
		State         string `json:"state"`
		FanOut        *struct {
			OriginSite        string `json:"origin_site"`
			OpHash            string `json:"op_hash"`
			Infohash          string `json:"infohash"`
			ReleaseName       string `json:"release_name"`
			Outcome           string `json:"outcome"`
			Detail            string `json:"detail"`
			ReportedUnsourced bool   `json:"reported_unsourced"`
		} `json:"fan_out"`
	}
	read := func(id string) status {
		t.Helper()
		resp := h.get("/api/v1/desired/" + id + "/acquisition")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
		}
		var got status
		if err := json.Unmarshal(h.body(resp), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	got := read(desired1ID)
	if got.DesiredItemID != desired1ID || got.Phase == "" || got.State == "" {
		t.Errorf("status = %+v, want the want's pipeline position", got)
	}
	if got.FanOut == nil {
		t.Fatal("a want fan-out created has no fan_out block")
	}
	fo := got.FanOut
	if fo.OriginSite != "site-b" || fo.OpHash != op.Hash || fo.Infohash != op.Acquire.Infohash ||
		fo.ReleaseName != "Arrival 2016 2160p" || fo.Outcome != "no_site_source" ||
		fo.Detail == "" || fo.ReportedUnsourced {
		t.Errorf("fan_out = %+v", *fo)
	}

	if got := read(desired2ID); got.FanOut != nil {
		t.Errorf("a want this site created has fan_out %+v", *got.FanOut)
	}

	if resp := h.get("/api/v1/desired/no-such-want/acquisition"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown want: status = %d, want 404", resp.StatusCode)
	}
}
