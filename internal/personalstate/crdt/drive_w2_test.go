package crdt

import (
	"math/rand"
	"testing"
	"time"
)

// --- Task 1: blob-id validation ---------------------------------------------

// TestDrivePutRejectsMalformedBlob: a local write of a blob that is not a
// canonical "blake3:<64 hex>" id fails at the source and writes nothing.
func TestDrivePutRejectsMalformedBlob(t *testing.T) {
	d := NewDrive()
	for _, bad := range []string{"", "blobA", "sha256:" + hexBlob("aa")[7:], "blake3:xyz", "blake3:" + "AB"} {
		if _, err := d.Put("f.txt", bad, 1, 1); err == nil {
			t.Fatalf("Put accepted malformed blob %q", bad)
		}
	}
	if _, ok := d.Get("f.txt"); ok {
		t.Fatal("a rejected Put must not enter the drive")
	}
	// A well-formed id is accepted.
	if _, err := d.Put("f.txt", blobA, 1, 1); err != nil {
		t.Fatalf("Put rejected a valid blob: %v", err)
	}
}

// TestDriveApplySkipsMalformedChanges: malformed changes from a peer are dropped,
// not applied, and the drop is deterministic so convergence is preserved.
func TestDriveApplySkipsMalformedChanges(t *testing.T) {
	valid, _ := NewDrive().Put("keep.txt", blobA, 1, 1)
	bad := []DriveChange{
		{Op: OpPut, Path: "evil.txt", Blob: "not-a-hash", At: 5, Writer: "w"},
		{Op: OpPut, Path: "empty.txt", Blob: "", At: 6, Writer: "w"},
		{Op: OpDelete, Path: "d.txt", Blob: blobA, At: 7, Writer: "w"}, // delete carries a blob
		{Op: OpPut, Path: "notag.txt", Blob: blobB, At: 8, Writer: ""}, // no writer tag
		{Op: OpKind(9), Path: "u.txt", At: 9, Writer: "w"},             // unknown op
	}

	d := NewDrive()
	rejected := d.ApplyReport(append([]DriveChange{valid}, bad...)...)
	if len(rejected) != len(bad) {
		t.Fatalf("want %d rejected, got %d", len(bad), len(rejected))
	}
	if got := d.List(); len(got) != 1 || got[0].Path != "keep.txt" {
		t.Fatalf("only the valid change should survive, got %v", got)
	}

	// Convergence holds: any interleaving of the valid + malformed changes yields
	// the identical drive, because rejection is a pure function of the bytes.
	all := append([]DriveChange{valid}, bad...)
	src := rand.New(rand.NewSource(7))
	for i := 0; i < 50; i++ {
		got := NewDrive()
		perm := make([]DriveChange, len(all))
		copy(perm, all)
		src.Shuffle(len(perm), func(a, b int) { perm[a], perm[b] = perm[b], perm[a] })
		got.Apply(perm...)
		if got.Encode() != d.Encode() {
			t.Fatalf("order %d diverged with malformed changes in the mix", i)
		}
	}
}

// --- Task 2: Unicode NFC in normalisePath -----------------------------------

// TestDriveNFCPathsConverge: a composed and a decomposed spelling of one accented
// path are the SAME file, so a write to one is visible via the other and two
// writers using the two spellings do not fork the file into a spurious conflict.
func TestDriveNFCPathsConverge(t *testing.T) {
	const composed = "Docs/café.txt"    // é as one code point (NFC)
	const decomposed = "Docs/café.txt" // e + U+0301 combining acute (NFD)

	if normalisePath(composed) != normalisePath(decomposed) {
		t.Fatalf("NFC did not unify the spellings: %q vs %q", normalisePath(composed), normalisePath(decomposed))
	}

	// Written via one spelling, read via the other.
	d := NewDrive()
	mustPut(t, d, composed, blobA, 1, 1)
	if _, ok := d.Get(decomposed); !ok {
		t.Fatal("a decomposed read should find the composed write")
	}

	// A second write via the OTHER spelling observes the first head -> clean
	// successor, one live entry, no conflict.
	d2 := NewDrive()
	mustPut(t, d2, composed, blobA, 1, 1)
	mustPut(t, d2, decomposed, blobB, 2, 2)
	e, ok := d2.Get(composed)
	if !ok || e.Conflicted || e.Blob != blobB {
		t.Fatalf("want a single blobB head, got %+v ok=%v", e, ok)
	}
	if got := d2.List(); len(got) != 1 {
		t.Fatalf("the two spellings must be one file, got %d entries", len(got))
	}
}

