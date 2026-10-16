package statesync_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync"
)

// album_bridge_test.go is the Phase 3 (#756) end-to-end proof for the album kind,
// standing in for a `make demo` scene: the acceptance harness drives everything
// through the `heyarr` binary, which has no album verb (space put/read are
// playlist-only, vault put/ls are drive-only), so an album scene there would first
// need a product CLI surface this issue does not add. The same claim is made here
// against the real store: device A creates an album in a shared space and ships
// it as opaque changes through the peer's store; device B — holding only its
// wrapped space key — reads it back byte-identically, from the log and from a
// snapshot plus tail; and the peer's rows contain no album plaintext. The server
// side is untouched: the store keys opaque changes and snapshots by space and
// never learns which kind a space holds (Invariant 6).

func photoFixture(manifestPair, hashPair string) crdt.PhotoManifest {
	return crdt.PhotoManifest{
		Manifest: validBlob(manifestPair), Thumbnail: validBlob("e" + manifestPair[:1]),
		Sidecar: validBlob("f" + manifestPair[:1]), PlaintextHash: validBlob(hashPair),
		Width: 4000, Height: 3000, TakenAt: 1_700_000_000,
	}
}

func encodeResolved(t *testing.T, a *crdt.Albums) string {
	t.Helper()
	var b bytes.Buffer
	for _, ra := range a.Resolved() {
		b.WriteString(ra.AlbumID + "|" + ra.Title + "|" + ra.Caption + "|" + ra.Cover + "\n")
		for _, it := range ra.Items {
			b.WriteString("  " + it.Photo.Manifest + "\n")
		}
	}
	return b.String()
}

// TestAlbumCreatedOnOneDeviceReadsBackOnAnother is the scene: A creates an album
// with three photos, a caption and a cover in a shared space and pushes each write
// through the peer's store; B opens the space with its own key, pulls the stored
// changes, folds them, and sees the identical album.
func TestAlbumCreatedOnOneDeviceReadsBackOnAnother(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ra, _ := device(t)
	rb, ub := device(t)

	mgrA := client.New()
	sp, wrapped, err := mgrA.Create(spaces.KindShared, testNow, []client.Recipient{ra, rb})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutSpace(ctx, sp.ID, sp.Kind); err != nil {
		t.Fatal(err)
	}

	// Device A: every local write becomes one opaque change in the peer's store,
	// parented on the heads it saw.
	albumsA := crdt.NewAlbums()
	ship := func(c crdt.AlbumChange, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		existing, err := s.ChangesFor(ctx, sp.ID)
		if err != nil {
			t.Fatal(err)
		}
		ec, err := statesync.EncodeChange(mgrA, sp.ID, protocol.Heads(existing), c)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.PutChange(ctx, ec); err != nil {
			t.Fatal(err)
		}
	}
	const title, caption = "Secret Weekend", "the lake house"
	pA, pB, pC := photoFixture("a1", "1a"), photoFixture("b2", "2b"), photoFixture("c3", "3c")
	ship(albumsA.Create("alb-1", title))
	ship(albumsA.SetCaption("alb-1", caption))
	ship(albumsA.AddItem("alb-1", pA))
	ib, err := albumsA.AddItem("alb-1", pB)
	ship(ib, err)
	ship(albumsA.AddItem("alb-1", pC))
	ship(albumsA.SetCover("alb-1", pB.Manifest))
	ship(albumsA.MoveItem("alb-1", crdt.WriteKey{At: ib.At, Writer: ib.Writer}, crdt.AlbumPosStep/2))

	// THE INVARIANT: what the peer stores is ciphertext. No title, caption,
	// manifest id or plaintext hash appears in any stored row.
	stored, err := s.ChangesFor(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 7 {
		t.Fatalf("peer holds %d changes, want 7", len(stored))
	}
	for _, ec := range stored {
		for _, secret := range []string{title, caption, pA.Manifest, pB.PlaintextHash, "alb-1", "album_id"} {
			if bytes.Contains(ec.Ciphertext, []byte(secret)) {
				t.Fatalf("a stored change leaks %q in the clear", secret)
			}
		}
	}

	// Device B: opens with ITS wrapped key, decodes the opaque log, folds it.
	mgrB := client.New()
	if err := mgrB.Open(sp.ID, wrappedFor(t, wrapped, rb.ID), ub); err != nil {
		t.Fatalf("B.Open: %v", err)
	}
	decoded, err := statesync.DecodeAllChanges[crdt.AlbumChange](mgrB, stored)
	if err != nil {
		t.Fatalf("B.DecodeAllChanges: %v", err)
	}
	albumsB := crdt.NewAlbums()
	if rejected := albumsB.ApplyReport(decoded...); len(rejected) != 0 {
		t.Fatalf("B rejected %d of A's changes as malformed", len(rejected))
	}
	if albumsB.Encode() != albumsA.Encode() {
		t.Fatalf("the album did not converge across devices:\n--- A ---\n%s\n--- B ---\n%s", albumsA.Encode(), albumsB.Encode())
	}
	want := "alb-1|" + title + "|" + caption + "|" + pB.Manifest + "\n  " + pB.Manifest + "\n  " + pA.Manifest + "\n  " + pC.Manifest + "\n"
	if got := encodeResolved(t, albumsB); got != want {
		t.Fatalf("B's resolved album:\n%s\nwant:\n%s", got, want)
	}

	// §44: B snapshots at the current heads and stores it; a THIRD read reaches the
	// same album from the snapshot plus an empty tail after compaction.
	heads, err := s.HeadsFor(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := statesync.EncodeAlbumSnapshot(mgrB, sp.ID, heads, albumsB)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{title, pA.Manifest, pB.PlaintextHash, "albums"} {
		if bytes.Contains(snap.Ciphertext, []byte(secret)) {
			t.Fatalf("the snapshot leaks %q in the clear", secret)
		}
	}
	if err := s.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if dropped, err := s.CompactChanges(ctx, sp.ID, heads); err != nil || dropped != 7 {
		t.Fatalf("compaction dropped %d (err %v), want 7", dropped, err)
	}
	storedSnap, _, err := s.LatestSnapshotFor(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	fromSnap, err := statesync.DecodeAlbumSnapshot(mgrB, storedSnap)
	if err != nil {
		t.Fatal(err)
	}
	tail, err := s.ChangesFor(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	tailDecoded, err := statesync.DecodeAllChanges[crdt.AlbumChange](mgrB, tail)
	if err != nil {
		t.Fatal(err)
	}
	fromSnap.Apply(tailDecoded...)
	if fromSnap.Encode() != albumsA.Encode() {
		t.Fatalf("snapshot + tail reached a different album:\n%s", fromSnap.Encode())
	}

	// A device the space was NOT wrapped for cannot read it.
	_, uc := device(t)
	stranger := client.New()
	if err := stranger.Open(sp.ID, wrappedFor(t, wrapped, rb.ID), uc); err == nil {
		t.Fatal("a stranger opened the space with another device's wrapped key")
	}
}
