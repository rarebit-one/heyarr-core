package crdt_test

// album_parity_test.go mints the CROSS-LANGUAGE PARITY VECTORS for the album CRDT
// (#756), on the same discipline parity_test.go documents for the other kinds: the
// exact json.Marshal(change) wire bytes in, the deterministic Albums.Encode() and
// Snapshot() out. The Kotlin twin decodes `changes`, folds them in ANY order, and
// must reproduce `encode` and `snapshot` byte-for-byte.
//
// Regenerate with:  go test ./internal/personalstate/crdt -run TestParityVectors -update

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
)

func vecBlob(pair string) string { return "blake3:" + strings.Repeat(pair, 32) }

func vecPhoto(manifestPair, hashPair string) *crdt.PhotoManifest {
	return &crdt.PhotoManifest{
		Manifest: vecBlob(manifestPair), Thumbnail: vecBlob("e" + manifestPair[:1]),
		Sidecar: vecBlob("f" + manifestPair[:1]), PlaintextHash: vecBlob(hashPair),
		Width: 4000, Height: 3000, TakenAt: 1_700_000_000,
	}
}

func albumVectors(t *testing.T) vectorFile {
	t.Helper()
	const step = crdt.AlbumPosStep
	w := func(at uint64, writer string) (uint64, crdt.PosTag) { return at, crdt.PosTag(writer) }
	ch := func(op crdt.AlbumOp, album string, at uint64, writer string) crdt.AlbumChange {
		a, wr := w(at, writer)
		return crdt.AlbumChange{Op: op, AlbumID: album, At: a, Writer: wr}
	}
	create := func(album, title string, at uint64, writer string) crdt.AlbumChange {
		c := ch(crdt.AlbumOpCreate, album, at, writer)
		c.Title = title
		return c
	}
	add := func(album string, p *crdt.PhotoManifest, pos, at uint64, writer string) crdt.AlbumChange {
		c := ch(crdt.AlbumOpAddItem, album, at, writer)
		c.Item, c.Pos = p, pos
		return c
	}
	remove := func(album string, itemAt uint64, itemBy string, at uint64, writer string) crdt.AlbumChange {
		c := ch(crdt.AlbumOpRemoveItem, album, at, writer)
		c.Base = crdt.WriteKey{At: itemAt, Writer: crdt.PosTag(itemBy)}
		return c
	}
	move := func(album string, itemAt uint64, itemBy string, pos, at uint64, writer string) crdt.AlbumChange {
		c := ch(crdt.AlbumOpMoveItem, album, at, writer)
		c.Base, c.Pos = crdt.WriteKey{At: itemAt, Writer: crdt.PosTag(itemBy)}, pos
		return c
	}
	title := func(album, v string, at uint64, writer string) crdt.AlbumChange {
		c := ch(crdt.AlbumOpSetTitle, album, at, writer)
		c.Title = v
		return c
	}
	caption := func(album, v string, at uint64, writer string) crdt.AlbumChange {
		c := ch(crdt.AlbumOpSetCaption, album, at, writer)
		c.Caption = v
		return c
	}
	cover := func(album, manifest string, at uint64, writer string) crdt.AlbumChange {
		c := ch(crdt.AlbumOpSetCover, album, at, writer)
		c.Cover = manifest
		return c
	}
	pA, pB, pC := vecPhoto("a1", "1a"), vecPhoto("b2", "2b"), vecPhoto("c3", "3c")
	pDup := vecPhoto("d4", "1a") // photoA's plaintext under a second manifest

	scenarios := []struct {
		name    string
		changes []crdt.AlbumChange
	}{
		{"empty", nil},
		{"create-then-add-in-order", []crdt.AlbumChange{
			create("alb", "Trip", 1, "w1"),
			add("alb", pA, step, 2, "w1"), add("alb", pB, 2*step, 3, "w1"), add("alb", pC, 3*step, 4, "w1"),
		}},
		{"concurrent-adds-tie-by-writer", []crdt.AlbumChange{
			create("alb", "", 1, "w1"),
			add("alb", pB, step, 2, "w2"), add("alb", pA, step, 2, "w1"), // same pos, same At → w1 before w2
		}},
		{"add-wins-over-concurrent-remove", []crdt.AlbumChange{
			create("alb", "", 1, "w1"),
			add("alb", pA, step, 2, "w1"),
			remove("alb", 2, "w1", 3, "w1"), // observed the first add only
			add("alb", pA, step, 3, "w2"),   // concurrent re-add → survives
		}},
		{"remove-tombstones-observed-add", []crdt.AlbumChange{
			create("alb", "", 1, "w1"), add("alb", pA, step, 2, "w1"), remove("alb", 2, "w1", 3, "w1"),
		}},
		{"move-to-front", []crdt.AlbumChange{
			create("alb", "", 1, "w1"),
			add("alb", pA, step, 2, "w1"), add("alb", pB, 2*step, 3, "w1"), add("alb", pC, 3*step, 4, "w1"),
			move("alb", 4, "w1", step/2, 5, "w1"),
		}},
		{"move-to-position-zero-omits-pos", []crdt.AlbumChange{
			create("alb", "", 1, "w1"),
			add("alb", pA, step, 2, "w1"), add("alb", pB, 2*step, 3, "w1"),
			move("alb", 3, "w1", 0, 4, "w1"), // pos 0 is omitted on the wire and still means 0
		}},
		{"concurrent-moves-greater-key-wins", []crdt.AlbumChange{
			create("alb", "", 1, "w1"),
			add("alb", pA, step, 2, "w1"), add("alb", pB, 2*step, 3, "w1"),
			move("alb", 3, "w1", 1, 4, "w1"), move("alb", 3, "w1", 5*step, 4, "w2"), // w2 > w1 → b stays last
		}},
		{"stale-move-before-add-is-ignored", []crdt.AlbumChange{
			create("alb", "", 1, "w1"),
			move("alb", 3, "w1", 1, 2, "w1"), // targets an add with a greater key
			add("alb", pA, step, 3, "w1"),
		}},
		{"cover-set", []crdt.AlbumChange{
			create("alb", "", 1, "w1"),
			add("alb", pA, step, 2, "w1"), add("alb", pB, 2*step, 3, "w1"),
			cover("alb", pB.Manifest, 4, "w1"),
		}},
		{"cover-falls-back-to-first-item", []crdt.AlbumChange{
			create("alb", "", 1, "w1"),
			add("alb", pA, step, 2, "w1"), add("alb", pB, 2*step, 3, "w1"),
			cover("alb", pA.Manifest, 4, "w1"),
			remove("alb", 2, "w1", 5, "w1"), // the cover photo leaves → first item (b)
		}},
		{"title-and-caption-lww-tie-by-writer", []crdt.AlbumChange{
			create("alb", "Draft", 1, "w1"),
			title("alb", "From w2", 2, "w2"), title("alb", "From w1", 2, "w1"),
			caption("alb", "k1", 3, "w1"), caption("alb", "k2", 2, "w9"), // At 3 beats At 2 whatever the writer
		}},
		{"delete-album", []crdt.AlbumChange{
			create("alb", "Gone", 1, "w1"), add("alb", pA, step, 2, "w1"), ch(crdt.AlbumOpDelete, "alb", 3, "w1"),
		}},
		{"recreate-after-delete-starts-clean", []crdt.AlbumChange{
			create("alb", "Old", 1, "w1"), add("alb", pA, step, 2, "w1"), ch(crdt.AlbumOpDelete, "alb", 3, "w1"),
			create("alb", "", 4, "w1"), add("alb", pB, step, 5, "w1"),
		}},
		{"add-racing-delete-survives", []crdt.AlbumChange{
			create("alb", "", 1, "w1"), ch(crdt.AlbumOpDelete, "alb", 2, "w1"),
			add("alb", pA, step, 2, "w2"), // same At, greater writer → the delete does not cover it
			create("alb", "", 2, "w3"),    // a re-create at the same At with a greater writer beats the delete → live, with a
		}},
		{"dedupe-by-plaintext-hash", []crdt.AlbumChange{
			create("alb", "", 1, "w1"),
			add("alb", pA, step, 2, "w1"), add("alb", pDup, 2*step, 3, "w2"), add("alb", pB, 3*step, 4, "w1"),
		}},
		{"two-albums-sorted", []crdt.AlbumChange{
			create("zeta", "Z", 1, "w1"), create("alpha", "A", 2, "w1"), add("zeta", pC, step, 3, "w1"),
		}},
		{"malformed-changes-are-skipped", []crdt.AlbumChange{
			create("alb", "", 1, "w1"),
			add("alb", pA, step, 2, "w1"),
			{Op: crdt.AlbumOpAddItem, AlbumID: "alb", At: 3, Writer: "w1"}, // no item
			{Op: crdt.AlbumOpAddItem, AlbumID: "alb", Item: &crdt.PhotoManifest{Manifest: "x", PlaintextHash: "y"}, At: 4, Writer: "w1"},
			{Op: crdt.AlbumOpRemoveItem, AlbumID: "alb", At: 5, Writer: "w1"},         // no base
			{Op: crdt.AlbumOpDelete, AlbumID: "alb", Title: "x", At: 6, Writer: "w1"}, // delete carries a title
			{Op: crdt.AlbumOp(200), AlbumID: "alb", At: 50, Writer: "w1"},             // unknown op
			{Op: crdt.AlbumOpCreate, AlbumID: "", At: 60, Writer: "w1"},               // no album id
			{Op: crdt.AlbumOpCreate, AlbumID: "alb", At: 70},                          // no writer
		}},
		{"counter-poison-saturates-at-maxuint64", []crdt.AlbumChange{
			create("alb", "", 1, "w1"), add("alb", pA, maxU64, maxU64, "w1"),
		}},
	}
	vf := vectorFile{Kind: "album", Note: vectorNote}
	for _, sc := range scenarios {
		a := crdt.NewAlbums()
		raw := make([]json.RawMessage, 0, len(sc.changes))
		for _, c := range sc.changes {
			raw = append(raw, mustMarshal(t, c))
		}
		a.Apply(sc.changes...)
		vf.Vectors = append(vf.Vectors, wireVector{
			Name: sc.name, Changes: raw, Encode: a.Encode(), Snapshot: mustSnap(t, a.Snapshot),
		})
	}
	return vf
}