// --- Task 4: conflicted-copy relocation -------------------------------------

// encodeEntries is a canonical string of a resolved tree for byte comparison.
func encodeEntries(entries []DriveEntry) string {
	var b []byte
	for _, e := range entries {
		b = append(b, []byte(e.Path)...)
		b = append(b, '\x00')
		b = append(b, []byte(e.Blob)...)
		b = append(b, '\n')
	}
	return string(b)
}

// threeWayConflict builds three offline writes to one fresh path (all Base zero) —
// a genuine multi-way conflict — returning the changes and the greatest-key blob.
func threeWayConflict(t *testing.T) (changes []DriveChange, winnerBlob string) {
	t.Helper()
	x, _ := NewDrive().Put("Photos/trip.jpg", blobX, 30, 3)
	y, _ := NewDrive().Put("Photos/trip.jpg", blobY, 40, 4)
	z, _ := NewDrive().Put("Photos/trip.jpg", blobA, 50, 5)
	changes = []DriveChange{x, y, z}
	// The winner is the greatest (At, Writer); compute it the same way the drive does.
	best := x
	for _, c := range []DriveChange{y, z} {
		if (posKey{At: c.At, Writer: c.Writer}).greater(posKey{At: best.At, Writer: best.Writer}) {
			best = c
		}
	}
	return changes, best.Blob
}

// TestDriveResolvedConvergesUnderReordering is the W2 headline proof: two replicas
// that apply the SAME multi-way concurrent writes in different orders resolve to a
// byte-identical tree — winner at the path, every loser at the identical derived
// path — and no blob is lost.
func TestDriveResolvedConvergesUnderReordering(t *testing.T) {
	changes, winnerBlob := threeWayConflict(t)

	ref := NewDrive()
	ref.Apply(changes...)
	want := encodeEntries(ref.Resolved())

	// The path keeps the winner; the two losers are relocated.
	byPath := map[string]DriveEntry{}
	blobs := map[string]bool{}
	for _, e := range ref.Resolved() {
		byPath[e.Path] = e
		blobs[e.Blob] = true
		if e.Conflicted {
			t.Fatalf("a resolved entry must not stay flagged conflicted: %+v", e)
		}
	}
	if got := byPath["Photos/trip.jpg"]; got.Blob != winnerBlob {
		t.Fatalf("winner should hold the path, got %q want %q", got.Blob, winnerBlob)
	}
	if len(byPath) != 3 {
		t.Fatalf("want 3 files after relocation (1 winner + 2 copies), got %d", len(byPath))
	}
	for _, want := range []string{blobX, blobY, blobA} {
		if !blobs[want] {
			t.Fatalf("relocation lost blob %q", want)
		}
	}

	src := rand.New(rand.NewSource(3))
	for i := 0; i < 200; i++ {
		got := NewDrive()
		perm := make([]DriveChange, len(changes))
		copy(perm, changes)
		src.Shuffle(len(perm), func(a, b int) { perm[a], perm[b] = perm[b], perm[a] })
		got.Apply(perm...)
		if encodeEntries(got.Resolved()) != want {
			t.Fatalf("order %d resolved to a different tree:\n got=%q\nwant=%q", i, encodeEntries(got.Resolved()), want)
		}
	}
}

// TestDriveResolvedGuardsReCollision: if a loser's natural derived path is already
// occupied by a real live file, relocation deterministically picks the next
// numbered variant and never overwrites the real file.
func TestDriveResolvedGuardsReCollision(t *testing.T) {
	changes, _ := threeWayConflict(t)
	d := NewDrive()
	d.Apply(changes...)

	// Find a loser and compute the path it WOULD take, then squat on it.
	rec := d.entries["Photos/trip.jpg"]
	live := rec.liveHeads() // ascending; losers are all but the last
	loser := live[0]
	squat := conflictPath("Photos/trip.jpg", loser.key, 1)
	mustPut(t, d, squat, blobB, 99, 99)

	resolved := d.Resolved()
	var atSquat, atNumbered int
	numbered := conflictPath("Photos/trip.jpg", loser.key, 2)
	for _, e := range resolved {
		if e.Path == squat {
			atSquat++
			if e.Blob != blobB {
				t.Fatalf("the real file at %q was overwritten (blob %q)", squat, e.Blob)
			}
		}
		if e.Path == numbered {
			atNumbered++
		}
	}
	if atSquat != 1 {
		t.Fatalf("the squatting file must survive exactly once at %q, saw %d", squat, atSquat)
	}
	if atNumbered != 1 {
		t.Fatalf("the displaced loser must relocate to %q, saw %d", numbered, atNumbered)
	}
}

