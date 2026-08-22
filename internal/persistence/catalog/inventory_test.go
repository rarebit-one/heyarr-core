package catalog_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/peer/inventory"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// M4-07's acceptance, against a real database (§19, §20, §21, ADR-0029).
//
// The property every test here is about is one sentence: `replicas` is a cache
// of what a PEER reported about its own disk, not a record of what this node
// believes. Two things follow that these assert rather than assume.
//
// First, the rows describe a machine that is not this one. Every writer in this
// package before ReconcileInventory resolves the self peer first, so a test
// that only checked "a row exists" would pass on a system that had written it
// against itself — which is the state of the world before this issue and is
// exactly what it is meant to change.
//
// Second, a report can take a replica AWAY. A design in which inventory only
// ever adds converges on a table that never shrinks and always claims the
// library is safer than it is, and that table is what garbage collection reads
// before deleting the last copy. So the removal test reproduces `present`
// first and asserts the TRANSITION: asserting only the end state would pass on
// a system that never wrote the row at all.

// remotePeer is a peer that is emphatically not this node.
const remotePeer = "01990000-0000-7000-8000-0000000remot"

var observed = time.Date(2026, 8, 1, 6, 0, 0, 0, time.UTC)

// seedRemotePeer enrols a second peer, and asserts it is not the self peer —
// because every assertion below about "the remote peer's rows" is worthless if
// the two ids are the same.
func (h *harness) seedRemotePeer(t *testing.T) string {
	t.Helper()
	h.exec(t, `INSERT INTO peers (id, name, site, mode, is_self, created_at, enrolled_at)
		VALUES (?, 'remote-peer', 'site-b', 'full', 0, ?, ?)`, remotePeer, stamp, stamp)
	self, err := h.cat.SelfPeer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if self == remotePeer {
		t.Fatalf("the fixture's remote peer IS the self peer (%s); nothing below would prove anything", self)
	}
	return self
}

// seedBlobs gives the catalog blob rows to hang replicas from. A replica of a
// blob the controller has never heard of cannot be recorded — see the unknown
// test at the bottom.
func (h *harness) seedBlobs(t *testing.T, hashes ...string) {
	t.Helper()
	for _, hash := range hashes {
		h.exec(t, `INSERT INTO blobs (hash, size, first_seen_at) VALUES (?, 1024, ?)`, hash, stamp)
	}
}

// replicaRow is one row as the tests compare them: everything the table says,
// so a comparison cannot pass by ignoring the column that differs.
type replicaRow struct {
	BlobHash     string
	PeerID       string
	State        string
	BytesPresent int64
	VerifiedAt   string
	ReportedAt   string
}

func (r replicaRow) String() string {
	return fmt.Sprintf("%s@%s state=%s bytes=%d verified=%q reported=%q",
		r.BlobHash, r.PeerID, r.State, r.BytesPresent, r.VerifiedAt, r.ReportedAt)
}

