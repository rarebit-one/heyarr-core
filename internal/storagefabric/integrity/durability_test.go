package integrity_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
)

// ADR-0018's deferred precondition, watched declining to delete things (M4-12).
//
// The baseline below comes first on purpose and it is not a formality. Every
// refusal in this file is unfalsifiable without it: "the bytes survived" passes
// trivially against a collector that never deletes anything, so the first thing
// asserted is that this collector, on this fixture, with a verified replica on
// the other peer, DOES unlink and the bytes DO go. Only then does a test that
// watches it decline mean something.

// peerA is the other peer in every two-peer fixture here.
const peerA = "peer-a"

// fakeDurability is the remote half of the precondition, under test control.
//
// It records what it was ASKED as well as what it answered, because half of
// these tests are about a check happening at all: a collector that skipped the
// network call entirely and returned the same verdict would pass an
// answer-only assertion, and that is exactly the sabotage this file has to
// catch.
type fakeDurability struct {
	controller error
	// holds maps peer id -> the answer for every blob on that peer. nil means
	// "yes, it holds them", which is the only answer that permits a delete.
	holds map[string]error
	asked []string
}

func newFakeDurability() *fakeDurability {
	return &fakeDurability{holds: map[string]error{}}
}

func (d *fakeDurability) Controller(context.Context) error { return d.controller }

func (d *fakeDurability) Holds(_ context.Context, p integrity.Peer, h hashing.Hash) error {
	d.asked = append(d.asked, p.PeerID+"|"+h.String())
	return d.holds[p.PeerID]
}

func (d *fakeDurability) askedAbout(peerID string, h hashing.Hash) bool {
	for _, a := range d.asked {
		if a == peerID+"|"+h.String() {
			return true
		}
	}
	return false
}

// twoPeers turns the fixture into a deployment with one other peer, holding a
// present, freshly confirmed replica of everything the catalog knows about.
//
// That is the HAPPY state. Each test below then breaks exactly one thing about
// it, so what the refusal is attributable to is never in doubt.
func (f *fixture) twoPeers() *fakeDurability {
	f.t.Helper()
	f.cat.peers = []integrity.Peer{{
		PeerID: peerA, Name: "site-b", Endpoint: "https://site-b.example:8443",
		PublicKey: []byte("pinned"), Health: "reachable", LastSeenAt: f.clock.Now(),
	}}
	f.cat.replicas = map[string][]integrity.Replica{}
	return newFakeDurability()
}

// claims gives the other peer a replicas row for this blob, last confirmed
// reportedAgo before whenever the sweep happens to run.
//
// Relative to the sweep rather than to setup, and re-stamped by [restamp] every
// time the clock moves. Freshness is measured against the moment of the check —
// a peer reporting every five minutes is still reporting five minutes ago after
// the grace window has passed — so a fixture that stamped an absolute time
// would turn every case in this file into the stale case, and the staleness
// test would pass for the wrong reason.
func (f *fixture) claims(h hashing.Hash, state string, reportedAgo time.Duration) {
	f.t.Helper()
	if f.claimAge == nil {
		f.claimAge = map[string]time.Duration{}
	}
	f.claimAge[h.String()] = reportedAgo
	f.cat.replicas[h.String()] = []integrity.Replica{{
		Peer: f.cat.peers[0], State: state, BytesPresent: 1,
	}}
	f.restamp()
}

// restamp re-dates every claim against the current clock.
func (f *fixture) restamp() {
	for hash, ago := range f.claimAge {
		rows := f.cat.replicas[hash]
		for i := range rows {
			if rows[i].ReportedAt.IsZero() && f.everStamped[hash] {
				// A row a test deliberately blanked stays blank: "no peer has
				// ever confirmed this" is a fact, not a stale timestamp.
				continue
			}
			rows[i].ReportedAt = f.clock.Now().Add(-ago)
			rows[i].VerifiedAt = rows[i].ReportedAt
		}
		if f.everStamped == nil {
			f.everStamped = map[string]bool{}
		}
		f.everStamped[hash] = true
	}
}

