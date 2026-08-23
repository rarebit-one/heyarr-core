package catalog_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/chunking"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// Chunk manifests, the local chunk index, and §16's three states (M5-03,
// ADR-0034).
//
// The assertions here are arranged around one rule: asking whether a blob has
// a manifest must never generate one. Everything else in this file is either
// that rule, the ordering property ADR-0034 says the whole-object hash is
// otherwise alone in catching, or a cascade asserted rather than inherited.

// fixedNow is the harness clock's instant, so a manifest's GeneratedAt is a
// value rather than a wall-clock read (ADR-0017).
var fixedNow = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

// blobHash builds a distinct, valid blob identity from one character.
func blobHash(t *testing.T, c string) hashing.Hash {
	t.Helper()
	h, err := hashing.Parse("blake3:" + strings.Repeat(c, 64))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// seedBlob records a blob of the given size with no manifest and no decision.
func (h *harness) seedBlob(t *testing.T, hash hashing.Hash, size int64) {
	t.Helper()
	h.exec(t, `INSERT INTO blobs (hash, size, first_seen_at) VALUES (?, ?, ?)`,
		hash.String(), size, stamp)
}

// chunkSeq builds a contiguous chunk sequence whose digests are distinguishable
// by index, so an assertion on the ORDER has something to fail on. A sequence
// of identical chunks would round-trip through any permutation.
func chunkSeq(t *testing.T, lengths ...int64) []chunking.Chunk {
	t.Helper()
	var out []chunking.Chunk
	var off int64
	for i, n := range lengths {
		// Distinct per index, and never the all-zero digest — a zero digest is
		// "no digest", which the manifest refuses for its own reasons.
		d, err := hashing.Parse(fmt.Sprintf("blake3:%064x", i+1))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, chunking.Chunk{Offset: off, Length: n, Digest: d})
		off += n
	}
	return out
}

func mustBuild(t *testing.T, blob hashing.Hash, chunks []chunking.Chunk) manifests.Manifest {
	t.Helper()
	m, err := manifests.Build(blob, chunking.DefaultConfig(), chunks, fixedNow)
	if err != nil {
		t.Fatalf("building a manifest: %v", err)
	}
	return m
}

// digests is the chunk digest SEQUENCE, as a comparable value.
func digests(m manifests.Manifest) []string {
	out := make([]string, 0, len(m.Chunks))
	for _, c := range m.Chunks {
		out = append(out, c.Digest.String())
	}
	return out
}

