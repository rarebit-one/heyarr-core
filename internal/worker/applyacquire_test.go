package worker

import (
	"context"
	"crypto/ed25519"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/catalogop"
	"github.com/rarebit-one/heyarr-core/internal/catalogtomb"
	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

var applyAt = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)

// applyHarness is the grab harness plus a catalog op-log, a sibling's key and
// this node's key: enough to receive a sibling's acquire op and act on it.
type applyHarness struct {
	*grabHarness
	ops     *catalogtomb.Store
	sibling ed25519.PrivateKey
	self    ed25519.PrivateKey
	selfKey string
}

func newApplyHarness(t *testing.T) *applyHarness {
	t.Helper()
	g := newGrabHarness(t)
	ops, err := catalogtomb.New(catalogtomb.Options{Writer: g.db.Writer(), Reader: g.db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	_, sibling, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	_, self, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &applyHarness{
		grabHarness: g, ops: ops, sibling: sibling, self: self,
		selfKey: identity.FormatPublicKey(self.Public().(ed25519.PublicKey)),
	}
}

// receive signs an op with key and records it as if a sync delivered it.
func (h *applyHarness) receive(t *testing.T, key ed25519.PrivateKey, ct, wk string, a catalogop.Acquire) string {
	t.Helper()
	heads, err := h.ops.Heads(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	tok, err := catalogop.SignAcquire(key, ct, wk, a, heads, applyAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.ops.RecordOps(t.Context(), []string{tok}); err != nil {
		t.Fatal(err)
	}
	return catalogop.Hash(tok)
}

func (h *applyHarness) apply(t *testing.T) error {
	t.Helper()
	handler := ApplyAcquireHandler(h.ops, h.cat, h.queue, h.selfKey, slog.New(slog.DiscardHandler))
	return handler(t.Context(), jobs.Job{Type: acquisition.ApplyAcquireJobType})
}

func (h *applyHarness) application(t *testing.T, hash string) catalog.FanOutApplication {
	t.Helper()
	all, err := h.cat.FanOutApplications(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return all[hash]
}

func siblingAcquire() catalogop.Acquire {
	return catalogop.Acquire{
		Scope: catalogop.ScopeWork, Aspect: catalogop.AspectPrimary, Profile: "living-room",
		Infohash: fakeInfohash, Title: "Arrival 2016 2160p", WorkTitle: "Arrival", Year: 2016,
		Size: 12_000_000_000, SelectedAt: applyAt, Site: "north",
	}
}

// TestASiblingsAcquireOpBecomesASelectedWantAndAQueuedGrab: THE assertion
// ADR-0119 exists for. A sibling published the release it chose; one pass
// here selects the same infohash on the matching want and queues the grab,
// with the op's magnet as the source.
func TestASiblingsAcquireOpBecomesASelectedWantAndAQueuedGrab(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	hash := h.receive(t, h.sibling, "movie", "movie:arrival:2016", siblingAcquire())

	if err := h.apply(t); err != nil {
		t.Fatal(err)
	}
	if got := h.state(t).Name(); got != "SELECTED" {
		t.Fatalf("after applying the op the want is %s, want SELECTED", got)
	}
	if got := h.selectedID(t); got != "infohash:"+fakeInfohash {
		t.Fatalf("selected %s, want the sibling's infohash", got)
	}
	if got := h.pendingGrabs(t); got != 1 {
		t.Fatalf("%d grabs queued, want 1", got)
	}
	app := h.application(t, hash)
	if app.Outcome != catalog.FanOutApplied || app.DesiredItemID != h.want {
		t.Fatalf("application = %+v, want applied on %s", app, h.want)
	}
	// The grab fetches it: the client is handed a trackerless magnet for the
	// infohash, nothing else.
	if err := h.grab(t, "infohash:"+fakeInfohash); err != nil {
		t.Fatal(err)
	}
	added := h.client.Added()
	if len(added) != 1 || !strings.HasPrefix(string(added[0]), "magnet:?xt=urn:btih:"+fakeInfohash) {
		t.Fatalf("client was handed %d sources: %v", len(added), added)
	}
	if strings.Contains(string(added[0]), "tr=") {
		t.Errorf("a trackerless op produced a magnet with trackers: %s", added[0])
	}
	if got := h.state(t).Name(); got != "QUEUED" {
		t.Fatalf("after the grab the want is %s, want QUEUED", got)
	}

	// A second pass changes nothing: the op is applied and final.
	if err := h.apply(t); err != nil {
		t.Fatal(err)
	}
	if got := h.pendingGrabs(t); got != 1 {
		t.Fatalf("a second pass queued another grab: %d", got)
	}

	// This site's grab of the sibling's release does not publish it back:
	// the real publisher, over the real facts, records nothing.
	pub := &CatalogAcquirePublisher{Facts: h.cat, Ops: h.ops, Signer: h.self, Site: "south"}
	if err := pub.Publish(t.Context(), h.want, "infohash:"+fakeInfohash, fakeInfohash); err != nil {
		t.Fatal(err)
	}
	ops, err := h.ops.Ops(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 {
		t.Fatalf("the op log holds %d ops, want only the sibling's: the fan-out grab echoed it", len(ops))
	}
}

// TestOwnOpsAreNotApplied: the origin's own op is in its log too; it is
// recorded as own and never fans out to itself.
func TestOwnOpsAreNotApplied(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	hash := h.receive(t, h.self, "movie", "movie:arrival:2016", siblingAcquire())
	if err := h.apply(t); err != nil {
		t.Fatal(err)
	}
	if got := h.state(t).Name(); got != "MISSING" {
		t.Fatalf("an own op moved the want to %s", got)
	}
	if app := h.application(t, hash); app.Outcome != catalog.FanOutOwn {
		t.Fatalf("application = %+v, want own", app)
	}
}

// TestAnUnresolvedScopeIsRetriedUntilItResolves: an item-scoped op for an item
// this node has not materialised is recorded scope_unresolved and tried again
// on the next pass; once the item exists the want is created and selected.
func TestAnUnresolvedScopeIsRetriedUntilItResolves(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	a := siblingAcquire()
	a.Scope = catalogop.ScopeItemPrefix + "s01e04"
	hash := h.receive(t, h.sibling, "movie", "movie:arrival:2016", a)

	if err := h.apply(t); err != nil {
		t.Fatal(err)
	}
	if app := h.application(t, hash); app.Outcome != catalog.FanOutScopeUnresolved {
		t.Fatalf("application = %+v, want scope_unresolved", app)
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	h.exec(t, `INSERT INTO items (id, work_id, edition_id, item_key, title, attributes, created_at, updated_at)
		VALUES ('i1', 'w1', NULL, 's01e04', 'Episode 4', '{}', ?, ?)`, stamp, stamp)
	if err := h.apply(t); err != nil {
		t.Fatal(err)
	}
	app := h.application(t, hash)
	if app.Outcome != catalog.FanOutApplied || app.DesiredItemID == "" || app.DesiredItemID == h.want {
		t.Fatalf("application = %+v, want applied on a new item-scoped want", app)
	}
	var phase string
	if err := h.db.Reader().QueryRow(`SELECT phase FROM acquisition_state WHERE desired_item_id = ?`, app.DesiredItemID).Scan(&phase); err != nil {
		t.Fatal(err)
	}
	if phase != "selected" {
		t.Errorf("the item want is %s, want selected", phase)
	}
}

// TestAPrivateReleaseHasNoSiteSourceYet: a private op (no trackers, resolve
// through own indexers) is not acted on in this increment, and the reason is
// recorded rather than a different release being grabbed.
func TestAPrivateReleaseHasNoSiteSourceYet(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	a := siblingAcquire()
	a.Private = true
	hash := h.receive(t, h.sibling, "movie", "movie:arrival:2016", a)
	if err := h.apply(t); err != nil {
		t.Fatal(err)
	}
	if app := h.application(t, hash); app.Outcome != catalog.FanOutNoSiteSource {
		t.Fatalf("application = %+v, want no_site_source", app)
	}
	if got := h.state(t).Name(); got != "MISSING" {
		t.Fatalf("a private op moved the want to %s", got)
	}
}

// TestAWantAlreadyAcquiringIsLeftAlone: if this site already selected its own
// release for the want, a sibling's op does not drag it back; the outcome
// says so.
func TestAWantAlreadyAcquiringIsLeftAlone(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	h.fake.Offer("Arrival", offer("good", 2160, "hevc"))
	if err := h.run(t); err != nil {
		t.Fatal(err)
	}
	chosen := h.selectedID(t)
	hash := h.receive(t, h.sibling, "movie", "movie:arrival:2016", siblingAcquire())
	if err := h.apply(t); err != nil {
		t.Fatal(err)
	}
	if app := h.application(t, hash); app.Outcome != catalog.FanOutHeld {
		t.Fatalf("application = %+v, want held", app)
	}
	if got := h.selectedID(t); got != chosen {
		t.Fatalf("the sibling's op replaced this site's selection: %s -> %s", chosen, got)
	}
}

// TestANewerOpSupersedesAnOlderOne: only the effective (newest) acquisition
// per scope is applied; the older op is never acted on.
func TestANewerOpSupersedesAnOlderOne(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	older := siblingAcquire()
	newer := siblingAcquire()
	newer.Infohash = strings.Repeat("ab", 20)
	newer.SelectedAt = applyAt.Add(time.Minute)
	oldHash := h.receive(t, h.sibling, "movie", "movie:arrival:2016", older)
	h.receive(t, h.sibling, "movie", "movie:arrival:2016", newer)
	if err := h.apply(t); err != nil {
		t.Fatal(err)
	}
	if got := h.selectedID(t); got != "infohash:"+newer.Infohash {
		t.Fatalf("selected %s, want the newer op's infohash", got)
	}
	if app := h.application(t, oldHash); app.Outcome != "" {
		t.Fatalf("the superseded op was applied: %+v", app)
	}
}

// failingQueue refuses every enqueue.
type failingQueue struct{}

func (failingQueue) Enqueue(context.Context, jobs.EnqueueOptions) (jobs.Job, error) {
	return jobs.Job{}, errors.New("queue unavailable")
}

// TestAnInterruptedApplicationIsResumedNotHeld: the direct release committed
// but the grab was never queued (here the enqueue fails; a crash between the
// two is the same shape). The pass fails without recording an outcome, and
// the next pass finds the want SELECTED on this op's own candidate and queues
// the grab, rather than calling it held and never grabbing.
func TestAnInterruptedApplicationIsResumedNotHeld(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	hash := h.receive(t, h.sibling, "movie", "movie:arrival:2016", siblingAcquire())

	broken := ApplyAcquireHandler(h.ops, h.cat, failingQueue{}, h.selfKey, slog.New(slog.DiscardHandler))
	if err := broken(t.Context(), jobs.Job{Type: acquisition.ApplyAcquireJobType}); err == nil {
		t.Fatal("a pass whose grab could not be queued reported success")
	}
	if got := h.state(t).Name(); got != "SELECTED" {
		t.Fatalf("precondition: the want is %s, want SELECTED with no grab", got)
	}
	if app := h.application(t, hash); app.Outcome != "" {
		t.Fatalf("an outcome was recorded for a pass that failed: %+v", app)
	}

	if err := h.apply(t); err != nil {
		t.Fatal(err)
	}
	if app := h.application(t, hash); app.Outcome != catalog.FanOutApplied {
		t.Fatalf("application = %+v, want applied (resumed)", app)
	}
	if got := h.pendingGrabs(t); got != 1 {
		t.Fatalf("%d grabs queued, want the resumed one", got)
	}
}

// TestAPrivateOpCreatesNothingAndIsRetried: a private op for a work this
// site does not have creates neither the work nor a want (a want would be
// searched here and could grab a different release), and the outcome is
// retried each pass without being rewritten while nothing changes.
func TestAPrivateOpCreatesNothingAndIsRetried(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	a := siblingAcquire()
	a.Private, a.WorkTitle, a.Year = true, "Dune", 2021
	hash := h.receive(t, h.sibling, "movie", "movie:dune:2021", a)
	if err := h.apply(t); err != nil {
		t.Fatal(err)
	}
	first := h.application(t, hash)
	if first.Outcome != catalog.FanOutNoSiteSource || first.Outcome.Final() {
		t.Fatalf("application = %+v, want a retried no_site_source", first)
	}
	var works int
	if err := h.db.Reader().QueryRow(
		`SELECT count(*) FROM works WHERE work_key = 'movie:dune:2021'`).Scan(&works); err != nil {
		t.Fatal(err)
	}
	if works != 0 {
		t.Fatalf("a private op created %d works", works)
	}
	if err := h.apply(t); err != nil {
		t.Fatal(err)
	}
	if again := h.application(t, hash); again != first {
		t.Fatalf("an unchanged retried outcome was rewritten: %+v then %+v", first, again)
	}
}
