package transfer_test

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/peer/transfer"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/chunking"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// M5-07's acceptance: fetch only what this node does not hold (§20, ADR-0034).
//
// §14 makes blobs immutable, so a retagged or re-muxed file is a NEW blob with
// a new digest. These tests are about that case — the ordinary case in a media
// library — and about the two things it must not become: a transfer that
// believes its own index, and a store that decides two blobs sharing chunks
// are one blob.

// hold puts a blob into a destination's store and indexes its chunks, exactly
// as a node that had ingested it would.
func (d *chunkedDestination) hold(t *testing.T, content []byte) hashing.Hash {
	t.Helper()
	desc, err := d.store.Put(t.Context(), bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	d.index.record(desc.Hash, chunkAll(t, content, testChunking()))
	return desc.Hash
}

// modifiedInPlace changes a region in the middle without moving anything: the
// case a fixed-size chunker also handles.
func modifiedInPlace(original []byte, at int) []byte {
	out := append([]byte(nil), original...)
	patch := deterministicContent(4242, 6<<10)
	copy(out[at:], patch)
	return out
}

// prepended puts new bytes at the FRONT: every absolute offset after them
// moves, which is the case a fixed-size chunker fails completely and the case
// content-defined chunking exists for.
func prepended(original []byte, n int) []byte {
	return append(deterministicContent(9999, n), original...)
}

// ---------------------------------------------------------------------------
// 🔴 the headline

// A modified large file transfers below a stated fraction of its size to a
// peer that already holds the original — in the middle-edit case and in the
// prepend case, measured on the source.
func TestAModifiedBlobTransfersFarLessThanItsSize(t *testing.T) {
	original := deterministicContent(11, 1<<20)

	tests := map[string]struct {
		modified []byte
		// maxFraction is the share of the blob the source is allowed to serve.
		maxFraction float64
	}{
		"a region changed in the middle": {
			modified:    modifiedInPlace(original, 400<<10),
			maxFraction: 0.25,
		},
		"bytes prepended, so every offset moves": {
			modified:    prepended(original, 3<<10),
			maxFraction: 0.25,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			src := newNode(t, "peer-source", "source")
			dst := newNode(t, "peer-destination", "destination")
			root := newTrustRoot(src.member(), dst.member())

			source := startChunkedSource(t, src, root, nil)
			blob, m := source.addBlob(t, tt.modified)
			dest := newChunkedDestination(t, dst)
			dest.hold(t, original)

			out, err := dest.puller.PullChunked(t.Context(), source.source(), blob, m)
			if err != nil {
				t.Fatalf("replicating a modified blob to a node holding the original: %v", err)
			}
			served, requests, _ := source.counting.stats()

			limit := int64(float64(len(tt.modified)) * tt.maxFraction)
			if served > limit {
				t.Errorf("the source served %d bytes of a %d byte blob (%.1f%%), want at most %d (%.0f%%)",
					served, len(tt.modified), 100*float64(served)/float64(len(tt.modified)),
					limit, 100*tt.maxFraction)
			}
			if out.ChunksReused == 0 {
				t.Error("nothing was reused, so the saving above is not a saving")
			}
			// The result verifies, asserted directly rather than through "the
			// transfer returned no error".
			if got := readBlob(t, dest.store, blob); !bytes.Equal(got, tt.modified) {
				t.Error("the assembled blob is not the modified content")
			}
			if err := dest.store.Verify(t.Context(), blob); err != nil {
				t.Errorf("the assembled blob does not verify: %v", err)
			}
			t.Logf("%s: blob %d bytes in %d chunks; reused %d chunks (%d bytes), fetched %d chunks "+
				"(%d bytes); the source served %d bytes in %d requests (%.1f%% of the blob)",
				name, len(tt.modified), len(m.Chunks), out.ChunksReused, out.BytesReused,
				out.ChunksFetched, out.BytesFetched, served, requests,
				100*float64(served)/float64(len(tt.modified)))
		})
	}
}

// The control: the same transfer to a peer holding nothing fetches the whole
// blob. Without it the saving above passes on a transfer that fetched nothing
// at all — which the digest check would catch, but a green run should not
// depend on that.
func TestTheSameTransferToAPeerHoldingNothingFetchesEverything(t *testing.T) {
	original := deterministicContent(11, 1<<20)
	modified := modifiedInPlace(original, 400<<10)

	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(src.member(), dst.member())

	source := startChunkedSource(t, src, root, nil)
	blob, m := source.addBlob(t, modified)
	dest := newChunkedDestination(t, dst) // holds nothing, indexes nothing

	out, err := dest.puller.PullChunked(t.Context(), source.source(), blob, m)
	if err != nil {
		t.Fatal(err)
	}
	served, _, _ := source.counting.stats()

	if served != int64(len(modified)) {
		t.Errorf("the source served %d bytes to a peer holding nothing, and the blob is %d",
			served, len(modified))
	}
	if out.ChunksReused != 0 {
		t.Errorf("a peer holding nothing reused %d chunks", out.ChunksReused)
	}
	if out.ChunksFetched != len(m.Chunks) {
		t.Errorf("fetched %d chunks of %d", out.ChunksFetched, len(m.Chunks))
	}
	if got := readBlob(t, dest.store, blob); !bytes.Equal(got, modified) {
		t.Error("the blob pulled with no reuse is not the source's content")
	}
}