// jobCount is every row in the job table, and manifestJobs is the subset that
// would chunk something. Both, because "no chunk_blob job" is satisfied
// vacuously by a handler that enqueued a differently-named job instead.
func (h *harness) jobCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.db.Reader().QueryRow(`SELECT count(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (h *harness) manifestRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.db.Reader().QueryRow(`SELECT count(*) FROM chunk_manifests`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (h *harness) chunkRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.db.Reader().QueryRow(`SELECT count(*) FROM manifest_chunks`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (h *harness) localChunkRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.db.Reader().QueryRow(`SELECT count(*) FROM local_chunks`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// THE round-trip, and it is asserted on the SEQUENCE.
//
// ADR-0034: "a set of individually valid chunks assembled in the wrong order
// is a set of valid chunks and the wrong file." A test that compared the chunk
// digests as a set would pass on a read that dropped ORDER BY idx, and the
// only thing that would then notice is the whole-object hash at the far end of
// a transfer that has already happened.
func TestAManifestRoundTripsInOrder(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	blob := blobHash(t, "a")
	chunks := chunkSeq(t, 300, 100, 700, 250, 650)
	var size int64
	for _, c := range chunks {
		size += c.Length
	}
	h.seedBlob(t, blob, size)

	want := mustBuild(t, blob, chunks)
	if err := h.cat.SaveChunkManifest(ctx, want); err != nil {
		t.Fatal(err)
	}

	got, found, err := h.cat.ChunkManifest(ctx, blob)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("the manifest just written was not found")
	}

	// The sequence, compared as a sequence.
	gotSeq, wantSeq := digests(got), digests(want)
	if len(gotSeq) != len(wantSeq) {
		t.Fatalf("read back %d chunks, wrote %d", len(gotSeq), len(wantSeq))
	}
	for i := range wantSeq {
		if gotSeq[i] != wantSeq[i] {
			t.Fatalf("chunk %d came back as %s, want %s — the ORDER is the data, and a "+
				"reassembly from this sequence is a valid-chunk, wrong-file blob (ADR-0034)",
				i, gotSeq[i], wantSeq[i])
		}
	}

	// Offsets contiguous from zero, summing to the blob's size.
	var expected int64
	for i, c := range got.Chunks {
		if c.Offset != expected {
			t.Fatalf("chunk %d starts at %d, the previous one ends at %d", i, c.Offset, expected)
		}
		expected = c.End()
	}
	if expected != size {
		t.Errorf("the chunks cover %d bytes, the blob is %d", expected, size)
	}
	if !got.Covers(size) {
		t.Errorf("CoveredSize = %d, blob size = %d", got.CoveredSize, size)
	}

	// And the parameters came back, because a manifest computed under other
	// settings is not comparable with one that was not.
	if got.Params != chunking.DefaultConfig() {
		t.Errorf("params = %+v, want %+v", got.Params, chunking.DefaultConfig())
	}
	if got.Algorithm != manifests.AlgorithmFastCDC {
		t.Errorf("algorithm = %q", got.Algorithm)
	}
	if !got.Digest.Equal(want.Digest) {
		t.Errorf("manifest digest = %s, want %s", got.Digest, want.Digest)
	}
}

// All three states, on one fixture, in one read.
//
// Equality on the state value, never containment: "not_required" contains
// neither of the others and none of them contains another, but the discipline
// is the point — an assert_contains on "not_satisfied" matching "satisfied"
// shipped in this repo once.
func TestTheThreeStatesAreDistinguishableInOneRead(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	manifested := blobHash(t, "b")
	exempt := blobHash(t, "c")
	undecided := blobHash(t, "d")
	h.seedBlob(t, manifested, 1000)
	h.seedBlob(t, exempt, 12)
	h.seedBlob(t, undecided, 5000)

	if err := h.cat.SaveChunkManifest(ctx, mustBuild(t, manifested, chunkSeq(t, 600, 400))); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.RecordChunkingNotRequired(ctx, exempt, "smaller than one chunk"); err != nil {
		t.Fatal(err)
	}
	// undecided gets nothing. That is the state.

	// ONE read.
	states, err := h.cat.ChunkManifestStates(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		hash hashing.Hash
		want manifests.State
	}{
		{"a blob with a manifest", manifested, manifests.StatePresent},
		{"a blob recorded as never needing one", exempt, manifests.StateNotRequired},
		{"a blob nobody has decided about", undecided, manifests.StateUndecided},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := states[tc.hash.String()]; got != tc.want {
				t.Errorf("state = %q, want %q", got, tc.want)
			}
			// And the single-blob read agrees with the listing, so a caller
			// cannot get two answers to one question.
			one, err := h.cat.ChunkManifestState(ctx, tc.hash)
			if err != nil {
				t.Fatal(err)
			}
			if one != tc.want {
				t.Errorf("ChunkManifestState = %q, want %q", one, tc.want)
			}
		})
	}

	// The two states a boolean collapsed together are DIFFERENT here. This is
	// the assertion the whole issue exists for.
	if states[exempt.String()] == states[undecided.String()] {
		t.Fatal("'never needs a manifest' and 'nobody has decided' came back as the same " +
			"state — that is the boolean again, and it is the distinction replication branches on")
	}
}

// 🔴 Asking generates nothing.
//
// The convenience this forbids is real: a caller asking "does this blob have a
// manifest" is usually about to want one, and producing it here would make the
// question unaskable by anything that was trying to decide whether the work
// was worth doing.
func TestAskingForTheStateGeneratesNothing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	blob := blobHash(t, "e")
	h.seedBlob(t, blob, 9000)

	beforeManifests := h.manifestRows(t)
	beforeChunks := h.chunkRows(t)
	beforeJobs := h.jobCount(t)
	beforeEvents := h.eventCount(t)

	// Ask repeatedly, in both shapes.
	for range 5 {
		got, err := h.cat.ChunkManifestState(ctx, blob)
		if err != nil {
			t.Fatal(err)
		}
		if got != manifests.StateUndecided {
			t.Fatalf("state = %q, want %q — and 'undecided' is a final answer, not a prompt",
				got, manifests.StateUndecided)
		}
		if _, found, err := h.cat.ChunkManifest(ctx, blob); err != nil {
			t.Fatal(err)
		} else if found {
			t.Fatal("reading the manifest of a blob that has none returned one")
		}
		if _, err := h.cat.ChunkManifestStates(ctx); err != nil {
			t.Fatal(err)
		}
	}

	if got := h.manifestRows(t); got != beforeManifests {
		t.Errorf("asking created %d manifest row(s)", got-beforeManifests)
	}
	if got := h.chunkRows(t); got != beforeChunks {
		t.Errorf("asking created %d manifest_chunks row(s)", got-beforeChunks)
	}
	// No job at all, and specifically no chunk_blob job. Counting only the
	// named kind would pass on a handler that enqueued the work under another
	// name.
	if got := h.jobCount(t); got != beforeJobs {
		t.Errorf("asking enqueued %d job(s); the question is a read", got-beforeJobs)
	}
	var chunkJobs int
	if err := h.db.Reader().QueryRow(
		`SELECT count(*) FROM jobs WHERE type = 'chunk_blob'`).Scan(&chunkJobs); err != nil {
		t.Fatal(err)
	}
	if chunkJobs != 0 {
		t.Errorf("asking enqueued %d chunk_blob job(s) — that is the trap ADR-0034 names", chunkJobs)
	}
	if got := h.eventCount(t); got != beforeEvents {
		t.Errorf("asking emitted %d event(s)", got-beforeEvents)
	}
	// And nothing was recorded as exempt either: 'undecided' must not be
	// resolved by writing a decision nobody took.
	var decided int
	if err := h.db.Reader().QueryRow(
		`SELECT count(*) FROM blobs WHERE chunking_exempt_reason IS NOT NULL`).Scan(&decided); err != nil {
		t.Fatal(err)
	}
	if decided != 0 {
		t.Errorf("asking recorded %d chunking decision(s)", decided)
	}
}

// A manifest is not an identity (ADR-0034).
//
// # About the fixture
//
// Two blobs with genuinely equal chunk lists and different whole-object
// digests cannot arise from real bytes: the chunk sequence determines the byte
// sequence, which determines the BLAKE3. So the fixture is built deliberately
// at the storage layer, which is exactly where the conflation ADR-0034 forbids
// would be implemented — a lookup that treated two identical chunk lists as
// one blob would do it here, on rows, not on bytes.
func TestAManifestIsNotAnIdentity(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	first := blobHash(t, "1")
	second := blobHash(t, "2")
	h.seedBlob(t, first, 1000)
	h.seedBlob(t, second, 1000)

	shared := chunkSeq(t, 600, 400)
	firstManifest := mustBuild(t, first, shared)
	secondManifest := mustBuild(t, second, shared)
	if err := h.cat.SaveChunkManifest(ctx, firstManifest); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.SaveChunkManifest(ctx, secondManifest); err != nil {
		t.Fatal(err)
	}

	// Two blobs, two manifests. Neither absorbed the other.
	if got := h.manifestRows(t); got != 2 {
		t.Fatalf("%d manifest rows for two blobs with identical chunk lists, want 2 — "+
			"conflation by chunk list is deduplication reasoning one level too high", got)
	}
	for _, blob := range []hashing.Hash{first, second} {
		m, found, err := h.cat.ChunkManifest(ctx, blob)
		if err != nil || !found {
			t.Fatalf("manifest for %s: found=%v err=%v", blob, found, err)
		}
		if !m.BlobHash.Equal(blob) {
			t.Errorf("the manifest read back for %s describes %s", blob, m.BlobHash)
		}
	}

	// The manifest's own digest binds the blob it describes, so a manifest
	// cannot be lifted from one blob and presented as another's.
	if firstManifest.Digest.Equal(secondManifest.Digest) {
		t.Error("two blobs with identical chunk lists produced the same manifest digest — " +
			"the manifest digest does not bind the blob it describes")
	}

	// And the chunk index answers "where", never "which blob is this": the
	// shared chunk resolves to BOTH blobs, so it cannot be an identity.
	for _, blob := range []hashing.Hash{first, second} {
		local := make([]manifests.LocalChunk, 0, len(shared))
		for _, c := range shared {
			local = append(local, manifests.LocalChunk{
				Digest: c.Digest, BlobHash: blob, Offset: c.Offset, Length: c.Length,
			})
		}
		if err := h.cat.RecordLocal(ctx, blob, local); err != nil {
			t.Fatal(err)
		}
	}
	where, err := h.cat.Locate(ctx, shared[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	if len(where) != 2 {
		t.Fatalf("a chunk shared by two blobs resolved to %d place(s), want 2 — a chunk "+
			"digest maps to SOME blob and offset, and never answers 'which blob is this'",
			len(where))
	}
}

// The manifest's own digest is checked on read, so a tampered manifest_chunks
// row is caught before anything reassembles from it.
func TestATamperedManifestChunkRowIsDetected(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	blob := blobHash(t, "f")
	h.seedBlob(t, blob, 1000)
	if err := h.cat.SaveChunkManifest(ctx, mustBuild(t, blob, chunkSeq(t, 600, 400))); err != nil {
		t.Fatal(err)
	}
	// It reads cleanly first, so the failure below is about the tampering.
	if _, found, err := h.cat.ChunkManifest(ctx, blob); err != nil || !found {
		t.Fatalf("setup: found=%v err=%v", found, err)
	}

	tampered := "blake3:" + strings.Repeat("9", 64)

	for _, tc := range []struct {
		name  string
		query string
		args  []any
	}{
		{
			"a swapped chunk digest",
			`UPDATE manifest_chunks SET digest = ? WHERE blob_hash = ? AND idx = 1`,
			[]any{tampered, blob.String()},
		},
		{
			"a changed chunk length",
			`UPDATE manifest_chunks SET byte_length = byte_length - 1 WHERE blob_hash = ? AND idx = 1`,
			[]any{blob.String()},
		},
		{
			"two chunk digests exchanged — the reordering the whole-object hash is otherwise alone in catching",
			`UPDATE manifest_chunks SET digest = CASE idx WHEN 0 THEN
				(SELECT digest FROM manifest_chunks WHERE blob_hash = ? AND idx = 1)
			 ELSE (SELECT digest FROM manifest_chunks WHERE blob_hash = ? AND idx = 0) END
			 WHERE blob_hash = ?`,
			[]any{blob.String(), blob.String(), blob.String()},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Rewrite the manifest cleanly before each case.
			if err := h.cat.SaveChunkManifest(ctx, mustBuild(t, blob, chunkSeq(t, 600, 400))); err != nil {
				t.Fatal(err)
			}
			h.exec(t, tc.query, tc.args...)

			_, _, err := h.cat.ChunkManifest(ctx, blob)
			if err == nil {
				t.Fatal("a tampered manifest read back as intact")
			}
			if !errors.Is(err, manifests.ErrDigestMismatch) && !errors.Is(err, manifests.ErrMalformed) {
				t.Fatalf("error = %v, want a digest or shape failure", err)
			}
		})
	}
}

// A hole in the idx sequence is a hole, not a shorter manifest. Deleting a row
// shifts every chunk after it while still reassembling something.
func TestAMissingChunkRowIsDetected(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	blob := blobHash(t, "3")
	h.seedBlob(t, blob, 1000)
	if err := h.cat.SaveChunkManifest(ctx, mustBuild(t, blob, chunkSeq(t, 600, 400))); err != nil {
		t.Fatal(err)
	}
	h.exec(t, `DELETE FROM manifest_chunks WHERE blob_hash = ? AND idx = 0`, blob.String())

	if _, _, err := h.cat.ChunkManifest(ctx, blob); err == nil {
		t.Fatal("a manifest missing a chunk row read back as intact")
	}
}

// Cascades, asserted DELIBERATELY (the M4-12 lesson).
//
// Deleting a blob takes its manifest, its chunk rows AND its entries in the
// local chunk index — the last because a claim to hold bytes at an offset of a
// blob that no longer exists is a dangling claim, and a reuse index full of
// them sends a transfer to fetch chunks from nowhere.
func TestDeletingABlobTakesItsManifestAndItsChunkIndex(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	doomed := blobHash(t, "4")
	survivor := blobHash(t, "5")
	h.seedBlob(t, doomed, 1000)
	h.seedBlob(t, survivor, 1000)

	chunks := chunkSeq(t, 600, 400)
	for _, blob := range []hashing.Hash{doomed, survivor} {
		if err := h.cat.SaveChunkManifest(ctx, mustBuild(t, blob, chunks)); err != nil {
			t.Fatal(err)
		}
		local := make([]manifests.LocalChunk, 0, len(chunks))
		for _, c := range chunks {
			local = append(local, manifests.LocalChunk{
				Digest: c.Digest, BlobHash: blob, Offset: c.Offset, Length: c.Length,
			})
		}
		if err := h.cat.RecordLocal(ctx, blob, local); err != nil {
			t.Fatal(err)
		}
	}
	if h.manifestRows(t) != 2 || h.chunkRows(t) != 4 || h.localChunkRows(t) != 4 {
		t.Fatalf("setup: %d manifests, %d chunks, %d index rows",
			h.manifestRows(t), h.chunkRows(t), h.localChunkRows(t))
	}

	h.exec(t, `DELETE FROM blobs WHERE hash = ?`, doomed.String())

	if got := h.manifestRows(t); got != 1 {
		t.Errorf("%d manifest rows after deleting one of two blobs, want 1", got)
	}
	if got := h.chunkRows(t); got != 2 {
		t.Errorf("%d manifest_chunks rows, want 2 — the chunk rows are the manifest and go with it", got)
	}
	if got := h.localChunkRows(t); got != 2 {
		t.Errorf("%d local_chunks rows, want 2 — the index kept a claim to hold bytes at an "+
			"offset of a blob that no longer exists", got)
	}
	// The survivor is untouched, so the cascade was scoped and not a sweep.
	if _, found, err := h.cat.ChunkManifest(ctx, survivor); err != nil || !found {
		t.Errorf("the surviving blob lost its manifest: found=%v err=%v", found, err)
	}
	where, err := h.cat.Locate(ctx, chunks[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	if len(where) != 1 || !where[0].BlobHash.Equal(survivor) {
		t.Errorf("the chunk index resolves to %+v, want only the surviving blob", where)
	}
}

// The other half of the cascade, and it is the half that had to be chosen
// rather than inherited: dropping a MANIFEST must not drop the record of bytes
// this node is holding. ADR-0034's falsification test is that deleting every
// manifest costs speed and nothing else.
func TestDiscardingAManifestKeepsTheBlobAndTheLocalChunkIndex(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	blob := blobHash(t, "6")
	h.seedBlob(t, blob, 1000)
	chunks := chunkSeq(t, 600, 400)
	if err := h.cat.SaveChunkManifest(ctx, mustBuild(t, blob, chunks)); err != nil {
		t.Fatal(err)
	}
	local := make([]manifests.LocalChunk, 0, len(chunks))
	for _, c := range chunks {
		local = append(local, manifests.LocalChunk{
			Digest: c.Digest, BlobHash: blob, Offset: c.Offset, Length: c.Length,
		})
	}
	if err := h.cat.RecordLocal(ctx, blob, local); err != nil {
		t.Fatal(err)
	}

	if err := h.cat.DiscardChunkManifest(ctx, blob); err != nil {
		t.Fatal(err)
	}

	if got := h.manifestRows(t); got != 0 {
		t.Errorf("%d manifest rows after a discard", got)
	}
	if got := h.chunkRows(t); got != 0 {
		t.Errorf("%d manifest_chunks rows survived the discard of their manifest", got)
	}
	if got := h.localChunkRows(t); got != len(chunks) {
		t.Errorf("%d local_chunks rows, want %d — discarding a manifest must not discard "+
			"the record of bytes this node holds", got, len(chunks))
	}
	// The blob is still there, and it is back to 'undecided' — which is the
	// truth: nobody has decided anything about it any more.
	got, err := h.cat.ChunkManifestState(ctx, blob)
	if err != nil {
		t.Fatal(err)
	}
	if got != manifests.StateUndecided {
		t.Errorf("state after discarding the manifest = %q, want %q", got, manifests.StateUndecided)
	}
}

// The recorded decision is a decision, with grounds, and it cannot coexist
// with a manifest.
func TestRecordingThatAManifestIsNotRequired(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	small := blobHash(t, "7")
	chunked := blobHash(t, "8")
	h.seedBlob(t, small, 12)
	h.seedBlob(t, chunked, 1000)
	if err := h.cat.SaveChunkManifest(ctx, mustBuild(t, chunked, chunkSeq(t, 600, 400))); err != nil {
		t.Fatal(err)
	}

	if err := h.cat.RecordChunkingNotRequired(ctx, small, ""); err == nil {
		t.Error("a decision with no grounds was accepted — it cannot be reviewed when the " +
			"threshold moves")
	}
	if err := h.cat.RecordChunkingNotRequired(ctx, chunked, "small"); err == nil {
		t.Error("a blob that HAS a manifest was also recorded as never needing one")
	}
	if err := h.cat.RecordChunkingNotRequired(ctx, small, "smaller than one chunk"); err != nil {
		t.Fatal(err)
	}
	// Idempotent: the job that records this will be re-run (invariant 9).
	if err := h.cat.RecordChunkingNotRequired(ctx, small, "smaller than one chunk"); err != nil {
		t.Fatal(err)
	}
	if got, err := h.cat.ChunkManifestState(ctx, small); err != nil {
		t.Fatal(err)
	} else if got != manifests.StateNotRequired {
		t.Errorf("state = %q, want %q", got, manifests.StateNotRequired)
	}

	// A manifest arriving later overrides the decision rather than sitting
	// alongside it, so the two facts can never both be true.
	if err := h.cat.SaveChunkManifest(ctx, mustBuild(t, small, chunkSeq(t, 12))); err != nil {
		t.Fatal(err)
	}
	if got, err := h.cat.ChunkManifestState(ctx, small); err != nil {
		t.Fatal(err)
	} else if got != manifests.StatePresent {
		t.Errorf("state after a manifest arrived = %q, want %q", got, manifests.StatePresent)
	}
}

// Re-saving converges rather than accumulating: the handler will be re-run
// (invariant 9), and re-chunking under other parameters shares no boundaries
// with the old manifest.
func TestSavingAManifestTwiceConverges(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	blob := blobHash(t, "9")
	h.seedBlob(t, blob, 1000)
	for range 3 {
		if err := h.cat.SaveChunkManifest(ctx, mustBuild(t, blob, chunkSeq(t, 600, 400))); err != nil {
			t.Fatal(err)
		}
	}
	if got := h.chunkRows(t); got != 2 {
		t.Errorf("%d manifest_chunks rows after three saves of a two-chunk manifest", got)
	}

	// A re-chunk at other parameters REPLACES; it does not leave rows from two
	// incomparable chunkings under one blob.
	coarse := mustBuild(t, blob, chunkSeq(t, 1000))
	if err := h.cat.SaveChunkManifest(ctx, coarse); err != nil {
		t.Fatal(err)
	}
	if got := h.chunkRows(t); got != 1 {
		t.Errorf("%d manifest_chunks rows after re-chunking to one chunk, want 1", got)
	}
}

// A manifest for bytes the catalog has never seen cannot be reached, because a
// manifest is keyed by the blob's identity and by nothing else.
func TestAManifestForAnUnknownBlobIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	unknown := blobHash(t, "e")
	err := h.cat.SaveChunkManifest(ctx, mustBuild(t, unknown, chunkSeq(t, 100)))
	if !errors.Is(err, catalog.ErrManifestBlobUnknown) {
		t.Fatalf("error = %v, want ErrManifestBlobUnknown", err)
	}
	if _, err := h.cat.ChunkManifestState(ctx, unknown); !errors.Is(err, catalog.ErrManifestBlobUnknown) {
		t.Errorf("state of an unknown blob = %v, want ErrManifestBlobUnknown", err)
	}
}

// Re-indexing one blob replaces that blob's rows, so a re-chunk cannot leave
// the index claiming bytes at offsets nothing cuts at any more.
func TestReindexingABlobReplacesItsEntries(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	blob := blobHash(t, "a")
	h.seedBlob(t, blob, 1000)
	first := chunkSeq(t, 600, 400)
	second := chunkSeq(t, 1000)

	toLocal := func(cs []chunking.Chunk) []manifests.LocalChunk {
		out := make([]manifests.LocalChunk, 0, len(cs))
		for _, c := range cs {
			out = append(out, manifests.LocalChunk{
				Digest: c.Digest, BlobHash: blob, Offset: c.Offset, Length: c.Length,
			})
		}
		return out
	}
	if err := h.cat.RecordLocal(ctx, blob, toLocal(first)); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.RecordLocal(ctx, blob, toLocal(second)); err != nil {
		t.Fatal(err)
	}
	if got := h.localChunkRows(t); got != 1 {
		t.Errorf("%d index rows after re-indexing a blob to one chunk, want 1", got)
	}
	stale, err := h.cat.Locate(ctx, first[1].Digest)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Errorf("a boundary from the previous chunking is still in the index: %+v", stale)
	}
}
