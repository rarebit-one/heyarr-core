//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package mcp_test

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/catalogop"
)

// logSiblingAcquire signs an acquire op from a sibling at site, puts it in the
// op-log and records this site's outcome for it, as apply_acquire would.
func (h *harness) logSiblingAcquire(site, scope, infohash, wantID, outcome, detail string) catalogop.Op {
	h.t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		h.t.Fatal(err)
	}
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	tok, err := catalogop.SignAcquire(priv, "movie", "movie:arrival:2016", catalogop.Acquire{
		Scope: scope, Aspect: catalogop.AspectPrimary, Profile: "living-room",
		Infohash: infohash, Title: "Arrival 2016 2160p", WorkTitle: "Arrival", Year: 2016,
		Size: 12_000_000_000, SelectedAt: at, Site: site,
	}, nil, at)
	if err != nil {
		h.t.Fatal(err)
	}
	op, err := catalogop.Verify(tok)
	if err != nil {
		h.t.Fatal(err)
	}
	stamp := at.Format(time.RFC3339)
	h.exec(`INSERT INTO catalog_ops (op_hash, content_type, work_key, op, signer, prev, iat, token, received_at)
		VALUES (?, 'movie', 'movie:arrival:2016', 'acquire', ?, '[]', ?, ?, ?)`, op.Hash, op.By, stamp, op.Token, stamp)
	var want any
	if wantID != "" {
		want = wantID
	}
	h.exec(`INSERT INTO catalog_op_applications (op_hash, desired_item_id, outcome, detail, applied_at)
		VALUES (?, ?, ?, ?, ?)`, op.Hash, want, outcome, detail, stamp)
	return op
}

// list_fan_out (#799) shows every sibling acquire op this site has an outcome
// for, including the ones that never reached a want, and filters them.
func TestListFanOut(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false)
	id := h.wantOne("")
	applied := h.logSiblingAcquire("site-b", catalogop.ScopeWork,
		"0123456789abcdef0123456789abcdef01234567", id, "applied", "from site site-b")
	unresolved := h.logSiblingAcquire("site-c", "item:s01e04",
		"1111111111111111111111111111111111111111", "", "scope_unresolved", "item s01e04 is not known here")

	type entry struct {
		OpHash        string `json:"op_hash"`
		OriginSite    string `json:"origin_site"`
		WorkKey       string `json:"work_key"`
		Outcome       string `json:"outcome"`
		Detail        string `json:"detail"`
		DesiredItemID string `json:"desired_item_id"`
	}
	type listing struct {
		Count     int     `json:"count"`
		Truncated bool    `json:"truncated"`
		FanOut    []entry `json:"fan_out"`
	}

	var all listing
	h.call("", "list_fan_out", `{}`).structured(t, &all)
	if all.Count != 2 || len(all.FanOut) != 2 || all.Truncated {
		t.Fatalf("list_fan_out = %+v, want both ops, untruncated", all)
	}
	got := map[string]entry{}
	for _, e := range all.FanOut {
		got[e.OpHash] = e
	}
	if e := got[unresolved.Hash]; e.Outcome != "scope_unresolved" || e.OriginSite != "site-c" ||
		e.DesiredItemID != "" || e.WorkKey != "movie:arrival:2016" || e.Detail == "" {
		t.Errorf("scope_unresolved op = %+v, want it listed with no want", e)
	}
	if e := got[applied.Hash]; e.Outcome != "applied" || e.DesiredItemID != id {
		t.Errorf("applied op = %+v, want it to name the want %s", e, id)
	}

	var bySite listing
	h.call("", "list_fan_out", `{"origin_site":"site-b"}`).structured(t, &bySite)
	if len(bySite.FanOut) != 1 || bySite.FanOut[0].OpHash != applied.Hash {
		t.Errorf("origin_site=site-b listed %+v", bySite.FanOut)
	}
	var byOutcome listing
	h.call("", "list_fan_out", `{"outcome":"scope_unresolved"}`).structured(t, &byOutcome)
	if len(byOutcome.FanOut) != 1 || byOutcome.FanOut[0].OpHash != unresolved.Hash {
		t.Errorf("outcome=scope_unresolved listed %+v", byOutcome.FanOut)
	}
	var limited listing
	h.call("", "list_fan_out", `{"limit":1}`).structured(t, &limited)
	if len(limited.FanOut) != 1 || !limited.Truncated {
		t.Errorf("limit 1 = %+v, want one op and truncated", limited)
	}

	if resp := h.call("", "list_fan_out", `{"outcome":"own"}`); resp.Body.Error == nil {
		t.Error("outcome own is this site's own op, not fan-out, and should be refused")
	}
}

