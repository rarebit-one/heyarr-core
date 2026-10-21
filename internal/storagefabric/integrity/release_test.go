package integrity_test

import (
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/hashing"

	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
)

// Release intent (#779, ADR-0114): on the personal profile a vault blob nothing
// pins any more may lose its last copy after the grace window, with the basis
// recorded. On the media profile ADR-0018's last-copy rule is unchanged.

func (f *fixture) release(h hashing.Hash, cause string) {
	f.t.Helper()
	b := f.cat.blobs[h.String()]
	b.ReleasedAt, b.ReleaseCause = f.clock.Now(), cause
}

func (f *fixture) collectorHonouring(d integrity.Durability, honour bool) *integrity.Collector {
	f.t.Helper()
	opts := f.options()
	opts.Durability = d
	opts.HonourReleases = honour
	c, err := integrity.NewCollector(opts)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// The two-site leak this exists to close: no other peer claims the blob, so
// without release intent it is spared as no_other_peer on every sweep forever.
func TestGCReclaimsTheLastCopyOfAReleasedBlobOnlyWhenHonoured(t *testing.T) {
	f := newFixture(t)
	dur := f.twoPeers()
	h := f.put("a vault blob its owner unpinned", 0)
	f.release(h, "unpinned")

	out := sweepTwice(t, f, f.collectorHonouring(dur, true))

	if len(out.Reclaimed) != 1 || out.Reclaimed[0].Hash != h.String() {
		t.Fatalf("reclaimed %+v, want the released blob", out.Reclaimed)
	}
	ev := f.cat.evidence[h.String()]
	if len(ev) != 1 || ev[0].Basis != integrity.BasisReleased {
		t.Fatalf("evidence = %+v, want one row with basis %q", ev, integrity.BasisReleased)
	}
	if dur.askedAbout(peerA, h) {
		t.Error("the other peer was asked about a released blob; release intent replaces the durability check")
	}
}

// The media profile: the same released blob is still the last copy in the
// fabric, and ADR-0018 spares it.
func TestGCWithoutHonourReleasesKeepsTheLastCopyRule(t *testing.T) {
	f := newFixture(t)
	dur := f.twoPeers()
	h := f.put("a released blob on a media node", 0)
	f.release(h, "unpinned")

	out := sweepTwice(t, f, f.collectorHonouring(dur, false))

	if len(out.Reclaimed) != 0 {
		t.Fatalf("reclaimed %+v without HonourReleases", out.Reclaimed)
	}
	if s := sparedFor(t, out, h); s.Reason != integrity.ReasonNoOtherPeer {
		t.Errorf("reason = %q, want %q", s.Reason, integrity.ReasonNoOtherPeer)
	}
}

// Release intent never shortens the grace window.
func TestGCHonoursAReleaseOnlyAfterTheGraceWindow(t *testing.T) {
	f := newFixture(t)
	dur := f.twoPeers()
	h := f.put("released a moment ago", 0)
	f.release(h, "withdrawn")
	c := f.collectorHonouring(dur, true)
	opts := integrity.CollectOptions{Apply: true, Grace: 24 * time.Hour}
	for _, step := range []time.Duration{0, 12 * time.Hour} {
		f.clock.advance(step)
		out, err := c.Collect(t.Context(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Reclaimed) != 0 {
			t.Fatalf("+%s: reclaimed %+v inside the grace window", step, out.Reclaimed)
		}
	}
}

// A blob wanted again (re-pinned, or referenced) is no longer released: the
// sweep clears the release with the grace window, and the last-copy rule
// applies again when it next goes unreferenced.
func TestGCClearsTheReleaseOfABlobThatIsWantedAgain(t *testing.T) {
	f := newFixture(t)
	dur := f.twoPeers()
	h := f.put("released, then pinned again", 1)
	f.release(h, "unpinned")
	c := f.collectorHonouring(dur, true)
	if _, err := c.Collect(t.Context(), integrity.CollectOptions{Apply: true, Grace: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if b := f.cat.blobs[h.String()]; !b.ReleasedAt.IsZero() {
		t.Fatal("a referenced blob kept its release")
	}

	f.cat.blobs[h.String()].References = 0 // dropped by a path that records no release
	out := sweepTwice(t, f, c)
	if len(out.Reclaimed) != 0 {
		t.Fatalf("reclaimed %+v on a release that had been cleared", out.Reclaimed)
	}
}

// The sweep-wide refusals still stand: a node cut off from its controller
// deletes nothing, released or not (§53).
func TestGCDoesNotHonourAReleaseWhileTheControllerIsUnreachable(t *testing.T) {
	f := newFixture(t)
	dur := f.twoPeers()
	dur.controller = integrity.ErrControllerUnreachable
	h := f.put("released while cut off", 0)
	f.release(h, "unpinned")

	out := sweepTwice(t, f, f.collectorHonouring(dur, true))

	if len(out.Reclaimed) != 0 {
		t.Fatalf("reclaimed %+v with the controller unreachable", out.Reclaimed)
	}
	if s := sparedFor(t, out, h); s.Reason != integrity.ReasonControllerUnreachable {
		t.Errorf("reason = %q, want %q", s.Reason, integrity.ReasonControllerUnreachable)
	}
}

// A release that went stale between the sweep's read and the delete (a pin
// came back) is refused by the catalog in the deleting transaction; the sweep
// spares the blob and carries on rather than failing.
func TestGCSparesAReleasedBlobTheCatalogSaysIsWantedAgain(t *testing.T) {
	f := newFixture(t)
	dur := f.twoPeers()
	h := f.put("released, then re-pinned mid-sweep", 0)
	other := f.put("released and still unwanted", 0)
	f.release(h, "unpinned")
	f.release(other, "unpinned")
	f.cat.wantedAgain = map[string]bool{h.String(): true}

	out := sweepTwice(t, f, f.collectorHonouring(dur, true))

	if s := sparedFor(t, out, h); s.Reason != integrity.ReasonWantedAgain {
		t.Errorf("reason = %q, want %q", s.Reason, integrity.ReasonWantedAgain)
	}
	if len(out.Reclaimed) != 1 || out.Reclaimed[0].Hash != other.String() {
		t.Fatalf("reclaimed %+v, want only the blob still released", out.Reclaimed)
	}
}

// A reference that lands after Reclaim committed but before the unlink keeps
// the bytes: the released unlink runs behind a final check under the catalog's
// write lock, and the sweep spares the blob.
func TestGCKeepsTheBytesOfAReleasedBlobReferencedBeforeTheUnlink(t *testing.T) {
	f := newFixture(t)
	dur := f.twoPeers()
	h := f.put("re-pinned between the row delete and the unlink", 0)
	f.release(h, "unpinned")
	f.cat.referencedAtUnlink = map[string]bool{h.String(): true}

	out := sweepTwice(t, f, f.collectorHonouring(dur, true))

	if s := sparedFor(t, out, h); s.Reason != integrity.ReasonWantedAgain {
		t.Errorf("reason = %q, want %q", s.Reason, integrity.ReasonWantedAgain)
	}
	if len(out.Reclaimed) != 0 {
		t.Fatalf("reclaimed %+v", out.Reclaimed)
	}
	if _, err := f.store.Stat(t.Context(), h); err != nil {
		t.Fatalf("the bytes were unlinked: %v", err)
	}
}