// ---------------------------------------------------------------------------
// 🔴 two blobs with identical chunk sets stay two blobs

// ADR-0034's core prohibition, tested rather than assumed: identity is the
// whole-object digest and nothing else, so two blobs that share every chunk
// are two blobs — both present, both readable, each verifying to its own
// digest, neither disturbed by the other.
//
// The two manifests are built BY HAND from the same two pieces rather than by
// running the chunker over two concatenations, and that is the whole fixture.
// FastCDC over X+Y and over Y+X produces boundaries that differ at the join,
// so those two blobs share MOST of their chunks and not all of them — which
// leaves the case this test is about untested and lets an implementation that
// keys on the chunk set pass. Here the two chunk sets are identical, element
// for element, and only the order and the whole-object digest differ.
func TestTwoBlobsWithIdenticalChunkSetsStayTwoBlobs(t *testing.T) {
	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(src.member(), dst.member())

	partA := deterministicContent(21, 300<<10)
	partB := deterministicContent(22, 300<<10)
	first := append(append([]byte(nil), partA...), partB...)
	second := append(append([]byte(nil), partB...), partA...)
	if bytes.Equal(first, second) {
		t.Fatal("the fixture's two blobs are the same bytes")
	}

	source := startChunkedSource(t, src, root, nil)
	firstBlob := putBlob(t, source.store, first)
	secondBlob := putBlob(t, source.store, second)
	if firstBlob.Equal(secondBlob) {
		t.Fatal("the fixture's two blobs have the same digest")
	}
	firstManifest := manifestOfPieces(t, firstBlob, partA, partB)
	secondManifest := manifestOfPieces(t, secondBlob, partB, partA)
	source.mans.store(firstManifest)
	source.mans.store(secondManifest)

	// The two chunk SETS are identical, asserted rather than assumed — a
	// fixture that only mostly overlapped would leave the prohibition untested.
	if !sameChunkSet(firstManifest, secondManifest) {
		t.Fatal("the fixture's two manifests do not have identical chunk sets, so the case this " +
			"test exists for is not reached")
	}
	if firstManifest.Chunks[0].Digest.Equal(secondManifest.Chunks[0].Digest) {
		t.Fatal("the fixture's two manifests are in the same order, so only the order differing " +
			"is not being tested")
	}

	dest := newChunkedDestination(t, dst)
	if _, err := dest.puller.PullChunked(t.Context(), source.source(), firstBlob, firstManifest); err != nil {
		t.Fatalf("the first blob: %v", err)
	}
	// The first blob is now held, and its chunks are indexed where they
	// actually are — so every chunk of the SECOND blob is available locally.
	// This is exactly the situation in which a store might be tempted to
	// decide the two are the same thing.
	dest.index.record(firstBlob, firstManifest.Chunks)

	out, err := dest.puller.PullChunked(t.Context(), source.source(), secondBlob, secondManifest)
	if err != nil {
		t.Fatalf("the second blob: %v", err)
	}
	if out.ChunksReused != len(secondManifest.Chunks) {
		t.Errorf("the second blob reused %d of %d chunks — every one of them is on this disk",
			out.ChunksReused, len(secondManifest.Chunks))
	}
	if out.ChunksFetched != 0 {
		t.Errorf("the second blob fetched %d chunks it already held", out.ChunksFetched)
	}

	for name, want := range map[string]struct {
		hash    hashing.Hash
		content []byte
	}{
		"the first":  {firstBlob, first},
		"the second": {secondBlob, second},
	} {
		held, err := dest.store.Has(t.Context(), want.hash)
		if err != nil {
			t.Fatal(err)
		}
		if !held {
			t.Errorf("%s blob is not present", name)
		}
		if got := readBlob(t, dest.store, want.hash); !bytes.Equal(got, want.content) {
			t.Errorf("%s blob's bytes are not its own", name)
		}
		if err := dest.store.Verify(t.Context(), want.hash); err != nil {
			t.Errorf("%s blob does not verify to its own digest: %v", name, err)
		}
	}

	// Two blobs, both stored. Reuse makes transfers cheaper and storage no
	// smaller, and this is where that would silently stop being true.
	var blobs int
	if err := dest.store.Walk(t.Context(), func(cas.Descriptor) error { blobs++; return nil }); err != nil {
		t.Fatal(err)
	}
	if blobs != 2 {
		t.Errorf("the store holds %d blobs after receiving two that share every chunk, want 2", blobs)
	}
}

