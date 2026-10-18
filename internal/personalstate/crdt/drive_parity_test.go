package crdt_test

// drive_parity_test.go mints the CROSS-LANGUAGE PARITY VECTORS for the drive CRDT
// (ADR-0095, ADR-0113, #774), on the discipline parity_test.go documents. Unlike
// the other kinds, a drive vector's `changes` are written as WIRE JSON rather than
// marshalled from structs: a drive space also carries other kinds' changes and
// legacy untagged ones (ADR-0113), and the vectors must pin how a fold reads those
// exact bytes. The Kotlin twin decodes `changes`, folds them in ANY order, and must
// reproduce `snapshot` (and `encode`, where it implements one) byte-for-byte.
//
// Regenerate with:  go test ./internal/personalstate/crdt -run TestParityVectors -update

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
)

// driveWire is a terse builder for one drive change's wire JSON.
func driveWire(kind string, op int, path, blob string, size, mtime int64, meta string, at uint64, writer string, baseAt uint64, baseBy string) string {
	var b strings.Builder
	b.WriteString("{")
	if kind != "" {
		fmt.Fprintf(&b, `"k":%q,`, kind)
	}
	fmt.Fprintf(&b, `"op":%d,"path":%q`, op, path)
	if blob != "" {
		fmt.Fprintf(&b, `,"blob":%q`, blob)
	}
	if size != 0 {
		fmt.Fprintf(&b, `,"size":%d`, size)
	}
	if mtime != 0 {
		fmt.Fprintf(&b, `,"mtime":%d`, mtime)
	}
	if meta != "" {
		b.WriteString("," + meta)
	}
	fmt.Fprintf(&b, `,"at":%d,"writer":%q,"base":{"At":%d,"Writer":%q}}`, at, writer, baseAt, baseBy)
	return b.String()
}

func driveVectors(t *testing.T) vectorFile {
	t.Helper()
	put := func(path, blob string, at uint64, writer string, baseAt uint64, baseBy string) string {
		return driveWire(crdt.KindDrive, 0, path, blob, 100, 1_700_000_000, "", at, writer, baseAt, baseBy)
	}
	putMeta := func(path, blob, meta string, at uint64, writer string, baseAt uint64, baseBy string) string {
		return driveWire(crdt.KindDrive, 0, path, blob, 100, 1_700_000_000, meta, at, writer, baseAt, baseBy)
	}
	del := func(path string, at uint64, writer string) string {
		return driveWire(crdt.KindDrive, 1, path, "", 0, 0, "", at, writer, 0, "")
	}
	photoMeta := fmt.Sprintf(`"mime":"image/jpeg","plaintext_hash":%q,"taken_at":1700000000,"width":4000,"height":3000,"thumbnail":%q,"sidecar":%q`,
		vecBlob("1a"), vecBlob("e1"), vecBlob("f1"))
	videoMeta := fmt.Sprintf(`"mime":"video/quicktime","width":1920,"height":1080,"duration_ms":61500,"thumbnail":%q`, vecBlob("e2"))

	scenarios := []struct {
		name    string
		changes []string
	}{
		{"empty", nil},
		{"put-then-overwrite-builds-on-the-head", []string{
			put("Docs/a.txt", vecBlob("a1"), 1, "w1", 0, ""),
			put("Docs/a.txt", vecBlob("b2"), 2, "w1", 1, "w1"),
		}},
		{"concurrent-puts-are-a-conflict", []string{
			put("Docs/a.txt", vecBlob("a1"), 1, "w1", 0, ""),
			put("Docs/a.txt", vecBlob("b2"), 2, "w1", 1, "w1"),
			put("Docs/a.txt", vecBlob("c3"), 2, "w2", 1, "w1"), // same base → two live heads
		}},
		{"delete-tombstones-the-path", []string{
			put("Docs/a.txt", vecBlob("a1"), 1, "w1", 0, ""),
			del("Docs/a.txt", 2, "w1"),
		}},
		{"nfc-paths-are-one-path", []string{
			put("Café/menu.pdf", vecBlob("a1"), 1, "w1", 0, ""),  // decomposed é
			put("Café/menu.pdf", vecBlob("b2"), 2, "w2", 1, "w1"), // composed é, built on it
		}},
		{"legacy-untagged-drive-changes-fold", []string{
			driveWire("", 0, "Old/x.bin", vecBlob("a1"), 7, 1_600_000_000, "", 1, "w1", 0, ""),
			driveWire("", 1, "Old/y.bin", "", 0, 0, "", 2, "w1", 0, ""),
		}},
		{"put-with-metadata", []string{
			putMeta("Photos/2026/10/IMG_1.jpg", vecBlob("a1"), photoMeta, 1, "w1", 0, ""),
			putMeta("Photos/2026/10/clip.mov", vecBlob("b2"), videoMeta, 2, "w1", 0, ""),
		}},
		{"metadata-only-rewrite-supersedes-without-a-version", []string{
			put("Photos/IMG_2.jpg", vecBlob("a1"), 1, "w1", 0, ""),
			putMeta("Photos/IMG_2.jpg", vecBlob("a1"), photoMeta, 2, "w9", 1, "w1"), // same blob, now with metadata
		}},
		{"other-kinds-in-the-space-are-skipped", []string{
			put("Docs/a.txt", vecBlob("a1"), 1, "w1", 0, ""),
			`{"k":"album","op":0,"album_id":"alb","title":"Trip","at":5,"writer":"w1","base":{"At":0,"Writer":""}}`,
			`{"op":1,"album_id":"alb","title":"Legacy title","at":6,"writer":"w1","base":{"At":0,"Writer":""}}`, // untagged album SET_TITLE: no path
			`{"k":"policy","device":"d1","at":7,"writer":"w1"}`,
			`{"k":"policy","op":"set","path":{"not":"a string"},"at":"later"}`, // another kind's schema, clashing types
		}},
		{"malformed-changes-are-skipped", []string{
			put("Docs/a.txt", vecBlob("a1"), 1, "w1", 0, ""),
			put("Docs/b.txt", "not-a-blob", 2, "w1", 0, ""),
			driveWire(crdt.KindDrive, 1, "Docs/a.txt", vecBlob("b2"), 0, 0, "", 3, "w1", 0, ""),         // delete carries a blob
			driveWire(crdt.KindDrive, 1, "Docs/a.txt", "", 0, 0, `"mime":"text/plain"`, 4, "w1", 0, ""), // delete carries metadata
			putMeta("Docs/c.txt", vecBlob("c3"), `"thumbnail":"blake3:short"`, 5, "w1", 0, ""),          // malformed thumbnail id
			putMeta("Docs/d.txt", vecBlob("d4"), `"taken_at":-1`, 6, "w1", 0, ""),                       // negative capture time
			driveWire(crdt.KindDrive, 0, "/", vecBlob("e5"), 1, 1, "", 7, "w1", 0, ""),                  // empty once normalised
			driveWire(crdt.KindDrive, 0, "Docs/e.txt", vecBlob("e5"), 1, 1, "", 8, "", 0, ""),           // no writer
			driveWire(crdt.KindDrive, 9, "Docs/f.txt", "", 0, 0, "", 50, "w1", 0, ""),                   // unknown op
		}},
		{"counter-poison-saturates-at-maxuint64", []string{
			put("Docs/a.txt", vecBlob("a1"), maxU64, "w1", 0, ""),
		}},
	}
	vf := vectorFile{Kind: "drive", Note: vectorNote}
	for _, sc := range scenarios {
		raw := make([]json.RawMessage, 0, len(sc.changes))
		for _, c := range sc.changes {
			if !json.Valid([]byte(c)) {
				t.Fatalf("%s: invalid wire JSON %s", sc.name, c)
			}
			raw = append(raw, json.RawMessage(c))
		}
		enc, snap := foldDrive(t, wireVector{Changes: raw})
		vf.Vectors = append(vf.Vectors, wireVector{Name: sc.name, Changes: raw, Encode: enc, Snapshot: snap})
	}
	return vf
}