func foldAlbum(t *testing.T, v wireVector) (string, string) {
	t.Helper()
	a := crdt.NewAlbums()
	for _, r := range v.Changes {
		var c crdt.AlbumChange
		mustUnmarshal(t, r, &c)
		a.Apply(c)
	}
	return a.Encode(), mustSnap(t, a.Snapshot)
}

// TestAlbumParityFoldIsOrderFree proves, against the committed vectors, that the
// fold is order-independent: every vector's changes reversed, and in 20 random
// permutations, reproduce the same `encode` and `snapshot` — the property the
// Kotlin twin must hold too, so a port may shuffle the same file to prove itself.
func TestAlbumParityFoldIsOrderFree(t *testing.T) {
	vf := loadVectorFile(t, "album.json")
	for _, v := range vf.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			perms := [][]json.RawMessage{reversed(v.Changes)}
			src := rand.New(rand.NewSource(int64(len(v.Name)))) //nolint:gosec // deterministic test shuffle
			for i := 0; i < 20; i++ {
				p := append([]json.RawMessage(nil), v.Changes...)
				src.Shuffle(len(p), func(a, b int) { p[a], p[b] = p[b], p[a] })
				perms = append(perms, p)
			}
			for i, p := range perms {
				enc, snap := foldAlbum(t, wireVector{Changes: p})
				if enc != v.Encode || snap != v.Snapshot {
					t.Fatalf("permutation %d of %q diverged from the vector:\n--- want ---\n%s\n--- got ---\n%s", i, v.Name, v.Encode, enc)
				}
			}
		})
	}
}

func reversed(in []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, len(in))
	for i, r := range in {
		out[len(in)-1-i] = r
	}
	return out
}
