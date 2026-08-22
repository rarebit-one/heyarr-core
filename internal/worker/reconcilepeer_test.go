package worker

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/peer/inventory"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// Peer convergence, against a real database and a real queue (§19, §57, M4-08).
//
// # What these tests are about, and what a weaker version of them would miss
//
// The property is not "one cycle emits the right jobs". It is that
// reconciliation CONVERGES: run twice over an unchanged fabric it must create
// nothing the second time, and after a transfer lands it must find strictly
// less to do. A reconciler that re-enqueues the same transfer every cycle is
// not converging, it is looping — and in a single run the two are
// indistinguishable, which is why every assertion here is made ACROSS cycles
// and why the first cycle is always asserted to have enqueued something. A
// reconciler that enqueued nothing at all would pass "the second cycle
// enqueued nothing" trivially.
//
// The fabric is two Full Peers, which is the configuration §19 describes and
// the one nothing in this repository had until M4-07: reconcile.go's
// requiredPeers has returned exactly one row in every deployment that has ever
// existed, and everything downstream of it runs here for the first time.

const (
	// Valid blake3 digests: the blobs table's CHECK constrains the shape, and
	// a fixture with a short hash would fail as a constraint error rather than
	// telling us anything.
	blobOne = "blake3:" + "11111111111111111111111111111111111111111111111111111111111111a1"
	blobTwo = "blake3:" + "22222222222222222222222222222222222222222222222222222222222222b2"
)

func blobHash(n int) string {
	return fmt.Sprintf("blake3:%064x", n)
}

type convergeHarness struct {
	db    *sqlite.DB
	cat   *catalog.Catalog
	queue *jobs.Queue
	log   *slog.Logger

	// self is this node and other is the second Full Peer. Every assertion
	// below about "the peer missing the bytes" is worthless if they are the
	// same row, which newConvergeHarness refuses to let happen.
	self  string
	other string

	stamp string
}

func newConvergeHarness(t *testing.T) *convergeHarness {
	t.Helper()
	ctx := t.Context()
	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{
		DB: db, Events: eventLog, PeerName: "site-a", PeerSite: "site-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(jobs.Options{
		Writer: db.Writer(), Reader: db.Reader(), Events: eventLog,
	})
	if err != nil {
		t.Fatal(err)
	}

	h := &convergeHarness{
		db: db, cat: cat, queue: queue,
		log:   slog.New(slog.DiscardHandler),
		stamp: time.Now().UTC().Format(time.RFC3339Nano),
	}
	self, err := cat.SelfPeer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.self = self
	h.other = "01990000-0000-7000-8000-00000000site"
	h.exec(t, `INSERT INTO peers (id, name, site, mode, is_self, created_at, enrolled_at)
		VALUES (?, 'site-b', 'site-b', 'full', 0, ?, ?)`, h.other, h.stamp, h.stamp)
	if h.self == h.other {
		t.Fatal("the fixture's second peer IS the self peer; nothing below would prove anything")
	}
	h.exec(t, `INSERT INTO works
		(id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES ('w1', 'movie', 'movie:arrival:2016', 'Arrival', 'arrival', 2016, '{}', ?, ?)`,
		h.stamp, h.stamp)
	return h
}

func (h *convergeHarness) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := h.db.Writer().Exec(query, args...); err != nil {
		t.Fatalf("seeding (%s): %v", query, err)
	}
}

// managed catalogues a blob under a managed asset — a blob the fabric is
// therefore expected to converge on.
func (h *convergeHarness) managed(t *testing.T, hash string) {
	t.Helper()
	h.exec(t, `INSERT INTO blobs (hash, size, mime, chunked, first_seen_at)
		VALUES (?, 1024, 'video/x-matroska', 0, ?)`, hash, h.stamp)
	h.exec(t, `INSERT INTO editions
		(id, work_id, label, edition_type, edition_key, language, attributes, created_at)
		VALUES (?, 'w1', '1080p', 'bluray', ?, 'en', '{}', ?)`, "e-"+hash, hash, h.stamp)
	h.exec(t, `INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash,
			source_path, role, filename, mime, identification_source, created_at, updated_at)
		VALUES (?, ?, NULL, 'managed', ?, '/srv/x.mkv', 'primary', 'x.mkv',
			'video/x-matroska', 'path', ?, ?)`, "a-"+hash, "e-"+hash, hash, h.stamp, h.stamp)
}

