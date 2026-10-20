package catalog_test

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/catalogop"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

var fanOutAt = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

// acquireOp signs an acquire op from a (fictional) sibling for the harness work.
func acquireOp(t *testing.T, ct, workKey string, a catalogop.Acquire) catalogop.Op {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := catalogop.SignAcquire(priv, ct, workKey, a, nil, fanOutAt)
	if err != nil {
		t.Fatal(err)
	}
	op, err := catalogop.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func arrivalAcquire() catalogop.Acquire {
	return catalogop.Acquire{
		Scope: catalogop.ScopeWork, Aspect: catalogop.AspectPrimary, Profile: "living-room",
		Infohash: "0123456789abcdef0123456789abcdef01234567", Title: "Arrival 2016 2160p",
		WorkTitle: "Arrival", Year: 2016, Size: 12_000_000_000, SelectedAt: fanOutAt, Site: "north",
	}
}

// TestEnsureFanOutWantReusesTheExistingWant: the harness already wants Arrival
// under living-room at work scope, so the op resolves to that want, creating
// nothing.
func TestEnsureFanOutWantReusesTheExistingWant(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	got, err := h.cat.EnsureFanOutWant(context.Background(), acquireOp(t, "movie", "movie:arrival:2016", arrivalAcquire()))
	if err != nil {
		t.Fatal(err)
	}
	if got.DesiredItemID != h.want || got.Created || got.WorkCreated || got.ProfileFallback != "" {
		t.Errorf("got %+v, want the existing want %s with nothing created", got, h.want)
	}
}

// TestEnsureFanOutWantCreatesTheWorkUnderTheOpsKey: a work this node has never
// seen is created under the op's (content_type, work_key) with the op's title
// and year, and a want over it under the named profile.
func TestEnsureFanOutWantCreatesTheWorkUnderTheOpsKey(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a := arrivalAcquire()
	a.WorkTitle, a.Year, a.Title = "The Thing", 1982, "The Thing 1982 2160p"
	got, err := h.cat.EnsureFanOutWant(context.Background(), acquireOp(t, "movie", "movie:the-thing:1982", a))
	if err != nil {
		t.Fatal(err)
	}
	if !got.WorkCreated || !got.Created || got.DesiredItemID == "" {
		t.Fatalf("got %+v, want a created work and want", got)
	}
	var title string
	var year int
	if err := h.db.Reader().QueryRow(
		`SELECT w.title, w.year FROM works w JOIN desired_items d ON d.work_id = w.id WHERE d.id = ?`,
		got.DesiredItemID).Scan(&title, &year); err != nil {
		t.Fatal(err)
	}
	if title != "The Thing" || year != 1982 {
		t.Errorf("work = %q %d, want The Thing 1982", title, year)
	}
	var reason string
	if err := h.db.Reader().QueryRow(`SELECT reason FROM desired_items WHERE id = ?`, got.DesiredItemID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason == "" {
		t.Error("the fan-out want carries no reason naming its origin")
	}
	// Idempotent: the same op again resolves to the same want.
	again, err := h.cat.EnsureFanOutWant(context.Background(), acquireOp(t, "movie", "movie:the-thing:1982", a))
	if err != nil {
		t.Fatal(err)
	}
	if again.DesiredItemID != got.DesiredItemID || again.Created || again.WorkCreated {
		t.Errorf("second call = %+v, want the same want and nothing created", again)
	}
}

// TestEnsureFanOutWantFallsBackToTheDefaultProfile: a profile name this site
// does not have falls back to the content type's seeded default, and says so.
func TestEnsureFanOutWantFallsBackToTheDefaultProfile(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	h.exec(t, `INSERT INTO quality_profiles
		(id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES ('q-def', 'everyday-indexed', '', '[]', '[]', '[]', 1, ?, ?)`, stamp, stamp)
	a := arrivalAcquire()
	a.Profile = "their-cinema"
	got, err := h.cat.EnsureFanOutWant(context.Background(), acquireOp(t, "movie", "movie:arrival:2016", a))
	if err != nil {
		t.Fatal(err)
	}
	if got.ProfileFallback != "everyday-indexed" || !got.Created {
		t.Errorf("got %+v, want a new want under the default profile", got)
	}
	var profile string
	if err := h.db.Reader().QueryRow(`SELECT quality_profile_id FROM desired_items WHERE id = ?`, got.DesiredItemID).Scan(&profile); err != nil {
		t.Fatal(err)
	}
	if profile != "q-def" {
		t.Errorf("want's profile = %s, want q-def", profile)
	}
	// Without a default for the content type either, there is no want to make.
	b := arrivalAcquire()
	b.Profile = "their-cinema"
	if _, err := h.cat.EnsureFanOutWant(context.Background(), acquireOp(t, "mystery", "mystery:x:0", b)); !catalog.IsFanOutNoProfile(err) {
		t.Errorf("unknown content type with unknown profile: err = %v, want no-profile", err)
	}
}

// TestEnsureFanOutWantNeedsTheScopeMaterialised: an edition- or item-scoped
// op resolves only once this node has the edition or item by key; until then
// it is the retryable "not yet", and nothing is created. A subtitle aspect
// becomes a subtitle want in that language.
func TestEnsureFanOutWantNeedsTheScopeMaterialised(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a := arrivalAcquire()
	a.Scope = catalogop.ScopeItemPrefix + "s01e04"
	a.Aspect = catalogop.AspectSubtitlePrefix + "en"
	op := acquireOp(t, "movie", "movie:arrival:2016", a)
	if _, err := h.cat.EnsureFanOutWant(context.Background(), op); !catalog.IsFanOutScopeUnresolved(err) {
		t.Fatalf("err = %v, want scope-unresolved", err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	h.exec(t, `INSERT INTO items (id, work_id, edition_id, item_key, title, attributes, created_at, updated_at)
		VALUES ('i1', 'w1', NULL, 's01e04', 'Episode 4', '{}', ?, ?)`, stamp, stamp)
	got, err := h.cat.EnsureFanOutWant(context.Background(), op)
	if err != nil {
		t.Fatal(err)
	}
	var scope, aspect, lang, itemID string
	if err := h.db.Reader().QueryRow(`SELECT scope, aspect, language, item_id FROM desired_items WHERE id = ?`, got.DesiredItemID).
		Scan(&scope, &aspect, &lang, &itemID); err != nil {
		t.Fatal(err)
	}
	if scope != "item" || aspect != "subtitle" || lang != "en" || itemID != "i1" {
		t.Errorf("want = %s/%s/%s on %s, want item/subtitle/en on i1", scope, aspect, lang, itemID)
	}
}

// TestEnsureFanOutWantHonoursATombstone: a work tombstoned here is not
// resurrected by a sibling's acquire (ADR-0073 remove-wins).
func TestEnsureFanOutWantHonoursATombstone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.exec(t, `INSERT INTO work_tombstones (content_type, work_key, op_hash, tombstoned_at)
		VALUES ('movie', 'movie:the-thing:1982', 'sha256:x', ?)`, time.Now().UTC().Format(time.RFC3339Nano))
	a := arrivalAcquire()
	a.WorkTitle, a.Year = "The Thing", 1982
	if _, err := h.cat.EnsureFanOutWant(context.Background(), acquireOp(t, "movie", "movie:the-thing:1982", a)); !catalog.IsFanOutTombstoned(err) {
		t.Fatalf("err = %v, want tombstoned", err)
	}
	var n int
	if err := h.db.Reader().QueryRow(`SELECT count(*) FROM works WHERE work_key = 'movie:the-thing:1982'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("a tombstoned work was created")
	}
}

// TestFanOutApplicationsRoundTrip: outcomes upsert by op hash and read back.
func TestFanOutApplicationsRoundTrip(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	op := acquireOp(t, "movie", "movie:arrival:2016", arrivalAcquire())
	// The applications table references catalog_ops, so the op must be in the log.
	h.exec(t, `INSERT INTO catalog_ops (op_hash, content_type, work_key, op, signer, prev, iat, token, received_at)
		VALUES (?, 'movie', 'movie:arrival:2016', 'acquire', ?, '[]', ?, ?, ?)`,
		op.Hash, op.By, fanOutAt.Format(time.RFC3339), op.Token, fanOutAt.Format(time.RFC3339))
	if err := h.cat.RecordFanOutApplication(ctx, op.Hash, "", catalog.FanOutScopeUnresolved, "item s01e04"); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.RecordFanOutApplication(ctx, op.Hash, h.want, catalog.FanOutApplied, "from site north"); err != nil {
		t.Fatal(err)
	}
	got, err := h.cat.FanOutApplications(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a, ok := got[op.Hash]
	if !ok || a.Outcome != catalog.FanOutApplied || a.DesiredItemID != h.want || a.Detail != "from site north" {
		t.Errorf("application = %+v %v", a, ok)
	}
	if !catalog.FanOutApplied.Final() || catalog.FanOutScopeUnresolved.Final() {
		t.Error("finality is wrong: applied is final, scope_unresolved is not")
	}
}
