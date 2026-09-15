package crdt

import (
	"math/rand"
	"testing"
)

// buildDriveChangeset returns a deterministic set of drive writes exercising the
// three shapes the skeleton implements — a linear successor (edit), a genuine
// concurrent divergence (conflict), and a delete — together with the drive that
// results from folding them. Writers are minted once, so every permutation folds
// the SAME writes (§43).
func buildDriveChangeset(t *testing.T) (changes []DriveChange, want *Drive) {
	t.Helper()

	// Device A: create then edit the same path (a clean successor), and delete a
	// second path.
	a := NewDrive()
	c1 := a.Put("Docs/tax.pdf", "blobA", 10, 1)
	c2 := a.Put("Docs/tax.pdf", "blobB", 20, 2) // observed blobA -> Base = c1's key
	c3 := a.Delete("notes.txt")

	// Devices B and D, offline, write different blobs to one fresh path with no
	// live head observed (Base zero on both) — a genuine concurrent divergence.
	b := NewDrive()
	c4 := b.Put("photo.jpg", "blobX", 30, 3)
	d := NewDrive()
	c5 := d.Put("photo.jpg", "blobY", 40, 4)

	changes = []DriveChange{c1, c2, c3, c4, c5}

	want = NewDrive()
	want.Apply(changes...)
	return changes, want
}

func permuteDrive(src *rand.Rand, changes []DriveChange) []DriveChange {
	out := make([]DriveChange, len(changes))
	copy(out, changes)
	src.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// TestDriveConvergesUnderReordering is the headline property (§43): the same set
// of writes folds to a byte-identical drive regardless of arrival order — the
// property an arrival-ordered heads model failed (a write arriving before its
// Base diverged), and the reason heads/history are derived from the whole set.
func TestDriveConvergesUnderReordering(t *testing.T) {
	changes, want := buildDriveChangeset(t)
	src := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		got := NewDrive()
		got.Apply(permuteDrive(src, changes)...)
		if got.Encode() != want.Encode() {
			t.Fatalf("order %d diverged:\n got=%s\nwant=%s", i, got.Encode(), want.Encode())
		}
	}
}

// TestDriveMergeEqualsApply asserts that folding materialised drives (MergeDrives)
// and folding the change log (Apply) reach the same state — they must, because
// both are "union the write sets, max the tombstone."
func TestDriveMergeEqualsApply(t *testing.T) {
	changes, want := buildDriveChangeset(t)
	// Split the changes across three independent drives, then merge.
	x, y, z := NewDrive(), NewDrive(), NewDrive()
	x.Apply(changes[0], changes[1])
	y.Apply(changes[2])
	z.Apply(changes[3], changes[4])
	if got := MergeDrives(x, y, z); got.Encode() != want.Encode() {
		t.Fatalf("merge != apply:\n got=%s\nwant=%s", got.Encode(), want.Encode())
	}
}

// TestDriveIsIdempotent asserts re-applying the whole set changes nothing.
func TestDriveIsIdempotent(t *testing.T) {
	changes, want := buildDriveChangeset(t)
	got := NewDrive()
	got.Apply(changes...)
	got.Apply(changes...)
	if got.Encode() != want.Encode() {
		t.Fatalf("not idempotent:\n got=%s\nwant=%s", got.Encode(), want.Encode())
	}
}

// TestDriveLinearSuccessorKeepsHistory: an edit that observed the current head
// makes the new blob current and retires the old one into version history.
func TestDriveLinearSuccessorKeepsHistory(t *testing.T) {
	_, d := buildDriveChangeset(t)
	e, ok := d.Get("Docs/tax.pdf")
	if !ok || e.Blob != "blobB" || e.Conflicted {
		t.Fatalf("want blobB current and not conflicted, got %+v ok=%v", e, ok)
	}
	if v := d.Versions("Docs/tax.pdf"); len(v) != 1 || v[0] != "blobA" {
		t.Fatalf("want [blobA] in history, got %v", v)
	}
}

// TestDriveConcurrentWriteConflicts: two writes to one path neither built on the
// other both survive as live heads, so the path reads back conflicted (ADR-0095 —
// the merge discards no bytes).
func TestDriveConcurrentWriteConflicts(t *testing.T) {
	_, d := buildDriveChangeset(t)
	e, ok := d.Get("photo.jpg")
	if !ok || !e.Conflicted {
		t.Fatalf("want photo.jpg conflicted, got %+v ok=%v", e, ok)
	}
}

// TestDriveDeleteTrashes: a deleted path leaves the live tree.
func TestDriveDeleteTrashes(t *testing.T) {
	_, d := buildDriveChangeset(t)
	if _, ok := d.Get("notes.txt"); ok {
		t.Fatal("notes.txt should be trashed")
	}
	for _, e := range d.List() {
		if e.Path == "notes.txt" {
			t.Fatal("trashed path must not appear in List")
		}
	}
}

// TestDriveSnapshotRoundTrips: a snapshot reconstructs a byte-identical drive.
func TestDriveSnapshotRoundTrips(t *testing.T) {
	_, d := buildDriveChangeset(t)
	snap, err := d.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	got, err := DriveFromSnapshot(snap)
	if err != nil {
		t.Fatalf("from snapshot: %v", err)
	}
	if got.Encode() != d.Encode() {
		t.Fatalf("snapshot lost state:\n got=%s\nwant=%s", got.Encode(), d.Encode())
	}
}

// TestNormalisePath pins the wire rule (ADR-0095): forward slashes, cleaned,
// leading slash trimmed, so two spellings of one path share a key.
func TestNormalisePath(t *testing.T) {
	cases := map[string]string{
		"/Docs/tax.pdf":       "Docs/tax.pdf",
		"Docs/./tax.pdf":      "Docs/tax.pdf",
		"Docs//tax.pdf":       "Docs/tax.pdf",
		"Docs/sub/../tax.pdf": "Docs/tax.pdf",
		`Docs\tax.pdf`:        "Docs/tax.pdf",
	}
	for in, want := range cases {
		if got := normalisePath(in); got != want {
			t.Errorf("normalisePath(%q) = %q, want %q", in, got, want)
		}
	}
	// Two spellings of one path address the same entry.
	d := NewDrive()
	d.Put("/Docs/tax.pdf", "blobA", 1, 1)
	if _, ok := d.Get("Docs/./tax.pdf"); !ok {
		t.Fatal("normalised spellings should address the same entry")
	}
}