// linked catalogues a linked asset (ADR-0020): a path, a fingerprint, and NO
// blob. The schema's CHECK enforces blob_hash IS NULL for it, which is why
// this is a fixture and not a hypothetical.
func (h *convergeHarness) linked(t *testing.T, id, path string) {
	t.Helper()
	h.exec(t, `INSERT INTO editions
		(id, work_id, label, edition_type, edition_key, language, attributes, created_at)
		VALUES (?, 'w1', 'scan', '', ?, 'en', '{}', ?)`, "e-"+id, id, h.stamp)
	h.exec(t, `INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash,
			source_path, fingerprint, role, filename, mime, identification_source, created_at, updated_at)
		VALUES (?, ?, NULL, 'linked', NULL, ?, 'size:1024', 'primary', 'photo.jpg',
			'image/jpeg', 'path', ?, ?)`, "a-"+id, "e-"+id, path, h.stamp, h.stamp)
}

// reports folds in a peer's inventory through the real M4-07 path, which is
// how a completed transfer becomes a `replicas` row: the destination verifies
// its own bytes and says so. Writing the row by hand would test convergence
// against a fact this system has no way of learning.
func (h *convergeHarness) reports(t *testing.T, peerID string, hashes ...string) {
	t.Helper()
	report := inventory.Report{
		PeerID:     peerID,
		Mode:       inventory.ModeFull,
		ObservedAt: time.Now().UTC(),
	}
	for _, hash := range hashes {
		report.Entries = append(report.Entries, inventory.Entry{
			BlobHash: hash, State: inventory.StatePresent, BytesPresent: 1024,
		})
	}
	if _, err := h.cat.ReconcileInventory(t.Context(), peerID, report); err != nil {
		t.Fatalf("folding in %s's inventory: %v", peerID, err)
	}
}

// cycleSummary is the sync.reconciled payload, as a value.
type cycleSummary struct {
	Scope           string `json:"scope"`
	Peers           int    `json:"peers"`
	Desired         int    `json:"desired"`
	UnderReplicated int    `json:"under_replicated"`
	InFlight        int    `json:"in_flight"`
	Enqueued        int    `json:"enqueued"`
	Deferred        int    `json:"deferred"`
}

// cycle runs one reconciliation and returns what its event said it did.
//
// The summary is read back OUT of the event log rather than returned by the
// handler, so these tests assert on what an operator and a subscriber would
// actually see. A handler that computed the right counts and emitted different
// ones would pass a test that trusted its return value.
func (h *convergeHarness) cycle(t *testing.T, scope string, limit int) cycleSummary {
	t.Helper()
	before := h.cycleEvents(t)
	handler := reconcilePeerHandler(h.cat, h.queue, h.log, limit)
	payload, err := json.Marshal(replication.ReconcilePeerPayload{PeerID: scope})
	if err != nil {
		t.Fatal(err)
	}
	if err := handler(t.Context(), jobs.Job{
		Type: replication.ReconcilePeerJobType, Payload: payload,
	}); err != nil {
		t.Fatalf("the reconciliation cycle failed: %v", err)
	}
	after := h.cycleEvents(t)
	// One event per cycle, never one per enqueued job — job.enqueued already
	// reports those, per job.
	if len(after) != len(before)+1 {
		t.Fatalf("one cycle emitted %d sync.reconciled events, want exactly 1",
			len(after)-len(before))
	}
	return after[len(after)-1]
}