func foldDrive(t *testing.T, v wireVector) (string, string) {
	t.Helper()
	d := crdt.NewDrive()
	for _, r := range v.Changes {
		var c crdt.DriveChange
		mustUnmarshal(t, r, &c)
		d.Apply(c)
	}
	return d.Encode(), mustSnap(t, d.Snapshot)
}

// TestDriveParityFoldIsOrderFree proves, against the committed vectors, that the
// drive fold — other kinds and malformed changes included — is order-independent.
func TestDriveParityFoldIsOrderFree(t *testing.T) {
	vf := loadVectorFile(t, "drive.json")
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
				enc, snap := foldDrive(t, wireVector{Changes: p})
				if enc != v.Encode || snap != v.Snapshot {
					t.Fatalf("permutation %d of %q diverged from the vector:\n--- want ---\n%s\n--- got ---\n%s", i, v.Name, v.Encode, enc)
				}
			}
		})
	}
}

// TestDriveVectorsPinTheADR0113Reads spot-checks the vectors' meaning, so a
// regenerated file that silently changed behaviour fails here rather than only
// in a port.
func TestDriveVectorsPinTheADR0113Reads(t *testing.T) {
	vf := loadVectorFile(t, "drive.json")
	byName := map[string]wireVector{}
	for _, v := range vf.Vectors {
		byName[v.Name] = v
	}
	fold := func(name string) *crdt.Drive {
		t.Helper()
		v, ok := byName[name]
		if !ok {
			t.Fatalf("vector %q missing", name)
		}
		d, err := crdt.DriveFromSnapshot([]byte(v.Snapshot))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	if e, ok := fold("put-with-metadata").Get("Photos/2026/10/clip.mov"); !ok || e.Meta.Mime != "video/quicktime" || e.Meta.DurationMS != 61500 {
		t.Fatalf("metadata did not survive the snapshot: %+v", e)
	}
	d := fold("metadata-only-rewrite-supersedes-without-a-version")
	if e, ok := d.Get("Photos/IMG_2.jpg"); !ok || e.Conflicted || e.Meta.Thumbnail == "" {
		t.Fatalf("a metadata rewrite must supersede cleanly: %+v", e)
	}
	if v := d.Versions("Photos/IMG_2.jpg"); len(v) != 0 {
		t.Fatalf("a metadata rewrite is not a content version, got %v", v)
	}
	if got := fold("other-kinds-in-the-space-are-skipped").List(); len(got) != 1 || got[0].Path != "Docs/a.txt" {
		t.Fatalf("other kinds must not touch the drive: %+v", got)
	}
	if got := fold("malformed-changes-are-skipped").List(); len(got) != 1 || got[0].Path != "Docs/a.txt" {
		t.Fatalf("only the valid put survives: %+v", got)
	}
}
