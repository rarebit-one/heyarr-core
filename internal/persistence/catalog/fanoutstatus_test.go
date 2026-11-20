package catalog_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/catalogop"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// TestIsSiblingKey: only a member other than this node counts, found by key.
// A stranger, this node itself and a malformed key do not.
func TestIsSiblingKey(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	sibling := h.seedSibling(t)
	strangerPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	selfPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h.exec(t, `UPDATE peers SET public_key = ? WHERE is_self = 1`, []byte(selfPub))

	for key, want := range map[string]bool{
		sibling:                               true,
		identity.FormatPublicKey(strangerPub): false,
		identity.FormatPublicKey(selfPub):     false,
		"not-a-key":                           false,
	} {
		got, err := h.cat.IsSiblingKey(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("IsSiblingKey(%q) = %v, want %v", key, got, want)
		}
	}
}

// A want fan-out did not create has no fan-out status.
func TestFanOutStatusForALocalWant(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if _, found, err := h.cat.FanOutStatusFor(context.Background(), "no-such-want"); err != nil || found {
		t.Fatalf("FanOutStatusFor = found %v, err %v; want not found", found, err)
	}
}

// logOp puts a signed op into the harness's op-log, as catalog sync would.
func (h *harness) logOp(t *testing.T, op catalogop.Op, kind string, prev string) {
	t.Helper()
	at := op.IssuedAt.UTC().Format(time.RFC3339)
	h.exec(t, `INSERT INTO catalog_ops (op_hash, content_type, work_key, op, signer, prev, iat, token, received_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		op.Hash, op.Target.ContentType, op.Target.WorkKey, kind, op.By, prev, at, op.Token, at)
}

// TestListFanOutShowsOutcomesWithNoWant (#799): every sibling acquire op this
// node has an outcome for is listed, newest first, including the ones that
// never reached a want; this node's own op is not fan-out and is left out.
// Outcome, site and limit filter it, and an op this node reported unsourced
// says so.
func TestListFanOutShowsOutcomesWithNoWant(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	selfPub, selfPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h.exec(t, `INSERT OR IGNORE INTO peers (id, name, site, mode, is_self, public_key, created_at, enrolled_at)
		VALUES ('self-peer', 'self', 'home', 'full', 1, ?, ?, ?)`, []byte(selfPub), stamp, stamp)
	h.exec(t, `UPDATE peers SET public_key = ? WHERE is_self = 1`, []byte(selfPub))

	north := arrivalAcquire()
	applied := acquireOp(t, "movie", "movie:arrival:2016", north)
	south := arrivalAcquire()
	south.Site, south.Scope, south.Infohash = "south", "item:s01e04", "1111111111111111111111111111111111111111"
	unresolved := acquireOp(t, "movie", "movie:arrival:2016", south)
	private := arrivalAcquire()
	private.Private, private.Infohash, private.Title = true, "2222222222222222222222222222222222222222", "Arrival 2016 1080p"
	unsourced := acquireOp(t, "movie", "movie:arrival:2016", private)
	ownTok, err := catalogop.SignAcquire(selfPriv, "movie", "movie:arrival:2016", arrivalAcquire(), nil, fanOutAt)
	if err != nil {
		t.Fatal(err)
	}
	own, err := catalogop.Verify(ownTok)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []catalogop.Op{applied, unresolved, unsourced, own} {
		h.logOp(t, op, "acquire", "[]")
	}
	rep, err := catalogop.SignUnsourced(selfPriv, "movie", "movie:arrival:2016",
		catalogop.Unsourced{Acquire: unsourced.Hash, Site: "home"}, []string{unsourced.Hash}, fanOutAt)
	if err != nil {
		t.Fatal(err)
	}
	repOp, err := catalogop.Verify(rep)
	if err != nil {
		t.Fatal(err)
	}
	h.logOp(t, repOp, "unsourced", `["`+unsourced.Hash+`"]`)

	for _, rec := range []struct {
		op      catalogop.Op
		want    string
		outcome catalog.FanOutOutcome
		detail  string
	}{
		{own, "", catalog.FanOutOwn, "signed by this peer"},
		{unresolved, "", catalog.FanOutScopeUnresolved, "item s01e04 is not known here"},
		{unsourced, "", catalog.FanOutNoSiteSource, "a private release publishes no trackers"},
		{applied, h.want, catalog.FanOutApplied, "from site north"},
	} {
		if err := h.cat.RecordFanOutApplication(ctx, rec.op.Hash, rec.want, rec.outcome, rec.detail); err != nil {
			t.Fatal(err)
		}
	}

	hashes := func(es []catalog.FanOutEntry) []string {
		var out []string
		for _, e := range es {
			out = append(out, e.OpHash)
		}
		return out
	}
	all, err := h.cat.ListFanOut(ctx, catalog.FanOutQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("listed %v, want the three sibling ops and not this node's own", hashes(all))
	}
	byHash := map[string]catalog.FanOutEntry{}
	for _, e := range all {
		byHash[e.OpHash] = e
	}
	if e := byHash[unresolved.Hash]; e.Outcome != catalog.FanOutScopeUnresolved || e.DesiredItemID != "" ||
		e.Site != "south" || e.Scope != "item:s01e04" || e.WorkKey != "movie:arrival:2016" ||
		e.ContentType != "movie" || e.WorkTitle != "Arrival" || e.Detail == "" || e.Reported {
		t.Errorf("scope_unresolved entry = %+v", e)
	}
	if e := byHash[unsourced.Hash]; e.Outcome != catalog.FanOutNoSiteSource || !e.Reported ||
		e.ReleaseTitle != "Arrival 2016 1080p" {
		t.Errorf("no_site_source entry = %+v, want it reported unsourced", e)
	}
	if e := byHash[applied.Hash]; e.Outcome != catalog.FanOutApplied || e.DesiredItemID != h.want ||
		e.Infohash != north.Infohash || !e.SelectedAt.Equal(fanOutAt) || e.AppliedAt.IsZero() {
		t.Errorf("applied entry = %+v", e)
	}

	for _, tc := range []struct {
		name string
		q    catalog.FanOutQuery
		want int
	}{
		{"by outcome", catalog.FanOutQuery{Outcome: catalog.FanOutScopeUnresolved}, 1},
		{"own is never fan-out", catalog.FanOutQuery{Outcome: catalog.FanOutOwn}, 0},
		{"by site", catalog.FanOutQuery{Site: "north"}, 2},
		{"by site and outcome", catalog.FanOutQuery{Site: "north", Outcome: catalog.FanOutApplied}, 1},
		{"an unknown site", catalog.FanOutQuery{Site: "west"}, 0},
		{"limited", catalog.FanOutQuery{Limit: 2}, 2},
	} {
		got, err := h.cat.ListFanOut(ctx, tc.q)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != tc.want {
			t.Errorf("%s: listed %v, want %d", tc.name, hashes(got), tc.want)
		}
	}

	// The want the applied op reached reads the same op back.
	fo, found, err := h.cat.FanOutStatusFor(ctx, h.want)
	if err != nil || !found {
		t.Fatalf("FanOutStatusFor = found %v, err %v", found, err)
	}
	if fo.OpHash != applied.Hash || fo.Site != "north" || fo.Outcome != catalog.FanOutApplied || fo.Reported {
		t.Errorf("FanOutStatusFor = %+v", fo)
	}
}

// TestPublishedForShowsTheOriginsSide (#799): at the origin, a want shows the
// newest acquire op this node signed for it, the current siblings that
// reported it unsourced (a stranger's report is not one), and the holds this
// node published for the work. A want this node published nothing for has no
// such side.
func TestPublishedForShowsTheOriginsSide(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	if _, found, err := h.cat.PublishedFor(ctx, h.want); err != nil || found {
		t.Fatalf("before publishing: found %v, err %v; want not found", found, err)
	}

	selfPub, selfPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h.exec(t, `INSERT OR IGNORE INTO peers (id, name, site, mode, is_self, public_key, created_at, enrolled_at)
		VALUES ('self-peer', 'self', 'home', 'full', 1, ?, ?, ?)`, []byte(selfPub), stamp, stamp)
	h.exec(t, `UPDATE peers SET public_key = ? WHERE is_self = 1`, []byte(selfPub))
	sibPub, sibPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h.exec(t, `INSERT INTO peers (id, name, site, mode, is_self, public_key, created_at, enrolled_at)
		VALUES (?, 'sibling', 'site-b', 'partial', 0, ?, ?, ?)`, remotePeer, []byte(sibPub), stamp, stamp)
	_, strangerPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sign := func(tok string, err error) catalogop.Op {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		op, err := catalogop.Verify(tok)
		if err != nil {
			t.Fatal(err)
		}
		return op
	}

	older := arrivalAcquire()
	older.SelectedAt = fanOutAt.Add(-time.Hour)
	older.Infohash = "3333333333333333333333333333333333333333"
	newer := arrivalAcquire()
	newer.Site = "home"
	subtitle := arrivalAcquire()
	subtitle.Scope, subtitle.Aspect = catalogop.ScopeEditionPrefix+"default", "subtitle:fr"
	oldOp := sign(catalogop.SignAcquire(selfPriv, "movie", "movie:arrival:2016", older, nil, fanOutAt))
	newOp := sign(catalogop.SignAcquire(selfPriv, "movie", "movie:arrival:2016", newer, nil, fanOutAt))
	subOp := sign(catalogop.SignAcquire(selfPriv, "movie", "movie:arrival:2016", subtitle, nil, fanOutAt))
	for _, op := range []catalogop.Op{oldOp, newOp, subOp} {
		h.logOp(t, op, "acquire", "[]")
	}
	report := sign(catalogop.SignUnsourced(sibPriv, "movie", "movie:arrival:2016",
		catalogop.Unsourced{Acquire: newOp.Hash, Site: "site-b"}, []string{newOp.Hash}, fanOutAt))
	stranger := sign(catalogop.SignUnsourced(strangerPriv, "movie", "movie:arrival:2016",
		catalogop.Unsourced{Acquire: newOp.Hash, Site: "elsewhere"}, []string{newOp.Hash}, fanOutAt))
	for _, op := range []catalogop.Op{report, stranger} {
		h.logOp(t, op, "unsourced", `["`+newOp.Hash+`"]`)
	}
	blob := "blake3:" + strings.Repeat("ab", 32)
	hold := sign(catalogop.SignHold(selfPriv, "movie", "movie:arrival:2016", catalogop.Hold{
		Scope: catalogop.ScopeEditionPrefix + "default", Blob: blob, Size: 12, Path: "Arrival (2016)/Arrival.mkv",
		Role: "primary", WorkTitle: "Arrival", Year: 2016, Library: "films", Site: "home", HeldAt: fanOutAt,
	}, []string{newOp.Hash}, fanOutAt))
	h.logOp(t, hold, "hold", `["`+newOp.Hash+`"]`)

	got, found, err := h.cat.PublishedFor(ctx, h.want)
	if err != nil || !found {
		t.Fatalf("PublishedFor = found %v, err %v", found, err)
	}
	if got.OpHash != newOp.Hash || got.Infohash != newer.Infohash || got.ReleaseTitle != newer.Title ||
		got.Scope != catalogop.ScopeWork || got.Aspect != catalogop.AspectPrimary || !got.SelectedAt.Equal(fanOutAt) {
		t.Errorf("published = %+v, want the newer primary acquire %s", got, newOp.Hash)
	}
	if len(got.Unsourced) != 1 || got.Unsourced[0].Site != "site-b" || got.Unsourced[0].OpHash != report.Hash {
		t.Errorf("unsourced = %+v, want site-b's report only", got.Unsourced)
	}
	if len(got.Holds) != 1 || got.Holds[0].OpHash != hold.Hash || got.Holds[0].Blob != blob {
		t.Errorf("holds = %+v, want the one hold", got.Holds)
	}
}