// get_acquisition_status carries the fan_out block for a want a sibling's
// choice created: the origin, the release and this site's outcome (#799).
func TestGetAcquisitionStatusOfAFanOutWant(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false)
	id := h.wantOne("")
	op := h.logSiblingAcquire("site-b", catalogop.ScopeWork,
		"0123456789abcdef0123456789abcdef01234567", id, "no_site_source",
		"from site site-b; the release did not fetch here and is blocked for this want")

	var out struct {
		FanOut *struct {
			OriginSite        string `json:"origin_site"`
			OpHash            string `json:"op_hash"`
			ReleaseName       string `json:"release_name"`
			Outcome           string `json:"outcome"`
			Detail            string `json:"detail"`
			ReportedUnsourced bool   `json:"reported_unsourced"`
		} `json:"fan_out"`
	}
	h.call("", "get_acquisition_status", `{"desired_item_id":"`+id+`"}`).structured(t, &out)
	if out.FanOut == nil {
		t.Fatal("a want a sibling's op was applied to has no fan_out block")
	}
	if fo := out.FanOut; fo.OriginSite != "site-b" || fo.OpHash != op.Hash || fo.Outcome != "no_site_source" ||
		fo.ReleaseName != "Arrival 2016 2160p" || fo.Detail == "" || fo.ReportedUnsourced {
		t.Errorf("fan_out = %+v", *fo)
	}
}

// get_acquisition_status at the origin carries `published` (#799): the acquire
// op this site signed for the want and the sibling sites that reported they
// could not source it.
func TestGetAcquisitionStatusAtTheOrigin(t *testing.T) {
	t.Parallel()
	h := newHarness(t, false)
	id := h.wantOne("")

	selfPub, selfPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	sibPub, sibPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	stamp := at.Format(time.RFC3339)
	h.exec(`UPDATE peers SET public_key = ? WHERE is_self = 1`, []byte(selfPub))
	h.exec(`INSERT INTO peers (id, name, site, mode, is_self, public_key, created_at, enrolled_at)
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
			VALUES (?, 'movie', 'movie:arrival:2016', ?, ?, ?, ?, ?, ?)`, op.Hash, kind, op.By, prev, stamp, op.Token, stamp)
		return op
	}
	tok, err := catalogop.SignAcquire(selfPriv, "movie", "movie:arrival:2016", catalogop.Acquire{
		Scope: catalogop.ScopeWork, Aspect: catalogop.AspectPrimary, Profile: "living-room",
		Infohash: "0123456789abcdef0123456789abcdef01234567", Title: "Arrival 2016 2160p",
		WorkTitle: "Arrival", Year: 2016, SelectedAt: at, Site: "site-a",
	}, nil, at)
	acq := log(tok, err, "acquire", "[]")
	tok, err = catalogop.SignUnsourced(sibPriv, "movie", "movie:arrival:2016",
		catalogop.Unsourced{Acquire: acq.Hash, Site: "site-b"}, []string{acq.Hash}, at)
	log(tok, err, "unsourced", `["`+acq.Hash+`"]`)

	var out struct {
		FanOut    *struct{} `json:"fan_out"`
		Published *struct {
			OpHash      string `json:"op_hash"`
			ReleaseName string `json:"release_name"`
			UnsourcedBy []struct {
				Site string `json:"site"`
			} `json:"unsourced_by"`
		} `json:"published"`
	}
	h.call("", "get_acquisition_status", `{"desired_item_id":"`+id+`"}`).structured(t, &out)
	if out.FanOut != nil {
		t.Error("the origin's own want reported a fan_out block")
	}
	if out.Published == nil {
		t.Fatal("a want this site published has no published block")
	}
	if p := out.Published; p.OpHash != acq.Hash || p.ReleaseName != "Arrival 2016 2160p" ||
		len(p.UnsourcedBy) != 1 || p.UnsourcedBy[0].Site != "site-b" {
		t.Errorf("published = %+v", *p)
	}
}