func (f *fixture) collectorWith(d integrity.Durability) *integrity.Collector {
	f.t.Helper()
	opts := f.options()
	opts.Durability = d
	c, err := integrity.NewCollector(opts)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// sweepTwice runs the mark pass, moves the clock past the window and runs the
// reclaiming pass, returning the second one. Mark-and-sweep is two passes by
// design and a test that ran one would be asserting on the wrong pass.
func sweepTwice(t *testing.T, f *fixture, c *integrity.Collector) integrity.Collection {
	t.Helper()
	opts := integrity.CollectOptions{Apply: true, Grace: 24 * time.Hour}
	if _, err := c.Collect(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(48 * time.Hour)
	// The other peer went on reporting during those two days, as a healthy
	// peer does. See [fixture.claims].
	f.restamp()
	out, err := c.Collect(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// stillOnDisk reports whether the bytes are there, which is the only assertion
// that matters — a catalog row is a belief and this file is about bytes.
func (f *fixture) stillOnDisk(h hashing.Hash) bool {
	f.t.Helper()
	has, err := f.store.Has(f.t.Context(), h)
	if err != nil {
		f.t.Fatal(err)
	}
	return has
}

// sparedFor returns the sparing recorded against one blob, or fails.
func sparedFor(t *testing.T, out integrity.Collection, h hashing.Hash) integrity.Sparing {
	t.Helper()
	for _, s := range out.Spared {
		if s.Hash == h.String() {
			return s
		}
	}
	t.Fatalf("no refusal was recorded for %s; spared = %+v, reclaimed = %+v",
		h, out.Spared, out.Reclaimed)
	return integrity.Sparing{}
}

// BASELINE. With a verified replica on the other peer, garbage collection
// unlinks and the bytes go.
//
// Without this test every refusal below would pass against a collector that had
// simply been broken into never deleting anything, which is the failure mode
// that makes a safety feature indistinguishable from an outage.
func TestGCUnlinksWhenAnotherPeerVerifiablyHoldsTheBytes(t *testing.T) {
	f := newFixture(t)
	dur := f.twoPeers()
	h := f.put("bytes that live in two places", 0)
	f.claims(h, "present", time.Minute)
	dur.holds[peerA] = nil

	out := sweepTwice(t, f, f.collectorWith(dur))

	if len(out.Reclaimed) != 1 || out.Reclaimed[0].Hash != h.String() {
		t.Fatalf("reclaimed %+v, want the one blob %s (spared: %+v)", out.Reclaimed, h, out.Spared)
	}
	if f.stillOnDisk(h) {
		t.Error("the bytes are still in the store; the baseline is not a baseline")
	}
	// The claim was CHECKED. A collector that read the row and skipped the
	// network call would reach the same verdict here and would fail every
	// refusal below for the same reason.
	if !dur.askedAbout(peerA, h) {
		t.Errorf("the other peer was never asked whether it holds %s; asked = %v", h, dur.asked)
	}
	ev := f.cat.evidence[h.String()]
	if len(ev) != 1 {
		t.Fatalf("recorded %d pieces of durability evidence, want 1", len(ev))
	}
	if ev[0].Basis != integrity.BasisVerifiedRemote {
		t.Errorf("basis = %q, want %q", ev[0].Basis, integrity.BasisVerifiedRemote)
	}
	if ev[0].PeerID != peerA {
		t.Errorf("evidence names peer %q, want %q", ev[0].PeerID, peerA)
	}
	// Recorded BEFORE the delete, not after: replicas.blob_hash is ON DELETE
	// CASCADE, so evidence written afterwards would describe a row that no
	// longer exists (migration 00028).
	if f.cat.order[0] != "evidence:"+h.String() || f.cat.order[1] != "reclaim:"+h.String() {
		t.Errorf("order = %v, want the evidence recorded before the reclaim", f.cat.order)
	}
}

// REFUSAL 1 — the peer is unreachable, so nothing can be established.
func TestGCRefusesWhenTheOnlyOtherPeerIsUnreachable(t *testing.T) {
	f := newFixture(t)
	dur := f.twoPeers()
	h := f.put("bytes whose only other home is dark", 0)
	f.claims(h, "present", time.Minute)
	// Down, in both senses the system models: the stored health column says so
	// (internal/peer/health), and the peer would answer nothing if asked.
	f.cat.peers[0].Health = "unreachable"
	f.cat.replicas[h.String()][0].Peer.Health = "unreachable"
	dur.holds[peerA] = integrity.ErrPeerUnreachable

	out := sweepTwice(t, f, f.collectorWith(dur))

	if len(out.Reclaimed) != 0 {
		t.Fatalf("reclaimed %+v while the only other peer was down", out.Reclaimed)
	}
	if !f.stillOnDisk(h) {
		t.Fatal("the bytes are gone: garbage collection deleted the last reachable copy")
	}
	s := sparedFor(t, out, h)
	if s.Reason != integrity.ReasonPeerUnreachable {
		t.Errorf("reason = %q, want %q", s.Reason, integrity.ReasonPeerUnreachable)
	}
	if s.PeerID != peerA {
		t.Errorf("the refusal names peer %q, want %q — a refusal that does not say which peer is "+
			"not actionable", s.PeerID, peerA)
	}
	if !strings.Contains(s.Detail, "site-b") {
		t.Errorf("detail = %q, want it to name the peer an operator has to go and look at", s.Detail)
	}
	if len(f.cat.evidence) != 0 {
		t.Errorf("evidence was recorded for a blob nothing was established about: %+v", f.cat.evidence)
	}
}

// REFUSAL 2 — the peer answers, but its last confirmation is past the freshness
// bound. What is known about it is a fact about the past.
func TestGCRefusesWhenTheRemoteClaimIsStale(t *testing.T) {
	f := newFixture(t)
	dur := f.twoPeers()
	h := f.put("bytes last vouched for a long time ago", 0)
	// Reachable, and it would say yes if asked — the ONLY thing wrong is the
	// age of the claim, so nothing else can account for the refusal.
	dur.holds[peerA] = nil
	f.claims(h, "present", 30*24*time.Hour)

	out := sweepTwice(t, f, f.collectorWith(dur))

	if len(out.Reclaimed) != 0 {
		t.Fatalf("reclaimed %+v on the strength of a month-old inventory claim", out.Reclaimed)
	}
	if !f.stillOnDisk(h) {
		t.Fatal("the bytes are gone")
	}
	s := sparedFor(t, out, h)
	if s.Reason != integrity.ReasonStaleInventory {
		t.Errorf("reason = %q, want %q", s.Reason, integrity.ReasonStaleInventory)
	}
	if !strings.Contains(s.Detail, integrity.DefaultFreshness.String()) {
		t.Errorf("detail = %q, want it to state the bound the claim was measured against — "+
			"\"stale\" without a bound is unfalsifiable", s.Detail)
	}
}

// REFUSAL 2b — a row nobody has EVER confirmed is not fresh.
//
// Its own test because NULL reported_at is a different fact from an old one
// (migration 00023 declines to backfill it precisely so the two stay
// distinguishable), and because a bound implemented as now - reported_at would
// read a zero time as "confirmed in the year 1", which is stale, or as "zero
// seconds ago", which is not.
func TestGCRefusesAClaimNoPeerHasEverConfirmed(t *testing.T) {
	f := newFixture(t)
	dur := f.twoPeers()
	h := f.put("bytes claimed by a row nobody ever confirmed", 0)
	dur.holds[peerA] = nil
	f.claims(h, "present", time.Minute)
	f.cat.replicas[h.String()][0].ReportedAt = time.Time{}

	out := sweepTwice(t, f, f.collectorWith(dur))

	if len(out.Reclaimed) != 0 {
		t.Fatalf("reclaimed %+v on a claim no peer has ever confirmed", out.Reclaimed)
	}
	if s := sparedFor(t, out, h); s.Reason != integrity.ReasonStaleInventory {
		t.Errorf("reason = %q, want %q", s.Reason, integrity.ReasonStaleInventory)
	}
}

// REFUSAL 3 — the lying row. `replicas` says present; the peer, asked, says it
// does not hold the bytes.
//
// This is the refusal that looks excessive and is not: a replicas row is the
// controller's belief about a machine it is not, and the premise of this
// milestone is that beliefs and bytes diverge. Both halves are asserted — the
// refusal, and the correction of the row, because an uncorrected lie goes on
// offering the same false assurance to every later sweep.
func TestGCRefusesAndCorrectsARowClaimingAReplicaThePeerDoesNotHave(t *testing.T) {
	f := newFixture(t)
	dur := f.twoPeers()
	h := f.put("bytes the catalog thinks are safe elsewhere", 0)
	// Present, fresh, reachable. Everything the catalog can see is fine, and
	// the catalog is wrong.
	f.claims(h, "present", time.Minute)
	dur.holds[peerA] = integrity.ErrPeerLacksBlob

	out := sweepTwice(t, f, f.collectorWith(dur))

	if len(out.Reclaimed) != 0 {
		t.Fatalf("reclaimed %+v on the strength of a row the peer contradicted", out.Reclaimed)
	}
	if !f.stillOnDisk(h) {
		t.Fatal("the bytes are gone: the last real copy was deleted because a table said otherwise")
	}
	s := sparedFor(t, out, h)
	if s.Reason != integrity.ReasonRemoteLacksBlob {
		t.Errorf("reason = %q, want %q", s.Reason, integrity.ReasonRemoteLacksBlob)
	}
	// The correction. Without it the next sweep rediscovers the same lie, and
	// so does read routing, and so does replication.
	want := h.String() + "|" + peerA
	if len(f.cat.corrected) != 1 || f.cat.corrected[0] != want {
		t.Errorf("corrected = %v, want the row %s moved to missing", f.cat.corrected, want)
	}
}

// §53: a peer cut off from the controller does not delete replicas.
//
// The spec's degraded-operation table has said this all along and garbage
// collection had no way to tell — it runs on local SQLite and a local CAS and
// consulted nothing.
func TestGCUnlinksNothingWhenTheControllerCannotBeReached(t *testing.T) {
	f := newFixture(t)
	dur := f.twoPeers()
	h := f.put("bytes a cut-off peer must not touch", 0)
	f.claims(h, "present", time.Minute)
	// Everything about the OTHER PEER is fine. Only the controller is gone,
	// so nothing but §53 can account for the refusal.
	dur.holds[peerA] = nil
	dur.controller = integrity.ErrControllerUnreachable

	// Untracked bytes too: a degraded node unlinks nothing addressable.
	orphan := f.putUnrecorded("orphaned bytes on a cut-off peer", 30*24*time.Hour)

	out := sweepTwice(t, f, f.collectorWith(dur))

	if len(out.Reclaimed) != 0 || len(out.Untracked) != 0 {
		t.Fatalf("a peer cut off from the controller reclaimed %+v and unlinked %+v (§53)",
			out.Reclaimed, out.Untracked)
	}
	if !f.stillOnDisk(h) || !f.stillOnDisk(orphan) {
		t.Fatal("bytes were unlinked while the controller was unreachable (§53)")
	}
	if len(out.Refusals) == 0 || out.Refusals[0].Reason != integrity.ReasonControllerUnreachable {
		t.Fatalf("refusals = %+v, want one naming the controller", out.Refusals)
	}
	if s := sparedFor(t, out, h); s.Reason != integrity.ReasonControllerUnreachable {
		t.Errorf("reason = %q, want %q", s.Reason, integrity.ReasonControllerUnreachable)
	}
}

// A collector with another peer to be durable on and no way to ask it anything
// refuses. A missing dependency must never read as a satisfied condition.
func TestGCRefusesWhenThereIsAPeerAndNoWayToAskIt(t *testing.T) {
	f := newFixture(t)
	f.twoPeers()
	h := f.put("bytes a collector with no peer client must not touch", 0)
	f.claims(h, "present", time.Minute)

	// Deliberately no Durability at all — the shape a wiring mistake produces.
	out := sweepTwice(t, f, f.collectorWith(nil))

	if len(out.Reclaimed) != 0 {
		t.Fatalf("reclaimed %+v with no way to establish anything", out.Reclaimed)
	}
	if s := sparedFor(t, out, h); s.Reason != integrity.ReasonDurabilityUnwired {
		t.Errorf("reason = %q, want %q", s.Reason, integrity.ReasonDurabilityUnwired)
	}
}

// A deployment with exactly one peer still collects, and records what it relied
// on.
//
// The one deliberate exemption, asserted so it stays deliberate. Refusing here
// would protect nothing and would mean no single-node Heyarr could ever reclaim
// a byte — a different way of losing a library. The evidence row is what keeps
// it from being a silent hole: the basis is written down and it is not
// `verified_remote`.
func TestASolePeerStillCollectsAndRecordsThatAsTheBasis(t *testing.T) {
	f := newFixture(t)
	h := f.put("bytes on the only peer there is", 0)

	out := sweepTwice(t, f, f.collectorWith(nil))

	if len(out.Reclaimed) != 1 {
		t.Fatalf("reclaimed %+v, want the one blob (spared: %+v)", out.Reclaimed, out.Spared)
	}
	if f.stillOnDisk(h) {
		t.Error("the bytes are still there")
	}
	ev := f.cat.evidence[h.String()]
	if len(ev) != 1 || ev[0].Basis != integrity.BasisSolePeer {
		t.Fatalf("evidence = %+v, want one row with the %q basis", ev, integrity.BasisSolePeer)
	}
	if ev[0].PeerID != "" {
		t.Errorf("sole-peer evidence names peer %q; the point is that there is no other peer",
			ev[0].PeerID)
	}
}

// A blob no other peer claims at all is the last copy in the fabric.
func TestGCRefusesWhenNoOtherPeerClaimsTheBlob(t *testing.T) {
	f := newFixture(t)
	dur := f.twoPeers()
	h := f.put("bytes only this peer has ever held", 0)
	// No claims() call: peer A exists and has no row for this blob.
	dur.holds[peerA] = nil

	out := sweepTwice(t, f, f.collectorWith(dur))

	if len(out.Reclaimed) != 0 {
		t.Fatalf("reclaimed %+v, which no other peer claims", out.Reclaimed)
	}
	if s := sparedFor(t, out, h); s.Reason != integrity.ReasonNoOtherPeer {
		t.Errorf("reason = %q, want %q", s.Reason, integrity.ReasonNoOtherPeer)
	}
}

// A row that already says missing, corrupt or pending is a claim NOT to hold
// the bytes, and it is refused before anything is dialled.
func TestGCRefusesAReplicaRowThatDoesNotClaimToHoldTheBytes(t *testing.T) {
	for _, state := range []string{"missing", "corrupt", "pending"} {
		t.Run(state, func(t *testing.T) {
			f := newFixture(t)
			dur := f.twoPeers()
			h := f.put("bytes the other peer does not claim: "+state, 0)
			f.claims(h, state, time.Minute)
			dur.holds[peerA] = nil

			out := sweepTwice(t, f, f.collectorWith(dur))

			if len(out.Reclaimed) != 0 {
				t.Fatalf("reclaimed %+v against a %s replica", out.Reclaimed, state)
			}
			if s := sparedFor(t, out, h); s.Reason != integrity.ReasonReplicaNotPresent {
				t.Errorf("reason = %q, want %q", s.Reason, integrity.ReasonReplicaNotPresent)
			}
			if dur.askedAbout(peerA, h) {
				t.Error("a peer whose own row says it does not hold the bytes was dialled anyway")
			}
		})
	}
}

// An empty catalog against a populated store sweeps NOTHING, and says why.
//
// This is finding 2, and it is a pre-existing hazard rather than a new one:
// untracked bytes were unlinked with no second opinion, so a database pointed
// at the wrong CAS — or an empty one, or a catalog snapshot mistaken for a
// catalog (#145) — deleted the library on a single `gc --apply`. out.Considered
// has existed since M1 to make "GC removed nothing" falsifiable and was never
// used as a precondition. Here it is one.
func TestAnEmptyCatalogAgainstAPopulatedStoreSweepsNothing(t *testing.T) {
	f := newFixture(t)
	var orphans []hashing.Hash
	for i := range integrity.VacuityFloor {
		orphans = append(orphans, f.putUnrecorded(fmt.Sprintf("library file %d", i), 30*24*time.Hour))
	}

	out, err := f.collector().Collect(t.Context(), integrity.CollectOptions{Apply: true, Grace: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	if out.Considered != 0 {
		t.Fatalf("considered = %d, want 0 — this fixture is meant to be an EMPTY catalog", out.Considered)
	}
	if len(out.Untracked) != 0 {
		t.Fatalf("unlinked %d untracked files against an empty catalog", len(out.Untracked))
	}
	for _, h := range orphans {
		if !f.stillOnDisk(h) {
			t.Fatalf("%s was unlinked: an empty database deleted the library", h)
		}
	}
	if len(out.Refusals) == 0 || out.Refusals[0].Reason != integrity.ReasonCatalogVacuous {
		t.Fatalf("refusals = %+v, want one naming the vacuous catalog", out.Refusals)
	}
	if len(out.UntrackedSpared) != len(orphans) {
		t.Errorf("spared %d untracked files, want all %d named rather than counted",
			len(out.UntrackedSpared), len(orphans))
	}
	if !strings.Contains(out.Refusals[0].Detail, "wrong place") {
		t.Errorf("detail = %q, want it to say what an operator should suspect", out.Refusals[0].Detail)
	}
}

// The other half of the guard: a genuine handful of orphans against a real
// catalog is still swept.
//
// Asserted because a guard that refused everything would pass the test above
// and would silently disable the orphan cleanup M1-10 built. The floor is what
// keeps the ratio from firing where it has no information in it.
func TestAFewOrphansAgainstARealCatalogAreStillSwept(t *testing.T) {
	f := newFixture(t)
	for i := range 20 {
		f.put(fmt.Sprintf("tracked content %d", i), 1)
	}
	orphan := f.putUnrecorded("one rolled-back ingest", 30*24*time.Hour)

	out, err := f.collector().Collect(t.Context(), integrity.CollectOptions{Apply: true, Grace: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Untracked) != 1 || out.Untracked[0].Hash != orphan.String() {
		t.Fatalf("untracked = %+v, want the one orphan %s (refusals: %+v)",
			out.Untracked, orphan, out.Refusals)
	}
	if f.stillOnDisk(orphan) {
		t.Error("the orphan survived; M1-10's cleanup has been disabled by the guard")
	}
}

// THE PROPERTY. Over random two-peer replica states: for every blob this sweep
// unlinked locally, some other peer verifiably held it at that moment.
//
// Shaped like TestGCNeverRemovesABlobAnAssetStillReferences in internal/worker,
// and for the same reason — a bug here destroys user data irreversibly, so it
// is asserted over generated states rather than over the handful of cases
// somebody thought of. The counters at the end are the anti-vacuity guard on
// the test itself: a run that unlinked nothing at all would satisfy the
// property and prove nothing, so it fails.
func TestGCOnlyUnlinksBlobsAnotherPeerVerifiablyHeld(t *testing.T) {
	const states = 40
	var (
		totalBlobs     int
		totalReclaimed int
		totalSpared    int
		reasons        = map[integrity.Reason]int{}
	)

	for seed := range uint64(states) {
		t.Run(fmt.Sprintf("state-%d", seed), func(t *testing.T) {
			f := newFixture(t)
			dur := f.twoPeers()
			rng := rand.New(rand.NewPCG(seed, 0x5DEECE66D))

			// truth is what the other peer ACTUALLY holds. The catalog's rows
			// are generated independently of it, which is the whole point:
			// this fabric is one where beliefs and bytes diverge.
			truth := map[string]bool{}
			blobs := map[string]hashing.Hash{}
			n := 3 + rng.IntN(6)
			for i := range n {
				h := f.put(fmt.Sprintf("state %d blob %d", seed, i), 0)
				blobs[h.String()] = h
				truth[h.String()] = rng.IntN(2) == 0
			}
			totalBlobs += n

			// The peer's reachability, and per-blob rows that may say anything.
			reachable := rng.IntN(4) != 0
			if !reachable {
				f.cat.peers[0].Health = "unreachable"
			}
			for hash, h := range blobs {
				switch rng.IntN(5) {
				case 0:
					// No row at all.
				case 1:
					f.claims(h, "missing", time.Minute)
				case 2:
					f.claims(h, "present", 30*24*time.Hour) // stale
				default:
					f.claims(h, "present", time.Minute)
				}
				if rows, ok := f.cat.replicas[hash]; ok {
					rows[0].Peer.Health = f.cat.peers[0].Health
				}
			}
			// The remote answers from truth, not from the rows.
			dur.holds[peerA] = nil
			if !reachable {
				dur.holds[peerA] = integrity.ErrPeerUnreachable
			}
			out := sweepTwice(t, f, f.collectorWith(&truthfulDurability{
				fake: dur, truth: truth, reachable: reachable,
			}))

			totalReclaimed += len(out.Reclaimed)
			totalSpared += len(out.Spared)
			for _, s := range out.Spared {
				reasons[s.Reason]++
			}

			if out.Considered != n {
				t.Fatalf("considered %d blobs, want %d — the sweep did not look at this state",
					out.Considered, n)
			}
			// THE PROPERTY.
			for _, c := range out.Reclaimed {
				if !truth[c.Hash] {
					t.Fatalf("unlinked %s, which no other peer actually held", c.Hash)
				}
				if f.stillOnDisk(blobs[c.Hash]) {
					t.Fatalf("%s was reported reclaimed and is still on disk", c.Hash)
				}
			}
			// And the converse, so the property cannot be satisfied by never
			// deleting: every spared blob is still there.
			for _, s := range out.Spared {
				if !f.stillOnDisk(blobs[s.Hash]) {
					t.Fatalf("%s was spared and its bytes are gone anyway", s.Hash)
				}
			}
			if len(out.Reclaimed)+len(out.Spared) != n {
				t.Fatalf("%d reclaimed + %d spared != %d eligible: some blob was neither",
					len(out.Reclaimed), len(out.Spared), n)
			}
		})
	}

	// The test's own non-vacuity. A property about what a sweep unlinked that
	// never watched a sweep unlink anything is decoration, and so is one that
	// never watched it refuse.
	if totalReclaimed == 0 {
		t.Fatalf("across %d states the collector unlinked nothing; the property is vacuous", states)
	}
	if totalSpared == 0 {
		t.Fatalf("across %d states the collector refused nothing; the property is vacuous", states)
	}
	for _, want := range []integrity.Reason{
		integrity.ReasonNoOtherPeer, integrity.ReasonReplicaNotPresent,
		integrity.ReasonStaleInventory, integrity.ReasonPeerUnreachable,
		integrity.ReasonRemoteLacksBlob,
	} {
		if reasons[want] == 0 {
			t.Errorf("no generated state produced the %q refusal, so this run never exercised it", want)
		}
	}
	t.Logf("property coverage: %d states, %d blobs, %d unlinked, %d spared, reasons %v",
		states, totalBlobs, totalReclaimed, totalSpared, reasons)
}

// truthfulDurability answers from what the peer ACTUALLY holds rather than from
// what the catalog claims, which is what makes the property test a test of
// divergence rather than of agreement.
type truthfulDurability struct {
	fake      *fakeDurability
	truth     map[string]bool
	reachable bool
}

func (d *truthfulDurability) Controller(context.Context) error { return nil }

func (d *truthfulDurability) Holds(_ context.Context, p integrity.Peer, h hashing.Hash) error {
	d.fake.asked = append(d.fake.asked, p.PeerID+"|"+h.String())
	if !d.reachable {
		return integrity.ErrPeerUnreachable
	}
	if !d.truth[h.String()] {
		return integrity.ErrPeerLacksBlob
	}
	return nil
}