// putBlob puts content into a store and returns its digest.
func putBlob(t *testing.T, store *cas.FS, content []byte) hashing.Hash {
	t.Helper()
	desc, err := store.Put(t.Context(), bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return desc.Hash
}

// manifestOfPieces builds a manifest whose chunks are exactly the pieces
// given, in the order given.
//
// A manifest is a description (ADR-0034), so one that was not produced by the
// chunker is still a legitimate description of a blob as long as it covers it
// contiguously — which is what makes this fixture possible at all, and what
// lets two blobs be described by identical chunk sets in different orders.
func manifestOfPieces(t *testing.T, blob hashing.Hash, pieces ...[]byte) manifests.Manifest {
	t.Helper()
	var (
		chunks []chunking.Chunk
		off    int64
	)
	for _, piece := range pieces {
		h := hashing.New()
		if _, err := h.Write(piece); err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, chunking.Chunk{Offset: off, Length: int64(len(piece)), Digest: h.Sum()})
		off += int64(len(piece))
	}
	m, err := manifests.Build(blob, testChunking(), chunks,
		time.Date(2026, 8, 24, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// sameChunkSet reports whether two manifests describe the same multiset of
// chunk digests, ignoring order.
func sameChunkSet(a, b manifests.Manifest) bool {
	if len(a.Chunks) != len(b.Chunks) {
		return false
	}
	counts := map[hashing.Hash]int{}
	for _, c := range a.Chunks {
		counts[c.Digest]++
	}
	for _, c := range b.Chunks {
		counts[c.Digest]--
		if counts[c.Digest] < 0 {
			return false
		}
	}
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// the index is a claim

// 🔴 A stale index entry — pointing at a blob whose bytes are not what it says
// they are — is caught by the re-verification, and the chunk is fetched
// instead. Watch it fire: the counter is asserted, not just the outcome.
func TestAStaleIndexEntryIsCaughtAndTheChunkIsFetched(t *testing.T) {
	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(src.member(), dst.member())

	original := deterministicContent(31, 512<<10)
	modified := modifiedInPlace(original, 200<<10)

	source := startChunkedSource(t, src, root, nil)
	blob, m := source.addBlob(t, modified)
	dest := newChunkedDestination(t, dst)
	decoy := dest.hold(t, original)

	// A claim that one of the CHANGED chunks — bytes this node has never held
	// — sits inside the decoy. It is chosen from the chunks the original does
	// not contain on purpose: an entry the honest indexing also produced would
	// be satisfied by the honest candidate, and the stale one would never be
	// read at all.
	held := map[hashing.Hash]bool{}
	for _, c := range chunkAll(t, original, testChunking()) {
		held[c.Digest] = true
	}
	var target chunking.Chunk
	for _, c := range m.Chunks {
		if !held[c.Digest] {
			target = c
			break
		}
	}
	if target.Digest.IsZero() {
		t.Fatal("the modified fixture shares every chunk with the original, so there is no chunk " +
			"a stale entry could be the only claim for")
	}
	dest.index.claim(manifests.LocalChunk{
		Digest: target.Digest, BlobHash: decoy, Offset: 1024, Length: target.Length,
	})

	out, err := dest.puller.PullChunked(t.Context(), source.source(), blob, m)
	if err != nil {
		t.Fatalf("transferring over a stale index entry: %v", err)
	}

	if out.ChunksIndexStale == 0 {
		t.Error("no index entry was found stale, so the re-verification never fired and this test " +
			"is asserting nothing")
	}
	if got := readBlob(t, dest.store, blob); !bytes.Equal(got, modified) {
		t.Error("the blob assembled over a stale index entry is not the source's content")
	}
	if err := dest.store.Verify(t.Context(), blob); err != nil {
		t.Errorf("the blob assembled over a stale index entry does not verify: %v", err)
	}
	// The decoy is intact and was NOT quarantined: a stale index entry is not
	// a damaged blob, and treating it as one would quarantine healthy content.
	if err := dest.store.Verify(t.Context(), decoy); err != nil {
		t.Errorf("the donor blob was disturbed by a stale index entry pointing into it: %v", err)
	}
	t.Logf("stale index: %d entries found stale, %d chunks reused, %d fetched",
		out.ChunksIndexStale, out.ChunksReused, out.ChunksFetched)
}

// An index entry naming a blob this node no longer holds is not an error: the
// chunk is fetched and the transfer completes.
func TestAnIndexEntryNamingAMissingBlobCostsAFetch(t *testing.T) {
	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(src.member(), dst.member())

	content := deterministicContent(33, 256<<10)
	source := startChunkedSource(t, src, root, nil)
	blob, m := source.addBlob(t, content)
	dest := newChunkedDestination(t, dst)

	gone := hashing.MustParse("blake3:abcdef" + strings.Repeat("01", 29))
	for _, c := range m.Chunks {
		dest.index.claim(manifests.LocalChunk{
			Digest: c.Digest, BlobHash: gone, Offset: c.Offset, Length: c.Length,
		})
	}

	out, err := dest.puller.PullChunked(t.Context(), source.source(), blob, m)
	if err != nil {
		t.Fatalf("transferring with an index full of entries naming a missing blob: %v", err)
	}
	if out.ChunksFetched != len(m.Chunks) {
		t.Errorf("fetched %d chunks, want all %d", out.ChunksFetched, len(m.Chunks))
	}
	if got := readBlob(t, dest.store, blob); !bytes.Equal(got, content) {
		t.Error("the assembled blob is not the source's content")
	}
}

// 🔴 Reuse across a CORRUPTED local source: the donor blob's bytes were
// replaced after it was indexed. The chunk is not reused, the corruption
// reaches quarantine on the existing path (ADR-0018), and the transfer
// completes from the network.
func TestACorruptDonorIsNotReusedAndIsQuarantined(t *testing.T) {
	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(src.member(), dst.member())

	original := deterministicContent(41, 512<<10)
	modified := modifiedInPlace(original, 300<<10)

	source := startChunkedSource(t, src, root, nil)
	blob, m := source.addBlob(t, modified)
	dest := newChunkedDestination(t, dst)
	donor := dest.hold(t, original)

	// The bytes under the donor's name are replaced — an external tool
	// rewriting a hard-linked original is the case ADR-0018 exists for, and it
	// looks exactly like this from here.
	path, err := dest.store.LocalPath(t.Context(), donor)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, deterministicContent(42, len(original)), 0o640); err != nil {
		t.Fatal(err)
	}

	out, err := dest.puller.PullChunked(t.Context(), source.source(), blob, m)
	if err != nil {
		t.Fatalf("transferring with a corrupt donor: %v", err)
	}

	if out.ChunksReused != 0 {
		t.Errorf("%d chunks were reused out of a blob whose bytes are not what they claim to be",
			out.ChunksReused)
	}
	if out.ChunksIndexStale == 0 {
		t.Error("the corrupt donor was never even attempted, so nothing detected it")
	}
	if got := readBlob(t, dest.store, blob); !bytes.Equal(got, modified) {
		t.Error("the blob completed from the network is not the source's content")
	}
	// Reported on the existing path: quarantined, not deleted, and no longer
	// addressable under its own name.
	if held, err := dest.store.Has(t.Context(), donor); err != nil || held {
		t.Errorf("the damaged donor is still addressable: Has = %v (err %v)", held, err)
	}
	quarantined, err := dest.store.QuarantinedBlobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(quarantined) != 1 || !quarantined[0].Hash.Equal(donor) {
		t.Errorf("quarantine holds %+v, want the damaged donor %s", quarantined, donor)
	}
}

// ---------------------------------------------------------------------------
// the fallback still exists

// A source with no manifest is pulled WHOLE, and the whole path is observed
// rather than inferred from success: the mode enum says which ran, and the
// source records that no request carried a Range header.
func TestASourceWithNoManifestIsPulledWhole(t *testing.T) {
	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(src.member(), dst.member())

	content := deterministicContent(51, 256<<10)
	source := startChunkedSource(t, src, root, content)
	blob, _ := manifestOf(t, content)
	// The source holds the bytes and has decided nothing about chunking them,
	// which is §16's third state and an ordinary permanent one.
	source.mans.hold(blob)

	dest := newChunkedDestination(t, dst)
	_, err := dest.puller.FetchManifest(t.Context(), source.source(), blob)
	if !errors.Is(err, transfer.ErrSourceHasNoManifest) {
		t.Fatalf("fetching an absent manifest returned %v, want ErrSourceHasNoManifest", err)
	}

	out, err := dest.puller.Pull(t.Context(), source.source(), blob)
	if err != nil {
		t.Fatalf("pulling whole: %v", err)
	}
	if out.Mode != transfer.ModeWhole {
		t.Errorf("mode = %q, want %q", out.Mode, transfer.ModeWhole)
	}
	_, _, ranged := source.counting.stats()
	if ranged != 0 {
		t.Errorf("%d requests carried a Range header on the whole path — a blob with no manifest "+
			"has no chunk boundaries to ask for", ranged)
	}
	if got := readBlob(t, dest.store, blob); !bytes.Equal(got, content) {
		t.Error("the whole-pulled blob is not the source's content")
	}
	if size := dest.partialSize(t, blob); size != 0 {
		t.Errorf("the whole path staged %d resumable bytes, and it has nothing to resume", size)
	}
}