// cycleEvents reads every sync.reconciled payload, in order.
func (h *convergeHarness) cycleEvents(t *testing.T) []cycleSummary {
	t.Helper()
	rows, err := h.db.Reader().Query(
		`SELECT payload FROM events WHERE type = ? ORDER BY seq`, events.TypeSyncReconciled)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []cycleSummary
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var s cycleSummary
		if err := json.Unmarshal([]byte(raw), &s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// queuedTransfer is one replicate_blob job as the queue holds it.
//
// Named for the QUEUE rather than for the act: internal/peer/transfer is the
// package that actually moves bytes (M4-09), and a test type sharing its name
// would shadow the import for the whole package.
type queuedTransfer struct {
	BlobHash    string
	Destination string
	DedupeKey   string
}

func (t queuedTransfer) String() string {
	return fmt.Sprintf("%s → %s", t.BlobHash, t.Destination)
}

// transfers reads every replicate_blob job in the queue, in a stable order.
func (h *convergeHarness) transfers(t *testing.T) []queuedTransfer {
	t.Helper()
	rows, err := h.db.Reader().Query(
		`SELECT payload, coalesce(dedupe_key, '') FROM jobs WHERE type = ? ORDER BY id`,
		replication.ReplicateBlobJobType)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []queuedTransfer
	for rows.Next() {
		var raw, key string
		if err := rows.Scan(&raw, &key); err != nil {
			t.Fatal(err)
		}
		var p replication.ReplicateBlobPayload
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, queuedTransfer{
			BlobHash: p.BlobHash, Destination: p.DestinationPeerID, DedupeKey: key,
		})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// With two Full Peers and a blob on one of them, exactly one transfer is
// emitted, for the peer that is missing it — and NONE for the peer that holds
// it, which is asserted by naming the destination rather than by counting.
func TestABlobOnOnePeerIsWorkForTheOtherOnly(t *testing.T) {
	h := newConvergeHarness(t)
	h.managed(t, blobOne)
	h.reports(t, h.self, blobOne)

	summary := h.cycle(t, "", 0)
	if summary.Peers != 2 {
		t.Fatalf("the cycle considered %d peers, want 2 — §19's required set is every Full Peer",
			summary.Peers)
	}
	if summary.Desired != 1 {
		t.Fatalf("the desired blob set is %d, want 1", summary.Desired)
	}

	got := h.transfers(t)
	if len(got) != 1 {
		t.Fatalf("transfers = %v, want exactly one", got)
	}
	if got[0].Destination != h.other {
		t.Fatalf("the transfer is destined for %s, want the peer that is MISSING the bytes (%s)",
			got[0].Destination, h.other)
	}
	if got[0].BlobHash != blobOne {
		t.Fatalf("the transfer carries %s, want %s", got[0].BlobHash, blobOne)
	}
	for _, tr := range got {
		if tr.Destination == h.self {
			t.Fatalf("a transfer to the peer that already holds the bytes: %v", tr)
		}
	}
}

// The dedupe key is blob_hash + destination peer, which is what makes the
// second cycle harmless by construction. Asserted on the ROW rather than on
// the constructor, so that an enqueue which forgot to pass the key is caught.
func TestTheTransferIsKeyedOnTheBlobAndTheDestination(t *testing.T) {
	h := newConvergeHarness(t)
	h.managed(t, blobOne)
	h.reports(t, h.self, blobOne)
	h.cycle(t, "", 0)

	got := h.transfers(t)
	if len(got) != 1 {
		t.Fatalf("transfers = %v, want exactly one", got)
	}
	want := replication.Gap{BlobHash: blobOne, PeerID: h.other}.DedupeKey()
	if got[0].DedupeKey != want {
		t.Fatalf("dedupe key = %q, want %q — without it a second cycle queues the same transfer again",
			got[0].DedupeKey, want)
	}
}

// The convergence property, stated as the issue states it: twice with nothing
// changed emits no second job — AND the first cycle actually enqueued
// something, because a reconciler that enqueues nothing passes the naive
// version of this test.
func TestASecondCycleWithNothingChangedCreatesNoSecondJob(t *testing.T) {
	h := newConvergeHarness(t)
	h.managed(t, blobOne)
	h.reports(t, h.self, blobOne)

	first := h.cycle(t, "", 0)
	if first.Peers != 2 {
		t.Fatalf("the cycle considered %d peers, want 2; a fabric with one peer would make "+
			"everything below true for the wrong reason", first.Peers)
	}
	if first.Enqueued == 0 {
		t.Fatal("the FIRST cycle enqueued nothing; a reconciler that never emits work " +
			"would pass the second-cycle assertion below trivially")
	}
	if first.Enqueued != 1 {
		t.Fatalf("the first cycle enqueued %d, want 1", first.Enqueued)
	}
	afterFirst := h.transfers(t)
	if len(afterFirst) != 1 {
		t.Fatalf("after one cycle the queue holds %v, want exactly one transfer", afterFirst)
	}

	second := h.cycle(t, "", 0)
	if second.Enqueued != 0 {
		t.Fatalf("the second cycle enqueued %d jobs over an unchanged fabric; "+
			"that is looping, not converging", second.Enqueued)
	}
	// The gap has NOT gone away — nothing has moved — and the cycle says so
	// while correctly doing nothing about it. A reconciler that reported zero
	// under-replicated here would be hiding the work rather than deduping it.
	if second.UnderReplicated != 1 {
		t.Fatalf("the second cycle saw %d under-replicated pairs, want 1: "+
			"nothing has been transferred yet", second.UnderReplicated)
	}
	if second.InFlight != 1 {
		t.Fatalf("the second cycle reported %d in flight, want 1", second.InFlight)
	}
	afterSecond := h.transfers(t)
	if len(afterSecond) != 1 {
		t.Fatalf("after two cycles the queue holds %v, want the SAME one transfer", afterSecond)
	}
	if afterSecond[0] != afterFirst[0] {
		t.Fatalf("the second cycle replaced the transfer: %v then %v", afterFirst[0], afterSecond[0])
	}
}

// Monotonic toward the desired set, observed ACROSS cycles: the work count
// falls from a non-zero value to zero once the destination reports the bytes,
// and never rises.
func TestConvergenceIsMonotonicAcrossCycles(t *testing.T) {
	h := newConvergeHarness(t)
	h.managed(t, blobOne)
	h.managed(t, blobTwo)
	h.reports(t, h.self, blobOne, blobTwo)

	// TWO series, because the diff shrinking is not on its own evidence of
	// convergence. A reconciler that re-enqueued the same transfer every cycle
	// would still see its DIFF fall as inventories reported the bytes landing
	// — the diff is computed from the fabric, not from what was enqueued — so
	// a test watching only that passes on a reconciler that loops. The queue
	// depth is what catches it: a converging reconciler stops adding rows.
	var work, queued []int
	first := h.cycle(t, "", 0)
	if first.Peers != 2 {
		t.Fatalf("the cycle considered %d peers, want 2; the series below is about two "+
			"Full Peers converging on each other", first.Peers)
	}
	work = append(work, first.UnderReplicated)
	queued = append(queued, len(h.transfers(t)))
	if first.UnderReplicated == 0 || first.Enqueued == 0 {
		t.Fatalf("the first cycle found %d gaps and enqueued %d; the series below "+
			"proves nothing unless it starts non-zero", first.UnderReplicated, first.Enqueued)
	}

	// The second peer takes one of the two blobs and re-reports its disk.
	h.reports(t, h.other, blobOne)
	work = append(work, h.cycle(t, "", 0).UnderReplicated)
	queued = append(queued, len(h.transfers(t)))

	// Then the other.
	h.reports(t, h.other, blobOne, blobTwo)
	last := h.cycle(t, "", 0)
	work = append(work, last.UnderReplicated)
	queued = append(queued, len(h.transfers(t)))

	for i := 1; i < len(work); i++ {
		if work[i] > work[i-1] {
			t.Fatalf("the work count rose across cycles: %v — a reconciler whose backlog "+
				"grows as transfers land is diverging", work)
		}
		if queued[i] > queued[i-1] {
			t.Fatalf("the queue grew across cycles: %v (gaps were %v) — the transfers were "+
				"already queued, so a reconciler still adding rows is looping, not converging",
				queued, work)
		}
	}
	if queued[0] == 0 {
		t.Fatalf("the first cycle queued nothing; the queue-depth series %v proves nothing", queued)
	}
	if work[0] <= 0 {
		t.Fatalf("the work count series %v does not start from a non-zero value", work)
	}
	if work[len(work)-1] != 0 {
		t.Fatalf("the work count series %v never reached zero; the fabric is converged "+
			"and reconciliation still believes there is work", work)
	}
	if last.Enqueued != 0 {
		t.Fatalf("the converged cycle still enqueued %d transfers", last.Enqueued)
	}
	// A converged cycle still EMITS. "We looked and everything is where it
	// should be" is the outcome that leaves no job rows behind to prove it
	// happened, and the only thing distinguishing it from a reconciler that
	// silently stopped running.
	if got := len(h.cycleEvents(t)); got != 3 {
		t.Fatalf("%d cycle events for 3 cycles", got)
	}
}

// A blob on neither peer is work for both. This is the assertion the "hard-code
// the required peer set to one peer" sabotage must break.
func TestABlobOnNeitherPeerIsWorkForBoth(t *testing.T) {
	h := newConvergeHarness(t)
	h.managed(t, blobOne)
	// Nobody reports holding it: the catalog knows the blob, no peer has
	// confirmed bytes.

	summary := h.cycle(t, "", 0)
	if summary.UnderReplicated != 2 {
		t.Fatalf("under_replicated = %d, want 2 — one for each Full Peer", summary.UnderReplicated)
	}

	got := h.transfers(t)
	if len(got) != 2 {
		t.Fatalf("transfers = %v, want one per peer", got)
	}
	destinations := map[string]bool{}
	for _, tr := range got {
		if tr.BlobHash != blobOne {
			t.Fatalf("a transfer for an unexpected blob: %v", tr)
		}
		destinations[tr.Destination] = true
	}
	if !destinations[h.self] {
		t.Fatalf("no transfer destined for %s (self); transfers = %v", h.self, got)
	}
	if !destinations[h.other] {
		t.Fatalf("no transfer destined for %s; transfers = %v", h.other, got)
	}
}

// ADR-0020, asserted DIRECTLY: a linked asset has no blob, so it is outside
// the desired set by construction. The assertion names the blob that IS
// desired rather than checking a total, so a fabric that produced the right
// COUNT for the wrong reason fails.
func TestALinkedAssetProducesNoReplicationWork(t *testing.T) {
	h := newConvergeHarness(t)
	h.linked(t, "photo1", "/home/jaryl/pictures/2019/beach.jpg")
	h.linked(t, "photo2", "/home/jaryl/pictures/2019/hill.jpg")
	h.managed(t, blobOne)
	h.reports(t, h.self, blobOne)

	plan, err := h.cat.PlanPeerConvergence(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	// Three assets, one blob. The desired set is the blobs, not the assets.
	if plan.Desired != 1 {
		t.Fatalf("the desired blob set is %d over 3 assets of which 2 are linked, want 1",
			plan.Desired)
	}
	if len(plan.Gaps) != 1 {
		t.Fatalf("gaps = %+v, want exactly one", plan.Gaps)
	}
	// Directly: the one gap is the MANAGED blob, and no gap carries an empty
	// or unknown hash — which is the only shape a linked asset could take here.
	if plan.Gaps[0].BlobHash != blobOne {
		t.Fatalf("the gap is for %q, want the managed blob %q", plan.Gaps[0].BlobHash, blobOne)
	}
	if plan.Gaps[0].PeerID != h.other {
		t.Fatalf("the gap names %s, want the peer missing the bytes (%s)",
			plan.Gaps[0].PeerID, h.other)
	}

	h.cycle(t, "", 0)
	for _, tr := range h.transfers(t) {
		if tr.BlobHash != blobOne {
			t.Fatalf("a transfer for something other than the managed blob: %v", tr)
		}
	}
}

// A blob nothing references any more is not replicated: it is what garbage
// collection is about to reclaim (ADR-0018), and shipping it to a second site
// so a sweep can delete it at both ends is work with a negative return.
func TestAnUnreferencedBlobIsNotReplicated(t *testing.T) {
	h := newConvergeHarness(t)
	h.exec(t, `INSERT INTO blobs (hash, size, mime, chunked, first_seen_at)
		VALUES (?, 1024, 'video/x-matroska', 0, ?)`, blobTwo, h.stamp)

	summary := h.cycle(t, "", 0)
	if summary.Desired != 0 {
		t.Fatalf("the desired set is %d for a blob no asset references, want 0", summary.Desired)
	}
	if got := h.transfers(t); len(got) != 0 {
		t.Fatalf("transfers = %v, want none", got)
	}
}

// Bounded work per cycle, and the remainder DEFERRED rather than dropped: the
// count is reported, and a later cycle picks the rest up.
func TestABoundedCycleDefersTheRemainderAndALaterCycleTakesIt(t *testing.T) {
	h := newConvergeHarness(t)
	const blobs = 5
	for i := 1; i <= blobs; i++ {
		h.managed(t, blobHash(i))
	}
	// Only the second peer is missing them, so the gap count is exactly blobs.
	h.reports(t, h.self, func() []string {
		var all []string
		for i := 1; i <= blobs; i++ {
			all = append(all, blobHash(i))
		}
		return all
	}()...)

	const limit = 2
	first := h.cycle(t, "", limit)
	if first.UnderReplicated != blobs {
		t.Fatalf("under_replicated = %d, want %d", first.UnderReplicated, blobs)
	}
	if first.Enqueued != limit {
		t.Fatalf("the first bounded cycle enqueued %d, want %d", first.Enqueued, limit)
	}
	if first.Deferred != blobs-limit {
		t.Fatalf("deferred = %d, want %d — the remainder must be REPORTED, not dropped",
			first.Deferred, blobs-limit)
	}
	if got := len(h.transfers(t)); got != limit {
		t.Fatalf("the queue holds %d transfers after a bounded cycle, want %d", got, limit)
	}

	// The later cycle picks up the remainder rather than re-offering the same
	// first two. This is the assertion a bound that counted dedupe hits
	// against its own limit would fail: it would enqueue nothing, forever.
	second := h.cycle(t, "", limit)
	if second.InFlight != limit {
		t.Fatalf("the second cycle saw %d in flight, want %d", second.InFlight, limit)
	}
	if second.Enqueued != limit {
		t.Fatalf("the second bounded cycle enqueued %d, want %d — it must take the NEXT slice",
			second.Enqueued, limit)
	}
	if got := len(h.transfers(t)); got != 2*limit {
		t.Fatalf("the queue holds %d transfers after two bounded cycles, want %d", got, 2*limit)
	}

	third := h.cycle(t, "", limit)
	if third.Enqueued != blobs-2*limit {
		t.Fatalf("the third cycle enqueued %d, want the last %d", third.Enqueued, blobs-2*limit)
	}
	if third.Deferred != 0 {
		t.Fatalf("the third cycle still defers %d, want 0", third.Deferred)
	}

	// Every blob, exactly once, to exactly the peer that was missing it.
	got := h.transfers(t)
	if len(got) != blobs {
		t.Fatalf("the queue holds %d transfers, want %d", len(got), blobs)
	}
	seen := map[string]int{}
	for _, tr := range got {
		if tr.Destination != h.other {
			t.Fatalf("a bounded cycle queued a transfer to the wrong peer: %v", tr)
		}
		seen[tr.BlobHash]++
	}
	for i := 1; i <= blobs; i++ {
		if seen[blobHash(i)] != 1 {
			t.Fatalf("blob %d was queued %d times across the bounded cycles; "+
				"the remainder was never reached or was queued twice", i, seen[blobHash(i)])
		}
	}
}

// The deferred count reaches an operator, not just the event: a cycle that hit
// its bound has NOT converged and must say so out loud.
func TestABoundedCycleLogsWhatItDeferred(t *testing.T) {
	h := newConvergeHarness(t)
	for i := 1; i <= 3; i++ {
		h.managed(t, blobHash(i))
	}
	h.reports(t, h.self, blobHash(1), blobHash(2), blobHash(3))

	var buf strings.Builder
	h.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h.cycle(t, "", 1)

	logged := buf.String()
	if !strings.Contains(logged, "deferred=2") {
		t.Fatalf("the cycle deferred 2 gaps and did not log the count; log was:\n%s", logged)
	}
}

// Scoped to one peer, which is the on-demand path: only that peer's gaps are
// considered, and the other peer's absence is not work this cycle does.
func TestAScopedCycleOnlyConsidersThatPeer(t *testing.T) {
	h := newConvergeHarness(t)
	h.managed(t, blobOne)

	summary := h.cycle(t, h.other, 0)
	if summary.Scope != h.other {
		t.Fatalf("scope = %q, want %q", summary.Scope, h.other)
	}
	if summary.Peers != 1 {
		t.Fatalf("a scoped cycle considered %d peers, want 1", summary.Peers)
	}
	got := h.transfers(t)
	if len(got) != 1 {
		t.Fatalf("transfers = %v, want exactly one", got)
	}
	if got[0].Destination != h.other {
		t.Fatalf("a scoped cycle queued work for %s, want only %s", got[0].Destination, h.other)
	}
}

// A scope naming a peer that is not a Full Peer does nothing, rather than
// failing the job five times over an ordinary race — a peer removed or demoted
// between the enqueue and the run.
func TestAScopeThatNamesNoFullPeerDoesNothing(t *testing.T) {
	h := newConvergeHarness(t)
	h.managed(t, blobOne)

	summary := h.cycle(t, "01990000-0000-7000-8000-0000000gone1", 0)
	if summary.Peers != 0 {
		t.Fatalf("peers = %d, want 0", summary.Peers)
	}
	if got := h.transfers(t); len(got) != 0 {
		t.Fatalf("transfers = %v, want none", got)
	}
}

// No RequiredCapability, per the reconcile_desired precedent: a node
// advertising nothing at all still claims this job, because a degraded node is
// exactly the one whose operator most needs to know what it is missing.
func TestADegradedNodeStillClaimsTheConvergenceCycle(t *testing.T) {
	h := newConvergeHarness(t)
	reg := ReconcilePeerRegistration(h.cat, h.queue, h.log)
	if reg.RequiredCapability != "" {
		t.Fatalf("reconcile_peer requires the capability %q; a node with no toolchain, "+
			"no indexer and no download client must still know what it is missing",
			reg.RequiredCapability)
	}
	if reg.MaxConcurrent != 1 {
		t.Fatalf("MaxConcurrent = %d, want 1: two cycles would each decide against "+
			"a fabric the other was changing", reg.MaxConcurrent)
	}

	registry := NewRegistry()
	registry.Register(replication.ReconcilePeerJobType, reg)
	runtime, err := NewRuntime(Config{Owner: "degraded", Capabilities: nil}, h.queue, registry, h.log)
	if err != nil {
		t.Fatal(err)
	}
	var claimable bool
	for _, typ := range runtime.claimableTypes() {
		if typ == replication.ReconcilePeerJobType {
			claimable = true
		}
	}
	if !claimable {
		t.Fatalf("a worker advertising no capabilities will not claim %s; claimable = %v",
			replication.ReconcilePeerJobType, runtime.claimableTypes())
	}
}

// Emit the work and stop there. Moving bytes is a separate job, and a
// reconciliation that also transferred would be one whose cycle time depended
// on the size of the library.
func TestReconciliationEmitsWorkAndMovesNothing(t *testing.T) {
	h := newConvergeHarness(t)
	h.managed(t, blobOne)
	h.reports(t, h.self, blobOne)
	h.cycle(t, "", 0)

	var replicas int
	if err := h.db.Reader().QueryRow(
		`SELECT count(*) FROM replicas WHERE peer_id = ?`, h.other).Scan(&replicas); err != nil {
		t.Fatal(err)
	}
	if replicas != 0 {
		t.Fatalf("reconciliation wrote %d replicas rows for the destination peer; "+
			"it must emit the transfer and touch nothing else", replicas)
	}
	for _, tr := range h.transfers(t) {
		var state string
		if err := h.db.Reader().QueryRow(
			`SELECT state FROM jobs WHERE type = ? AND payload LIKE ?`,
			replication.ReplicateBlobJobType, "%"+tr.BlobHash+"%").Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != string(jobs.Pending) {
			t.Fatalf("the transfer is %q, want %q — reconciliation does not run it",
				state, jobs.Pending)
		}
	}
}
