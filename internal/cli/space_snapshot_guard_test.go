package cli

import (
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync"
)

// `space snapshot` folds only the playlist CRDT, but a snapshot's frontier covers
// every change in the space and `space compact` drops what it subsumes. A
// playlist snapshot of a vault space would let compaction delete the drive's
// changes, so the command refuses any space holding another kind (ADR-0113) and
// pushes nothing.
func TestSpaceSnapshotRefusesASpaceHoldingAnotherKind(t *testing.T) {
	h := newPSHarness(t, auth.ScopeAdmin)
	spaceID := h.createSpace()
	d := crdt.NewDrive()
	ch, err := d.Put("docs/policy.pdf", "blake3:"+strings.Repeat("ab", 32), 42, 1)
	if err != nil {
		t.Fatal(err)
	}
	pushChanges(h, spaceID, []crdt.DriveChange{ch})

	out, err := h.cli("space", "snapshot", spaceID)
	if err == nil || !strings.Contains(err.Error()+out, "snapshots only a playlist space") {
		t.Fatalf("space snapshot of a vault space: err = %v, out = %s; want a refusal", err, out)
	}
	if _, ok, err := h.client.Snapshot(h.ctx, spaceID); err != nil || ok {
		t.Fatalf("a refused snapshot must push nothing: ok=%v err=%v", ok, err)
	}
}

// The same holds when the other kind lives only in the latest snapshot (its
// changes compacted away): replacing it with a playlist snapshot would make the
// drive unreachable, so the command checks the snapshot too.
func TestSpaceSnapshotRefusesASpaceWhoseSnapshotIsAnotherKind(t *testing.T) {
	h := newPSHarness(t, auth.ScopeAdmin)
	spaceID := h.createSpace()
	d := crdt.NewDrive()
	if _, err := d.Put("docs/a.txt", "blake3:"+strings.Repeat("cd", 32), 1, 1); err != nil {
		t.Fatal(err)
	}
	snap, err := statesync.EncodeDriveSnapshot(h.mgr, spaceID, nil, d)
	if err != nil {
		t.Fatal(err)
	}
	driveSnap, err := h.client.PushSnapshot(h.ctx, snap)
	if err != nil {
		t.Fatal(err)
	}

	out, err := h.cli("space", "snapshot", spaceID)
	if err == nil || !strings.Contains(err.Error()+out, "snapshots only a playlist space") {
		t.Fatalf("space snapshot over a drive snapshot: err = %v, out = %s; want a refusal", err, out)
	}
	latest, ok, err := h.client.Snapshot(h.ctx, spaceID)
	if err != nil || !ok || latest.SnapshotID != driveSnap {
		t.Fatalf("the drive snapshot must stay the latest: got %q ok=%v err=%v", latest.SnapshotID, ok, err)
	}
}
