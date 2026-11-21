// Every HTTP response in this file is closed by the t.Cleanup that the harness
// registers, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/resources"
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

// GET /api/v1/fan-out (#799) lists the siblings' acquire ops this site has an
// outcome for, including ones that never reached a want, and filters them.
func TestListFanOutOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed()
	op := seedFanOut(t, h)

	type listing struct {
		Items []struct {
			OpHash        string `json:"op_hash"`
			OriginSite    string `json:"origin_site"`
			ContentType   string `json:"content_type"`
			WorkKey       string `json:"work_key"`
			Outcome       string `json:"outcome"`
			DesiredItemID string `json:"desired_item_id"`
		} `json:"items"`
		Truncated bool `json:"truncated"`
	}
	read := func(query string) listing {
		t.Helper()
		resp := h.get("/api/v1/fan-out" + query)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d: %s", query, resp.StatusCode, h.body(resp))
		}
		var got listing
		if err := json.Unmarshal(h.body(resp), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	all := read("")
	if len(all.Items) != 1 || all.Truncated {
		t.Fatalf("fan-out = %+v, want the one op", all)
	}
	if it := all.Items[0]; it.OpHash != op.Hash || it.OriginSite != "site-b" || it.ContentType != "movie" ||
		it.WorkKey != "arrival|2016" || it.Outcome != "no_site_source" || it.DesiredItemID != desired1ID {
		t.Errorf("item = %+v", it)
	}
	if got := read("?origin_site=site-c"); len(got.Items) != 0 {
		t.Errorf("origin_site=site-c listed %+v", got.Items)
	}
	if got := read("?outcome=applied"); len(got.Items) != 0 {
		t.Errorf("outcome=applied listed %+v", got.Items)
	}
	if got := read("?limit=1"); len(got.Items) != 1 || !got.Truncated {
		t.Errorf("limit=1 = %+v, want one item and truncated", got)
	}
	for _, q := range []string{"?outcome=own", "?outcome=nonsense", "?limit=0", "?limit=501"} {
		if resp := h.get("/api/v1/fan-out" + q); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, resp.StatusCode)
		}
	}
}

// GET /desired/{id}/acquisition at the origin carries `published` (#799): the
// acquire op this site signed and the siblings that reported it unsourced. A
// want this site published nothing for has none.
func TestAcquisitionStatusAtTheOriginOverHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed()
	acq, report := seedPublished(t, h)
	stamp := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC).Format(time.RFC3339)

	type status struct {
		Published *struct {
			OpHash      string `json:"op_hash"`
			Infohash    string `json:"infohash"`
			Private     bool   `json:"private"`
			SelectedAt  string `json:"selected_at"`
			UnsourcedBy []struct {
				Site   string `json:"site"`
				OpHash string `json:"op_hash"`
			} `json:"unsourced_by"`
			Siblings []json.RawMessage `json:"siblings"`
		} `json:"published"`
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
	if got.Published == nil {
		t.Fatal("a want this site published has no published block")
	}
	if p := got.Published; p.OpHash != acq.Hash || p.Infohash != acq.Acquire.Infohash || !p.Private ||
		p.SelectedAt != stamp || len(p.UnsourcedBy) != 1 || p.UnsourcedBy[0].Site != "site-b" ||
		p.UnsourcedBy[0].OpHash != report.Hash {
		t.Errorf("published = %+v", *p)
	}
	if got.Published.Siblings != nil {
		t.Errorf("siblings = %s, want none on a node with no peer surface", got.Published.Siblings)
	}
	// desired2 wants the same work under another profile, at the same scope
	// and aspect: the op is keyed on those, not the profile, so it is the
	// same publication.
	if got := read(desired2ID); got.Published == nil || got.Published.OpHash != acq.Hash {
		t.Errorf("desired2: published = %+v, want the same op (same work, scope and aspect)", got.Published)
	}
}

// fakeSiblingFanOut answers with fixed outcomes, recording the op asked about.
type fakeSiblingFanOut struct {
	outcomes []resources.SiblingOutcome
	err      error
	asked    []string
}

func (f *fakeSiblingFanOut) Outcomes(_ context.Context, op string) ([]resources.SiblingOutcome, error) {
	f.asked = append(f.asked, op)
	return f.outcomes, f.err
}