// replicas reads the whole table, ordered, so two runs can be compared as
// values rather than as counts.
//
// updated_at is deliberately excluded: it is the CONTROLLER's clock at the
// moment of the write, and two reports that produce the same state legitimately
// land at different instants. Everything a reader of this table acts on is
// here.
func (h *harness) replicas(t *testing.T) []replicaRow {
	t.Helper()
	rows, err := h.db.Reader().Query(`
		SELECT blob_hash, peer_id, state, bytes_present,
		       coalesce(verified_at, ''), coalesce(reported_at, '')
		FROM replicas ORDER BY blob_hash, peer_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []replicaRow
	for rows.Next() {
		var r replicaRow
		if err := rows.Scan(&r.BlobHash, &r.PeerID, &r.State, &r.BytesPresent,
			&r.VerifiedAt, &r.ReportedAt); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// replicaOf reads one peer's row for one blob, failing if there is none.
func (h *harness) replicaOf(t *testing.T, hash, peerID string) replicaRow {
	t.Helper()
	for _, r := range h.replicas(t) {
		if r.BlobHash == hash && r.PeerID == peerID {
			return r
		}
	}
	t.Fatalf("no replicas row for %s on peer %s", hash, peerID)
	return replicaRow{}
}

// hasReplica reports whether a row exists at all, without failing.
func (h *harness) hasReplica(t *testing.T, hash, peerID string) bool {
	t.Helper()
	for _, r := range h.replicas(t) {
		if r.BlobHash == hash && r.PeerID == peerID {
			return true
		}
	}
	return false
}

func hashOf(c byte) string { return "blake3:" + strings.Repeat(string(c), 64) }

func report(mode inventory.Mode, at time.Time, entries ...inventory.Entry) inventory.Report {
	return inventory.Report{PeerID: remotePeer, Mode: mode, ObservedAt: at, Entries: entries}
}

func present(hash string, bytes int64) inventory.Entry {
	return inventory.Entry{BlobHash: hash, State: inventory.StatePresent, BytesPresent: bytes}
}

// ---------------------------------------------------------------------------
// the first non-self replicas rows this system has ever held

func TestAPeerReportProducesReplicaRowsForThatRemotePeer(t *testing.T) {
	h := newHarness(t)
	self := h.seedRemotePeer(t)
	a, b := hashOf('a'), hashOf('b')
	h.seedBlobs(t, a, b)

	out, err := h.cat.ReconcileInventory(context.Background(), remotePeer,
		report(inventory.ModeFull, observed, present(a, 1024), present(b, 2048)))
	if err != nil {
		t.Fatal(err)
	}
	if out.Added != 2 {
		t.Errorf("added = %d, want 2", out.Added)
	}
	if out.PeerID != remotePeer {
		t.Errorf("the outcome names peer %s, want the acting peer %s", out.PeerID, remotePeer)
	}

	got := h.replicas(t)
	if len(got) != 2 {
		t.Fatalf("replicas holds %d rows, want 2:\n%v", len(got), got)
	}
	for _, r := range got {
		// The assertion this file exists for. Every writer in this package
		// before ReconcileInventory resolves SelfPeer first, so "a row exists"
		// is not evidence of anything: it has to be a row about the OTHER
		// machine.
		if r.PeerID != remotePeer {
			t.Errorf("%s describes peer %s, want the remote peer %s", r, r.PeerID, remotePeer)
		}
		if r.PeerID == self {
			t.Errorf("%s describes THIS node — the report was folded in against the self peer", r)
		}
		if r.State != "present" {
			t.Errorf("%s state = %s, want present", r, r.State)
		}
		// Freshness: the peer confirmed these by naming them.
		if r.ReportedAt != observed.Format(time.RFC3339Nano) {
			t.Errorf("%s reported_at = %q, want the report's observation time %q",
				r, r.ReportedAt, observed.Format(time.RFC3339Nano))
		}
	}
	if h.replicaOf(t, a, remotePeer).BytesPresent != 1024 {
		t.Errorf("bytes_present for %s = %d, want 1024", a, h.replicaOf(t, a, remotePeer).BytesPresent)
	}

	// One event per report CYCLE, not one per blob (internal/events).
	if n := h.eventsOfType(t, events.TypeSyncInventoryReported); n != 1 {
		t.Errorf("%s emitted %d times, want exactly 1 per report cycle", events.TypeSyncInventoryReported, n)
	}
	if n := h.eventsOfType(t, events.TypeReplicaPresent); n != 0 {
		t.Errorf("a first inventory emitted %d per-blob replica.present events; "+
			"learning a fact is not a transition, and a hundred-thousand-blob peer "+
			"must not put a hundred thousand records in the log", n)
	}
}

// ---------------------------------------------------------------------------
// a peer that stops holding a blob

// TestAPeerThatStopsHoldingABlobMovesTheRowToMissing is the consequence that
// gets discovered late, asserted as a TRANSITION.
//
// It drives the real collector over a real CAS on a real filesystem, because
// the path that matters is "the bytes left the disk and the next report did
// not mention them". A test that hand-built the second report would be
// asserting that this file knows how to describe a loss, not that the peer
// does.
func TestAPeerThatStopsHoldingABlobMovesTheRowToMissing(t *testing.T) {
	h := newHarness(t)
	h.seedRemotePeer(t)
	ctx := context.Background()

	store, err := cas.OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	kept := putBlob(t, store, "the blob this peer keeps")
	lost := putBlob(t, store, "the blob this peer is about to lose")
	h.seedBlobs(t, kept.String(), lost.String())

	first, err := inventory.Collect(ctx, inventory.Options{
		Store: store, Quarantine: store, Now: func() time.Time { return observed },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.cat.ReconcileInventory(ctx, remotePeer, first.Full(remotePeer)); err != nil {
		t.Fatal(err)
	}

	// Reproduce `present` FIRST. Asserting only the end state would pass on a
	// system that never wrote the row: `missing` and "there was never a row"
	// are indistinguishable from the far end of the transition.
	before := h.replicaOf(t, lost.String(), remotePeer)
	if before.State != "present" {
		t.Fatalf("%s: the row is not present before the loss, so the transition below proves nothing", before)
	}
	if before.BytesPresent == 0 {
		t.Fatalf("%s: bytes_present is 0 before the loss", before)
	}

	// The bytes leave the disk. Nothing tells the controller.
	if err := store.Delete(ctx, lost); err != nil {
		t.Fatal(err)
	}

	later := observed.Add(time.Hour)
	second, err := inventory.Collect(ctx, inventory.Options{
		Store: store, Quarantine: store, Now: func() time.Time { return later },
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := h.cat.ReconcileInventory(ctx, remotePeer, second.Full(remotePeer))
	if err != nil {
		t.Fatal(err)
	}

	after := h.replicaOf(t, lost.String(), remotePeer)
	if after.State != "missing" {
		t.Errorf("%s: after the peer stopped holding it, state = %s, want missing", after, after.State)
	}
	if after.BytesPresent != 0 {
		t.Errorf("%s: a missing replica still claims %d bytes", after, after.BytesPresent)
	}
	// Missing, not gone. A deleted row is indistinguishable from one that was
	// never written, and "this peer used to have it" is the fact garbage
	// collection needs.
	if !h.hasReplica(t, lost.String(), remotePeer) {
		t.Error("the row was deleted rather than marked missing; a peer losing bytes must stay visible")
	}
	if out.Removed != 1 {
		t.Errorf("removed = %d, want 1", out.Removed)
	}
	// The blob it still holds is untouched.
	if got := h.replicaOf(t, kept.String(), remotePeer); got.State != "present" {
		t.Errorf("%s: the blob the peer still holds stopped being present", got)
	}
	// present → missing IS a transition: the controller believed something and
	// now believes something else.
	if n := h.eventsOfType(t, events.TypeReplicaMissing); n != 1 {
		t.Errorf("%s emitted %d times, want 1", events.TypeReplicaMissing, n)
	}
}

// TestAnIncrementalReportAlsoTakesAReplicaAway is the same property through
// the other shape. An incremental report cannot say "and nothing else", so it
// says the loss out loud — and if it could not, a peer that reports
// incrementally would never be able to give a replica back.
func TestAnIncrementalReportAlsoTakesAReplicaAway(t *testing.T) {
	h := newHarness(t)
	h.seedRemotePeer(t)
	ctx := context.Background()
	a, b := hashOf('a'), hashOf('b')
	h.seedBlobs(t, a, b)

	if _, err := h.cat.ReconcileInventory(ctx, remotePeer,
		report(inventory.ModeFull, observed, present(a, 1024), present(b, 2048))); err != nil {
		t.Fatal(err)
	}
	if got := h.replicaOf(t, a, remotePeer); got.State != "present" {
		t.Fatalf("%s: not present before the loss", got)
	}

	later := observed.Add(time.Hour)
	out, err := h.cat.ReconcileInventory(ctx, remotePeer, report(inventory.ModeIncremental, later,
		inventory.Entry{BlobHash: a, State: inventory.StateMissing}))
	if err != nil {
		t.Fatal(err)
	}
	if out.Removed != 1 {
		t.Errorf("removed = %d, want 1", out.Removed)
	}
	if got := h.replicaOf(t, a, remotePeer); got.State != "missing" {
		t.Errorf("%s: an explicit missing entry left the row at %s", got, got.State)
	}
	if got := h.replicaOf(t, b, remotePeer); got.State != "present" {
		t.Errorf("%s: an incremental report touched a blob it never mentioned", got)
	}
}

// ---------------------------------------------------------------------------
// quarantine

// TestAQuarantinedBlobIsReportedCorrupt covers the case an inventory derived
// from "which files are addressable" gets wrong on its own. The peer HAS those
// bytes and cannot serve them: omitting the blob reports it gone and invites a
// replacement transfer over the evidence (ADR-0018), and reporting it present
// leaves the controller believing in a copy nothing can read.
func TestAQuarantinedBlobIsReportedCorrupt(t *testing.T) {
	h := newHarness(t)
	h.seedRemotePeer(t)
	ctx := context.Background()

	root := t.TempDir()
	store, err := cas.OpenFS(root)
	if err != nil {
		t.Fatal(err)
	}
	healthy := putBlob(t, store, "bytes that still hash to their own name")
	rotten := putBlob(t, store, "bytes that are about to be rewritten underneath us")
	h.seedBlobs(t, healthy.String(), rotten.String())

	// Corrupt it the way it actually happens: an external tool rewrites the
	// file in place (#43), and Verify finds the mismatch and quarantines it.
	corruptInPlace(t, store, rotten, "something else entirely")
	if err := store.Verify(ctx, rotten); err == nil {
		t.Fatal("Verify accepted rewritten bytes; the quarantine below never happened")
	}

	snapshot, err := inventory.Collect(ctx, inventory.Options{
		Store: store, Quarantine: store, Now: func() time.Time { return observed },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.cat.ReconcileInventory(ctx, remotePeer, snapshot.Full(remotePeer)); err != nil {
		t.Fatal(err)
	}

	got := h.replicaOf(t, rotten.String(), remotePeer)
	switch got.State {
	case "corrupt":
	case "missing":
		t.Errorf("%s: a quarantined blob was reported as gone. The peer still HAS the bytes, "+
			"and replicating over them destroys the evidence (ADR-0018)", got)
	default:
		t.Errorf("%s: state = %s, want corrupt", got, got.State)
	}
	if got.BytesPresent != 0 {
		t.Errorf("%s: a corrupt replica claims %d servable bytes", got, got.BytesPresent)
	}
	if h := h.replicaOf(t, healthy.String(), remotePeer); h.State != "present" {
		t.Errorf("%s: the healthy blob was not reported present", h)
	}
}

// ---------------------------------------------------------------------------
// idempotence (invariant 9)

func TestTwoIdenticalReportsChangeNothingAndEmitNoEvents(t *testing.T) {
	h := newHarness(t)
	h.seedRemotePeer(t)
	ctx := context.Background()
	a, b := hashOf('a'), hashOf('b')
	h.seedBlobs(t, a, b)

	rep := report(inventory.ModeFull, observed, present(a, 1024), present(b, 2048))
	if _, err := h.cat.ReconcileInventory(ctx, remotePeer, rep); err != nil {
		t.Fatal(err)
	}
	firstRows := h.replicas(t)
	if len(firstRows) == 0 {
		t.Fatal("the first report wrote nothing, so the second proves nothing")
	}
	eventsAfterFirst := h.eventCount(t)

	out, err := h.cat.ReconcileInventory(ctx, remotePeer, rep)
	if err != nil {
		t.Fatal(err)
	}

	if diff := diffRows(firstRows, h.replicas(t)); diff != "" {
		t.Errorf("a re-run of an identical report changed replicas:\n%s", diff)
	}
	if out.Added+out.Changed+out.Removed != 0 {
		t.Errorf("a re-run reported added=%d changed=%d removed=%d, want all zero",
			out.Added, out.Changed, out.Removed)
	}
	// One sync.inventory_reported for the second cycle and nothing else. The
	// cycle event is the receipt that a report arrived, which is a thing that
	// happened; per-blob events would be a hundred thousand records of nothing
	// having happened.
	if got, want := h.eventCount(t)-eventsAfterFirst, 1; got != want {
		t.Errorf("a re-run emitted %d events, want %d (the cycle receipt only)", got, want)
	}
	for _, typ := range []string{events.TypeReplicaPresent, events.TypeReplicaCorrupt, events.TypeReplicaMissing} {
		if n := h.eventsOfType(t, typ); n != 0 {
			t.Errorf("%s emitted %d times across two identical reports, want 0", typ, n)
		}
	}
}

// ---------------------------------------------------------------------------
// full and incremental agree

// TestAnIncrementalAndAFullReportOfTheSameRealityAgree compares the two
// resulting TABLES.
//
// Asserting that each is non-empty would pass on an incremental path that
// applied nothing and a full path that applied everything — which is the
// failure mode most worth catching here, because it is invisible until a peer
// that reports incrementally silently stops updating.
//
// Both databases start from the same full report, because that is the only
// starting point at which the two shapes are describing the same question: an
// incremental report is a diff, and a diff is meaningless without the state it
// is a diff from.
func TestAnIncrementalAndAFullReportOfTheSameRealityAgree(t *testing.T) {
	ctx := context.Background()
	gone, changed, steady1, steady2, arrived := hashOf('a'), hashOf('b'), hashOf('c'), hashOf('d'), hashOf('e')
	all := []string{gone, changed, steady1, steady2, arrived}

	// Reality one: four blobs.
	one, err := inventory.NewSnapshot(observed, []inventory.Entry{
		present(gone, 1024), present(changed, 2048), present(steady1, 4096), present(steady2, 8192),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Reality two, an hour later: one blob went away, one grew a verification
	// time, two are untouched, one arrived. Most of the library is unchanged,
	// which is the ordinary case and the one incremental reporting exists for.
	later := observed.Add(time.Hour)
	verified := later.Add(-time.Minute)
	two, err := inventory.NewSnapshot(later, []inventory.Entry{
		{BlobHash: changed, State: inventory.StatePresent, BytesPresent: 2048, VerifiedAt: &verified},
		present(steady1, 4096), present(steady2, 8192), present(arrived, 16384),
	})
	if err != nil {
		t.Fatal(err)
	}

	viaIncremental := newHarness(t)
	viaIncremental.seedRemotePeer(t)
	viaIncremental.seedBlobs(t, all...)
	viaFull := newHarness(t)
	viaFull.seedRemotePeer(t)
	viaFull.seedBlobs(t, all...)

	for _, h := range []*harness{viaIncremental, viaFull} {
		if _, err := h.cat.ReconcileInventory(ctx, remotePeer, one.Full(remotePeer)); err != nil {
			t.Fatal(err)
		}
	}

	full := two.Full(remotePeer)
	incremental := two.Since(one, remotePeer)
	if incremental.Mode != inventory.ModeIncremental {
		t.Fatalf("Since produced mode %q", incremental.Mode)
	}
	// The whole point of incremental reporting is that it does not ship the
	// set. This is not a size assertion for its own sake: an implementation
	// that sent everything under an incremental label would satisfy every
	// other assertion in this test.
	if len(incremental.Entries) >= len(full.Entries) {
		t.Errorf("the incremental report carries %d entries and the full one %d — it is not a diff",
			len(incremental.Entries), len(full.Entries))
	}
	if _, err := viaIncremental.cat.ReconcileInventory(ctx, remotePeer, incremental); err != nil {
		t.Fatal(err)
	}
	if _, err := viaFull.cat.ReconcileInventory(ctx, remotePeer, full); err != nil {
		t.Fatal(err)
	}

	got, want := viaIncremental.replicas(t), viaFull.replicas(t)
	if len(want) != len(all) {
		t.Fatalf("the full path produced %d rows, want %d:\n%v", len(want), len(all), want)
	}
	// Compared on what each row SAYS ABOUT THE BYTES — state, byte count,
	// verification — which is the "same replicas state" this test is about.
	//
	// reported_at is deliberately outside the comparison, and the reason is
	// not convenience: the two shapes are SUPPOSED to differ there. A full
	// report confirms an unchanged blob by naming the whole set; an
	// incremental report says nothing about it and must not re-date it. That
	// difference is asserted explicitly below rather than excluded and
	// forgotten, so this exclusion cannot quietly hide a freshness bug.
	if diff := diffRows(stateOnly(want), stateOnly(got)); diff != "" {
		t.Errorf("an incremental report and a full report of the same reality disagree:\n%s", diff)
	}
	// And the reality they agree on is the right one, so that "they agree" is
	// not satisfied by both being wrong in the same way.
	if r := viaFull.replicaOf(t, gone, remotePeer); r.State != "missing" {
		t.Errorf("%s: the blob that went away is not missing", r)
	}
	if r := viaFull.replicaOf(t, arrived, remotePeer); r.State != "present" {
		t.Errorf("%s: the blob that arrived is not present", r)
	}
	if r := viaFull.replicaOf(t, changed, remotePeer); r.VerifiedAt != verified.Format(time.RFC3339Nano) {
		t.Errorf("%s: verified_at = %q, want %q", r, r.VerifiedAt, verified.Format(time.RFC3339Nano))
	}
	// The freshness half of the comparison, asserted rather than excluded.
	//
	// Absence from a full report is an assertion and absence from an
	// incremental one is silence, so the blobs nobody touched are confirmed by
	// the full path and untouched by the incremental one. The two agree about
	// the bytes and disagree about how recently anyone looked, which is
	// exactly right — and which is why the diff above is a projection.
	for _, hash := range []string{steady1, steady2} {
		if r := viaFull.replicaOf(t, hash, remotePeer); r.ReportedAt != later.Format(time.RFC3339Nano) {
			t.Errorf("%s: a full report did not confirm an unchanged blob", r)
		}
		if r := viaIncremental.replicaOf(t, hash, remotePeer); r.ReportedAt != observed.Format(time.RFC3339Nano) {
			t.Errorf("%s: an incremental report that never mentioned this blob re-dated it to %q",
				r, r.ReportedAt)
		}
	}
}

// ---------------------------------------------------------------------------
// freshness

// TestFreshnessAdvancesOnConfirmationAndNotOnOmission is what M4-12 depends
// on: a row nobody has confirmed recently must be readable as a fact about the
// past.
func TestFreshnessAdvancesOnConfirmationAndNotOnOmission(t *testing.T) {
	h := newHarness(t)
	h.seedRemotePeer(t)
	ctx := context.Background()
	a, b := hashOf('a'), hashOf('b')
	h.seedBlobs(t, a, b)

	if _, err := h.cat.ReconcileInventory(ctx, remotePeer,
		report(inventory.ModeFull, observed, present(a, 1024), present(b, 2048))); err != nil {
		t.Fatal(err)
	}
	firstStamp := observed.Format(time.RFC3339Nano)
	for _, hash := range []string{a, b} {
		if got := h.replicaOf(t, hash, remotePeer); got.ReportedAt != firstStamp {
			t.Fatalf("%s: reported_at = %q after the first report, want %q", got, got.ReportedAt, firstStamp)
		}
	}

	// An incremental report that mentions only `a`. It asserts nothing about
	// `b`, so `b` must not be re-dated — an implementation that stamped every
	// row on every cycle would make a stale table look fresh forever, which is
	// the single most dangerous thing this column could do.
	later := observed.Add(2 * time.Hour)
	if _, err := h.cat.ReconcileInventory(ctx, remotePeer,
		report(inventory.ModeIncremental, later, present(a, 1024))); err != nil {
		t.Fatal(err)
	}

	if got := h.replicaOf(t, a, remotePeer); got.ReportedAt != later.Format(time.RFC3339Nano) {
		t.Errorf("%s: reported_at = %q after being confirmed again, want %q",
			got, got.ReportedAt, later.Format(time.RFC3339Nano))
	}
	if got := h.replicaOf(t, b, remotePeer); got.ReportedAt != firstStamp {
		t.Errorf("%s: reported_at = %q — an incremental report that never mentioned this blob "+
			"advanced its freshness, want it left at %q", got, got.ReportedAt, firstStamp)
	}
}

// TestARowNoPeerHasEverConfirmedHasNoReportedAt asserts the other half of the
// column's meaning. NULL is reachable and means exactly "nobody has confirmed
// this", which is what the migration deliberately does not backfill away.
func TestARowNoPeerHasEverConfirmedHasNoReportedAt(t *testing.T) {
	h := newHarness(t)
	self := h.seedRemotePeer(t)
	a := hashOf('a')
	h.seedBlobs(t, a)
	h.exec(t, `INSERT INTO replicas (blob_hash, peer_id, state, bytes_present, updated_at)
		VALUES (?, ?, 'present', 1024, ?)`, a, self, stamp)

	if got := h.replicaOf(t, a, self); got.ReportedAt != "" {
		t.Errorf("%s: a row written by a local path claims a peer confirmed it at %q", got, got.ReportedAt)
	}
}

// ---------------------------------------------------------------------------
// the edges

// TestAReportOfABlobThisControllerDoesNotKnowIsCountedNotRefused: a peer
// restored from a newer catalog legitimately holds bytes this controller has
// not learned about. There is no blobs row to reference, so there can be no
// replicas row — and refusing the whole report over it would let one unknown
// blob block every real one in the same cycle.
func TestAReportOfABlobThisControllerDoesNotKnowIsCountedNotRefused(t *testing.T) {
	h := newHarness(t)
	h.seedRemotePeer(t)
	known, stranger := hashOf('a'), hashOf('f')
	h.seedBlobs(t, known)

	out, err := h.cat.ReconcileInventory(context.Background(), remotePeer,
		report(inventory.ModeFull, observed, present(known, 1024), present(stranger, 2048)))
	if err != nil {
		t.Fatalf("one unknown blob refused the whole report: %v", err)
	}
	if out.Unknown != 1 {
		t.Errorf("unknown = %d, want 1", out.Unknown)
	}
	if out.Added != 1 {
		t.Errorf("added = %d, want 1 — the known blob still landed", out.Added)
	}
	if h.hasReplica(t, stranger, remotePeer) {
		t.Error("a replicas row was written for a blob with no blobs row")
	}
}

// TestAReportFromAPeerWithNoCatalogRowIsRefused: membership and the catalog
// disagreeing is an operator problem, and it has to be named rather than
// producing rows for a peer that does not exist.
func TestAReportFromAPeerWithNoCatalogRowIsRefused(t *testing.T) {
	h := newHarness(t)
	a := hashOf('a')
	h.seedBlobs(t, a)

	_, err := h.cat.ReconcileInventory(context.Background(), "01990000-0000-7000-8000-00000nobody",
		report(inventory.ModeFull, observed, present(a, 1024)))
	if err == nil {
		t.Fatal("a report from a peer with no catalog row was accepted")
	}
	if !strings.Contains(err.Error(), "no catalog row") {
		t.Errorf("error = %v, want it to name the missing peer row", err)
	}
}

// TestAFullReportWithNoEntriesEmptiesThePeer. A wiped peer must be able to say
// so: an implementation that read the empty set as "nothing to report" would
// let it keep its replicas forever, and that table is what garbage collection
// reads before deleting the last copy.
func TestAFullReportWithNoEntriesEmptiesThePeer(t *testing.T) {
	h := newHarness(t)
	h.seedRemotePeer(t)
	ctx := context.Background()
	a, b := hashOf('a'), hashOf('b')
	h.seedBlobs(t, a, b)

	if _, err := h.cat.ReconcileInventory(ctx, remotePeer,
		report(inventory.ModeFull, observed, present(a, 1024), present(b, 2048))); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{a, b} {
		if got := h.replicaOf(t, hash, remotePeer); got.State != "present" {
			t.Fatalf("%s: not present before the wipe", got)
		}
	}

	out, err := h.cat.ReconcileInventory(ctx, remotePeer,
		report(inventory.ModeFull, observed.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if out.Removed != 2 {
		t.Errorf("removed = %d, want 2", out.Removed)
	}
	for _, hash := range []string{a, b} {
		if got := h.replicaOf(t, hash, remotePeer); got.State != "missing" {
			t.Errorf("%s: a peer that reported holding nothing kept a %s replica", got, got.State)
		}
	}
}

// TestTheReportIsRecordedAgainstTheActingPeerNotTheDeclaredOne is ADR-0033's
// third rule at the storage layer.
//
// The surface compares the declaration and refuses a mismatch, and this asserts
// the layer beneath it cannot be talked round either: ReconcileInventory takes
// the acting peer as an argument and must never read report.PeerID. Without
// this, a future caller that forgot to compare would silently file one peer's
// inventory under another's id.
func TestTheReportIsRecordedAgainstTheActingPeerNotTheDeclaredOne(t *testing.T) {
	h := newHarness(t)
	self := h.seedRemotePeer(t)
	a := hashOf('a')
	h.seedBlobs(t, a)

	// A report whose body declares the SELF peer, folded in for the remote
	// peer. The catalog must obey the argument.
	rep := report(inventory.ModeFull, observed, present(a, 1024))
	rep.PeerID = self
	if _, err := h.cat.ReconcileInventory(context.Background(), remotePeer, rep); err != nil {
		t.Fatal(err)
	}

	if h.hasReplica(t, a, self) {
		t.Error("the declared peer_id in the body decided where the row landed")
	}
	if got := h.replicaOf(t, a, remotePeer); got.PeerID != remotePeer {
		t.Errorf("%s: the row did not land on the acting peer", got)
	}
}

// ---------------------------------------------------------------------------
// helpers

// putBlob writes bytes into a real store and returns their hash.
func putBlob(t *testing.T, store *cas.FS, content string) hashing.Hash {
	t.Helper()
	d, err := store.Put(context.Background(), strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return d.Hash
}

// corruptInPlace rewrites a blob's bytes behind the store's back, which is
// what an external tool sharing a hard-linked inode does (#43).
func corruptInPlace(t *testing.T, store *cas.FS, h hashing.Hash, content string) {
	t.Helper()
	path, err := store.LocalPath(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	// Blobs are stored read-only, so an external rewrite has to widen the mode
	// first — which is exactly what a tool that owns the original inode does.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// stateOnly projects rows onto what they say about the bytes, dropping
// freshness. See the comparison in TestAnIncrementalAndAFullReportOfTheSameRealityAgree
// for why that column is asserted separately rather than compared here.
func stateOnly(rows []replicaRow) []replicaRow {
	out := make([]replicaRow, len(rows))
	for i, r := range rows {
		r.ReportedAt = ""
		out[i] = r
	}
	return out
}

// diffRows renders the difference between two row sets, or "" when they match.
func diffRows(want, got []replicaRow) string {
	index := func(rows []replicaRow) map[string]replicaRow {
		out := map[string]replicaRow{}
		for _, r := range rows {
			out[r.BlobHash+"@"+r.PeerID] = r
		}
		return out
	}
	wantBy, gotBy := index(want), index(got)
	keys := map[string]bool{}
	for k := range wantBy {
		keys[k] = true
	}
	for k := range gotBy {
		keys[k] = true
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)

	var b strings.Builder
	for _, k := range ordered {
		w, inWant := wantBy[k]
		g, inGot := gotBy[k]
		switch {
		case inWant && !inGot:
			fmt.Fprintf(&b, "  only in want: %s\n", w)
		case inGot && !inWant:
			fmt.Fprintf(&b, "  only in got:  %s\n", g)
		case w != g:
			fmt.Fprintf(&b, "  want: %s\n  got:  %s\n", w, g)
		}
	}
	return b.String()
}

// eventsOfType counts events of one type in the log.
func (h *harness) eventsOfType(t *testing.T, eventType string) int {
	t.Helper()
	evs, err := h.events.Since(context.Background(), 0, []string{eventType}, 10000)
	if err != nil {
		t.Fatal(err)
	}
	return len(evs)
}