// --- Task 5: retention / GC reference view ----------------------------------

// TestDriveRetentionCapsVersions: keeping N prior versions reports the older ones
// as unreferenced, never the live head.
func TestDriveRetentionCapsVersions(t *testing.T) {
	d := NewDrive()
	blobC := hexBlob("e5")
	mustPut(t, d, "log.txt", blobA, 1, 1)
	mustPut(t, d, "log.txt", blobB, 2, 2)
	mustPut(t, d, "log.txt", blobC, 3, 3) // live head

	rep := d.Retain(RetentionPolicy{MaxVersionsPerPath: 1, TrashTTL: -1}, time.Unix(1000, 0))
	// Keep blobC (live) + blobB (1 version); drop blobA.
	if len(rep.Unreferenced) != 1 || rep.Unreferenced[0] != blobA {
		t.Fatalf("want [blobA] unreferenced, got %v", rep.Unreferenced)
	}

	// Keeping zero versions drops all history but never the live head.
	rep0 := d.Retain(RetentionPolicy{MaxVersionsPerPath: 0, TrashTTL: -1}, time.Unix(1000, 0))
	if len(rep0.Unreferenced) != 2 {
		t.Fatalf("want blobA+blobB unreferenced, got %v", rep0.Unreferenced)
	}
	for _, u := range rep0.Unreferenced {
		if u == blobC {
			t.Fatal("retention reported the live blob as unreferenced")
		}
	}

	// Unlimited history reports nothing.
	if rep := d.Retain(RetentionPolicy{MaxVersionsPerPath: -1, TrashTTL: -1}, time.Unix(1000, 0)); len(rep.Unreferenced) != 0 {
		t.Fatalf("unlimited history should report nothing, got %v", rep.Unreferenced)
	}
}

// TestDriveRetentionTrashTTL: a deleted path's blob is reclaimable only once the
// trash is older than the TTL.
func TestDriveRetentionTrashTTL(t *testing.T) {
	d := NewDrive()
	mustPut(t, d, "old.txt", blobA, 1, 100) // content mtime = 100
	d.Delete("old.txt")

	// now = 100 + 50; TTL = 100s -> trash still young -> kept.
	if rep := d.Retain(RetentionPolicy{MaxVersionsPerPath: -1, TrashTTL: 100 * time.Second}, time.Unix(150, 0)); len(rep.Unreferenced) != 0 {
		t.Fatalf("young trash should be kept, got %v", rep.Unreferenced)
	}
	// now = 100 + 500; TTL = 100s -> trash expired -> reclaimable.
	rep := d.Retain(RetentionPolicy{MaxVersionsPerPath: -1, TrashTTL: 100 * time.Second}, time.Unix(600, 0))
	if len(rep.Unreferenced) != 1 || rep.Unreferenced[0] != blobA {
		t.Fatalf("expired trash should report blobA, got %v", rep.Unreferenced)
	}
	// Negative TTL keeps trash forever.
	if rep := d.Retain(RetentionPolicy{MaxVersionsPerPath: -1, TrashTTL: -1}, time.Unix(1<<40, 0)); len(rep.Unreferenced) != 0 {
		t.Fatalf("negative TTL keeps trash forever, got %v", rep.Unreferenced)
	}
}

// TestDriveRetentionKeepsBlobReferencedElsewhere: a blob dropped as a version at
// one path is NOT reported if it is a live head at another path.
func TestDriveRetentionKeepsBlobReferencedElsewhere(t *testing.T) {
	d := NewDrive()
	mustPut(t, d, "a.txt", blobA, 1, 1)
	mustPut(t, d, "a.txt", blobB, 2, 2) // blobA becomes a version at a.txt
	mustPut(t, d, "b.txt", blobA, 3, 3) // blobA is the LIVE head at b.txt

	rep := d.Retain(RetentionPolicy{MaxVersionsPerPath: 0, TrashTTL: -1}, time.Unix(1000, 0))
	for _, u := range rep.Unreferenced {
		if u == blobA {
			t.Fatal("blobA is live at b.txt and must not be reported unreferenced")
		}
	}
}