// TestAcquisitionStatusShowsEachSiblingsOutcome (#924): the origin asks its
// siblings about the op it published, and status lists each one — the one
// that answered with its outcome, and the one that could not be asked as
// unknown and unreachable. Failing to list the siblings at all leaves them
// out rather than failing the read.
func TestAcquisitionStatusShowsEachSiblingsOutcome(t *testing.T) {
	t.Parallel()
	asked := time.Date(2026, 8, 2, 9, 30, 0, 0, time.UTC)
	fake := &fakeSiblingFanOut{outcomes: []resources.SiblingOutcome{
		{
			PeerID: "peer-b-id", Name: "peer-b", Site: "site-b", Reachable: true, Outcome: "applied",
			Detail: "from site site-a", AppliedAt: "2026-08-02T09:05:00Z", State: "DOWNLOADING", AskedAt: asked,
		},
		{PeerID: "peer-c-id", Name: "peer-c", Outcome: "unknown", Detail: "unreachable: connection refused", AskedAt: asked},
	}}
	h := newHarness(t, withSiblingFanOut(fake)).seed()
	acq, _ := seedPublished(t, h)

	type sibling struct {
		Peer      string `json:"peer"`
		PeerID    string `json:"peer_id"`
		Site      string `json:"site"`
		Reachable *bool  `json:"reachable"`
		Outcome   string `json:"outcome"`
		Detail    string `json:"detail"`
		AppliedAt string `json:"applied_at"`
		State     string `json:"state"`
		AskedAt   string `json:"asked_at"`
	}
	read := func() []sibling {
		t.Helper()
		resp := h.get("/api/v1/desired/" + desired1ID + "/acquisition")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
		}
		var got struct {
			Published *struct {
				Siblings []sibling `json:"siblings"`
			} `json:"published"`
		}
		if err := json.Unmarshal(h.body(resp), &got); err != nil {
			t.Fatal(err)
		}
		if got.Published == nil {
			t.Fatal("no published block")
		}
		return got.Published.Siblings
	}
	sibs := read()
	if len(fake.asked) != 1 || fake.asked[0] != acq.Hash {
		t.Errorf("asked the siblings about %v, want the published op %s", fake.asked, acq.Hash)
	}
	if len(sibs) != 2 {
		t.Fatalf("siblings = %+v, want both", sibs)
	}
	if s := sibs[0]; s.Peer != "peer-b" || s.PeerID != "peer-b-id" || s.Site != "site-b" ||
		s.Reachable == nil || !*s.Reachable || s.Outcome != "applied" || s.State != "DOWNLOADING" ||
		s.AppliedAt != "2026-08-02T09:05:00Z" || s.AskedAt != "2026-08-02T09:30:00Z" {
		t.Errorf("the reachable sibling = %+v", s)
	}
	if s := sibs[1]; s.Peer != "peer-c" || s.Reachable == nil || *s.Reachable || s.Outcome != "unknown" ||
		s.Site != "" || s.Detail == "" {
		t.Errorf("the unreachable sibling = %+v, want unknown and reachable false", s)
	}

	fake.outcomes, fake.err = nil, errors.New("listing peers: database is locked")
	if sibs := read(); sibs != nil {
		t.Errorf("siblings = %+v, want none when they cannot be listed", sibs)
	}
}

// seedPublished makes this site the origin of an acquire op for desired1ID's
// work, which sibling site-b reported unsourced.
func seedPublished(t *testing.T, h *harness) (acq, report catalogop.Op) {
	t.Helper()
	selfPub, selfPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sibPub, sibPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
	stamp := at.Format(time.RFC3339)
	h.exec(`UPDATE peers SET public_key = ? WHERE is_self = 1`, []byte(selfPub))
	h.exec(`INSERT INTO peers (id, name, site, mode, is_self, public_key, enrolled_at, created_at)
		VALUES ('sibling-peer', 'peer-b', 'site-b', 'partial', 0, ?, ?, ?)`, []byte(sibPub), stamp, stamp)
	log := func(tok string, err error, kind, prev string) catalogop.Op {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		op, err := catalogop.Verify(tok)
		if err != nil {
			t.Fatal(err)
		}
		h.exec(`INSERT INTO catalog_ops (op_hash, content_type, work_key, op, signer, prev, iat, token, received_at)
			VALUES (?, 'movie', 'arrival|2016', ?, ?, ?, ?, ?, ?)`, op.Hash, kind, op.By, prev, stamp, op.Token, stamp)
		return op
	}
	tok, err := catalogop.SignAcquire(selfPriv, "movie", "arrival|2016", catalogop.Acquire{
		Scope: catalogop.ScopeWork, Aspect: catalogop.AspectPrimary, Profile: "living-room",
		Infohash: "0123456789abcdef0123456789abcdef01234567", Title: "Arrival 2016 2160p", Private: true,
		WorkTitle: "Arrival", Year: 2016, SelectedAt: at, Site: "site-a",
	}, nil, at)
	acq = log(tok, err, "acquire", "[]")
	tok, err = catalogop.SignUnsourced(sibPriv, "movie", "arrival|2016",
		catalogop.Unsourced{Acquire: acq.Hash, Site: "site-b"}, []string{acq.Hash}, at)
	report = log(tok, err, "unsourced", `["`+acq.Hash+`"]`)
	return acq, report
}
