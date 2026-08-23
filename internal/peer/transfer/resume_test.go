package transfer_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/peer/transfer"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// M5-06's acceptance: resumable replication, and what a resumed transfer is
// allowed to trust (§84, ADR-0035, invariants 1 and 9).
//
// The order of this file is the order of the argument. The baseline comes
// first, because without it every refusal below is unfalsifiable — they would
// all pass against a transfer that never completed anything.

// ---------------------------------------------------------------------------
// the baseline

// An uninterrupted chunked transfer produces the same bytes a whole one would,
// takes the chunked path, and publishes once.
func TestAnUninterruptedChunkedTransferIsByteIdentical(t *testing.T) {
	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(src.member(), dst.member())

	content := deterministicContent(1, 256<<10)
	source := startChunkedSource(t, src, root, content)
	blob, m := manifestOf(t, content)
	source.mans.store(m)
	dest := newChunkedDestination(t, dst)

	out, err := dest.puller.PullChunked(t.Context(), source.source(), blob, m)
	if err != nil {
		t.Fatalf("a first, uninterrupted chunked transfer: %v", err)
	}

	// assert_eq on the enum: which path ran is the thing worth asserting, and
	// "it succeeded" is satisfied by either path.
	if out.Mode != transfer.ModeChunked {
		t.Errorf("mode = %q, want %q", out.Mode, transfer.ModeChunked)
	}
	if out.Bytes != int64(len(content)) {
		t.Errorf("transferred %d bytes, the blob is %d", out.Bytes, len(content))
	}
	if out.ChunksFetched != len(m.Chunks) {
		t.Errorf("fetched %d chunks, the manifest has %d", out.ChunksFetched, len(m.Chunks))
	}
	if out.ChunksKept != 0 || out.ChunksReused != 0 {
		t.Errorf("a first transfer kept %d and reused %d chunks", out.ChunksKept, out.ChunksReused)
	}

	// The bytes, compared directly rather than through the digest that
	// published them — a comparison against the store's own answer would be
	// the function under test grading its own work.
	if got := readBlob(t, dest.store, blob); !bytes.Equal(got, content) {
		t.Errorf("the published blob is %d bytes and the source's content is %d, and they differ",
			len(got), len(content))
	}
	if err := dest.store.Verify(t.Context(), blob); err != nil {
		t.Errorf("the published blob does not verify: %v", err)
	}

	served, requests, ranged := source.counting.stats()
	if served != int64(len(content)) {
		t.Errorf("the source served %d bytes for a %d byte blob", served, len(content))
	}
	if ranged != requests {
		t.Errorf("%d of %d requests carried a Range header — the chunked path reads chunk "+
			"boundaries and nothing else", ranged, requests)
	}
	t.Logf("baseline: %d chunks, %d bytes served by the source in %d ranged requests",
		len(m.Chunks), served, requests)

	// Nothing left staged: the partial became a blob rather than being copied
	// into one.
	if size := dest.partialSize(t, blob); size != 0 {
		t.Errorf("a completed transfer left %d bytes staged", size)
	}
}

// A partial is addressable by nothing while it is in flight. The store-level
// paths are asserted here; the catalog-level ones — an inventory report, a
// replica row, a GC durability witness — are asserted in internal/worker,
// where those code paths live.
func TestAPartialTransferIsNotPresentInTheStore(t *testing.T) {
	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(src.member(), dst.member())

	content := deterministicContent(2, 256<<10)
	source := startChunkedSource(t, src, root, content)
	blob, m := manifestOf(t, content)
	source.mans.store(m)
	dest := newChunkedDestination(t, dst)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	source.counting.interruptAfter(64<<10, cancel)
	if _, err := dest.puller.PullChunked(ctx, source.source(), blob, m); err == nil {
		t.Fatal("an interrupted transfer reported success")
	}

	staged := dest.partialSize(t, blob)
	if staged == 0 {
		t.Fatal("the interrupted transfer staged nothing, so there is no partial to be wrong about")
	}
	if staged >= int64(len(content)) {
		t.Fatalf("the interrupted transfer staged %d bytes of a %d byte blob, which is not partial",
			staged, len(content))
	}
	if held, err := dest.store.Has(t.Context(), blob); err != nil || held {
		t.Errorf("Has = %v (err %v) for a blob that is %d/%d received", held, err, staged, len(content))
	}
	if _, _, err := dest.store.Open(t.Context(), blob); !errors.Is(err, cas.ErrNotFound) {
		t.Errorf("Open of a partially received blob returned %v, want ErrNotFound", err)
	}
	var walked int
	if err := dest.store.Walk(t.Context(), func(cas.Descriptor) error { walked++; return nil }); err != nil {
		t.Fatal(err)
	}
	if walked != 0 {
		t.Errorf("Walk visited %d blobs and the only bytes here are a partial transfer", walked)
	}
}

// ---------------------------------------------------------------------------
// the headline: a resumed transfer moves fewer bytes

// 🔴 Interrupt part-way, resume, and assert the second attempt read materially
// less from the SOURCE than the whole blob.
func TestAResumedTransferMovesMateriallyFewerBytes(t *testing.T) {
	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(src.member(), dst.member())

	content := deterministicContent(3, 512<<10)
	source := startChunkedSource(t, src, root, content)
	blob, m := manifestOf(t, content)
	source.mans.store(m)
	dest := newChunkedDestination(t, dst)

	// Interrupted at about three quarters, so the saving is large enough to be
	// a number rather than a rounding error.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	interruptAt := int64(len(content)) * 3 / 4
	source.counting.interruptAfter(interruptAt, cancel)
	if _, err := dest.puller.PullChunked(ctx, source.source(), blob, m); err == nil {
		t.Fatal("the interrupted attempt reported success")
	}
	firstServed, _, _ := source.counting.stats()
	staged := dest.partialSize(t, blob)
	if staged == 0 {
		t.Fatal("nothing was staged, so there is nothing to resume from")
	}

	source.counting.reset()
	out, err := dest.puller.PullChunked(t.Context(), source.source(), blob, m)
	if err != nil {
		t.Fatalf("resuming: %v", err)
	}
	secondServed, secondRequests, _ := source.counting.stats()

	if out.ChunksKept == 0 {
		t.Error("the resumed transfer kept no chunks, so it was a whole retry wearing a resume's name")
	}
	if out.ChunksKept+out.ChunksFetched != len(m.Chunks) {
		t.Errorf("kept %d + fetched %d chunks, the manifest has %d",
			out.ChunksKept, out.ChunksFetched, len(m.Chunks))
	}
	// The stated fraction. A resumed transfer interrupted at three quarters
	// must move well under half the blob.
	limit := int64(len(content)) / 2
	if secondServed >= limit {
		t.Errorf("the resume read %d bytes from the source, want under %d for a %d byte blob",
			secondServed, limit, len(content))
	}
	if secondServed == 0 {
		t.Error("the resume read nothing from the source at all, which is not a resume of an " +
			"interrupted transfer — it is a transfer that had already finished")
	}
	if got := readBlob(t, dest.store, blob); !bytes.Equal(got, content) {
		t.Error("the resumed blob's bytes are not the source's content")
	}
	if err := dest.store.Verify(t.Context(), blob); err != nil {
		t.Errorf("the resumed blob does not verify: %v", err)
	}
	t.Logf("resume: blob %d bytes; interrupted after %d served; kept %d/%d chunks (%d bytes); "+
		"the resume served %d bytes in %d ranged requests (%.1f%% of the blob)",
		len(content), firstServed, out.ChunksKept, len(m.Chunks), out.BytesKept,
		secondServed, secondRequests, 100*float64(secondServed)/float64(len(content)))
}

// The control for the assertion above: with nothing staged, the same transfer
// moves the whole blob. Without it, the saving passes on a transfer that
// silently fetched nothing.
func TestATransferWithNothingStagedMovesTheWholeBlob(t *testing.T) {
	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(src.member(), dst.member())

	content := deterministicContent(4, 256<<10)
	source := startChunkedSource(t, src, root, content)
	blob, m := manifestOf(t, content)
	source.mans.store(m)
	dest := newChunkedDestination(t, dst)

	out, err := dest.puller.PullChunked(t.Context(), source.source(), blob, m)
	if err != nil {
		t.Fatal(err)
	}
	served, _, _ := source.counting.stats()
	if served != int64(len(content)) {
		t.Errorf("a transfer to a node holding nothing served %d bytes of a %d byte blob",
			served, len(content))
	}
	if out.ChunksKept != 0 || out.ChunksReused != 0 {
		t.Errorf("a node holding nothing kept %d and reused %d chunks", out.ChunksKept, out.ChunksReused)
	}
}

// ---------------------------------------------------------------------------
// 🔴 a tampered partial is refused

// Interrupt, edit the staging file, resume. The tampered chunks are discarded
// and re-fetched and the published blob still verifies — three times, because
// a prefix scan that stops early passes one placement and fails another.
func TestATamperedPartialIsRefused(t *testing.T) {
	tests := map[string]struct {
		// tamper edits the staging file and reports how many leading chunks
		// should survive the re-verification.
		tamper func(t *testing.T, path string, staged int64, chunkEnd func(i int) int64) (wantKept int)
	}{
		"in the last received chunk": {
			tamper: func(t *testing.T, path string, staged int64, chunkEnd func(int) int64) int {
				// The last chunk that fully landed; flipping a byte inside it
				// must cost that chunk and nothing before it.
				last := 0
				for chunkEnd(last+1) <= staged {
					last++
				}
				if last < 2 {
					t.Fatalf("only %d chunks landed, which is too few to distinguish the last "+
						"from the first", last)
				}
				flipByteAt(t, path, chunkEnd(last)-1)
				return last - 1
			},
		},
		"in the first chunk": {
			tamper: func(t *testing.T, path string, _ int64, _ func(int) int64) int {
				// Nothing survives: the prefix ends before it starts, and a
				// scan that skipped ahead would keep chunks it never checked.
				flipByteAt(t, path, 0)
				return 0
			},
		},
		"by appending plausible bytes so the length lies": {
			tamper: func(t *testing.T, path string, staged int64, chunkEnd func(int) int64) int {
				last := 0
				for chunkEnd(last+1) <= staged {
					last++
				}
				// Truncate to a chunk boundary, then append a whole chunk's
				// worth of plausible bytes. The file is now longer than what
				// was verifiably received and every byte of the extra is a
				// lie. Length is not evidence.
				truncateTo(t, path, chunkEnd(last))
				appendBytes(t, path, deterministicContent(99, int(chunkEnd(last+1)-chunkEnd(last))))
				return last
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			src := newNode(t, "peer-source", "source")
			dst := newNode(t, "peer-destination", "destination")
			root := newTrustRoot(src.member(), dst.member())

			content := deterministicContent(5, 512<<10)
			source := startChunkedSource(t, src, root, content)
			blob, m := manifestOf(t, content)
			source.mans.store(m)
			dest := newChunkedDestination(t, dst)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			source.counting.interruptAfter(int64(len(content))*3/4, cancel)
			if _, err := dest.puller.PullChunked(ctx, source.source(), blob, m); err == nil {
				t.Fatal("the interrupted attempt reported success")
			}
			staged := dest.partialSize(t, blob)
			if staged == 0 {
				t.Fatal("nothing was staged, so there is nothing to tamper with")
			}

			chunkEnd := func(i int) int64 {
				if i <= 0 {
					return 0
				}
				if i > len(m.Chunks) {
					return m.CoveredSize
				}
				return m.Chunks[i-1].End()
			}
			wantKept := tt.tamper(t, dest.partialPath(blob), staged, chunkEnd)

			source.counting.reset()
			out, err := dest.puller.PullChunked(t.Context(), source.source(), blob, m)
			if err != nil {
				t.Fatalf("resuming after a tamper: %v", err)
			}

			if out.ChunksKept != wantKept {
				t.Errorf("kept %d chunks, want %d — the tampered chunk and everything after it "+
					"must be discarded", out.ChunksKept, wantKept)
			}
			if out.ChunksFetched != len(m.Chunks)-wantKept {
				t.Errorf("fetched %d chunks, want %d", out.ChunksFetched, len(m.Chunks)-wantKept)
			}
			// The published bytes, compared against the source's content
			// directly. This is what "the published blob verifies" has to mean
			// — the store's own digest check is the thing being tested.
			if got := readBlob(t, dest.store, blob); !bytes.Equal(got, content) {
				t.Error("the blob published after a tamper is not the source's content")
			}
			if err := dest.store.Verify(t.Context(), blob); err != nil {
				t.Errorf("the blob published after a tamper does not verify: %v", err)
			}
			served, _, _ := source.counting.stats()
			t.Logf("%s: kept %d/%d chunks, refetched %d bytes from the source",
				name, out.ChunksKept, len(m.Chunks), served)
		})
	}
}

// A staging file that is pure invention — the right length, the wrong bytes
// throughout — costs the whole blob and produces the right one.
func TestAWhollyInventedPartialCostsTheWholeBlob(t *testing.T) {
	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(src.member(), dst.member())

	content := deterministicContent(6, 256<<10)
	source := startChunkedSource(t, src, root, content)
	blob, m := manifestOf(t, content)
	source.mans.store(m)
	dest := newChunkedDestination(t, dst)

	// Written straight into the staging path, at exactly the blob's length: a
	// transfer that trusted its own file's length would publish this.
	if err := os.MkdirAll(dest.root+"/tmp", 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest.partialPath(blob), deterministicContent(7, len(content)), 0o640); err != nil {
		t.Fatal(err)
	}

	out, err := dest.puller.PullChunked(t.Context(), source.source(), blob, m)
	if err != nil {
		t.Fatalf("transferring over an invented partial: %v", err)
	}
	if out.ChunksKept != 0 {
		t.Errorf("kept %d chunks of a staging file that shares no bytes with the blob", out.ChunksKept)
	}
	if out.ChunksFetched != len(m.Chunks) {
		t.Errorf("fetched %d chunks, want all %d", out.ChunksFetched, len(m.Chunks))
	}
	if got := readBlob(t, dest.store, blob); !bytes.Equal(got, content) {
		t.Error("the published blob is not the source's content")
	}
	served, _, _ := source.counting.stats()
	if served != int64(len(content)) {
		t.Errorf("the source served %d bytes, want the whole %d byte blob", served, len(content))
	}
}

// ---------------------------------------------------------------------------
// the manifest is the description everything else is decided from

// A manifest for another blob, or one that does not check out, is refused
// before a byte moves.
func TestAManifestThatIsNotThisBlobsIsRefusedBeforeBytesMove(t *testing.T) {
	src := newNode(t, "peer-source", "source")
	dst := newNode(t, "peer-destination", "destination")
	root := newTrustRoot(src.member(), dst.member())

	content := deterministicContent(8, 128<<10)
	other := deterministicContent(9, 128<<10)
	source := startChunkedSource(t, src, root, content)
	blob, m := manifestOf(t, content)
	source.mans.store(m)
	_, otherManifest := manifestOf(t, other)
	dest := newChunkedDestination(t, dst)

	if _, err := dest.puller.PullChunked(t.Context(), source.source(), blob, otherManifest); !errors.Is(
		err, transfer.ErrManifestMismatch) {
		t.Errorf("pulling with another blob's manifest returned %v, want ErrManifestMismatch", err)
	}

	// A manifest whose recorded digest does not match its own chunk sequence:
	// the same refusal. A description nothing checked is not a description,
	// and this one is checked here rather than only at the far end.
	tampered := m
	tampered.Digest = otherManifest.Digest
	if _, err := dest.puller.PullChunked(t.Context(), source.source(), blob, tampered); !errors.Is(
		err, transfer.ErrManifestMismatch) {
		t.Errorf("pulling with a manifest that fails its own digest returned %v, "+
			"want ErrManifestMismatch", err)
	}

	if served, _, _ := source.counting.stats(); served != 0 {
		t.Errorf("the source served %d bytes for a refused manifest", served)
	}
	if size := dest.partialSize(t, blob); size != 0 {
		t.Errorf("a refused manifest staged %d bytes", size)
	}
}

// helpers ------------------------------------------------------------------

func flipByteAt(t *testing.T, path string, off int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0o640) // #nosec G304 -- a test fixture path
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	one := make([]byte, 1)
	if _, err := f.ReadAt(one, off); err != nil {
		t.Fatal(err)
	}
	one[0] ^= 0xff
	if _, err := f.WriteAt(one, off); err != nil {
		t.Fatal(err)
	}
}

func truncateTo(t *testing.T, path string, n int64) {
	t.Helper()
	if err := os.Truncate(path, n); err != nil {
		t.Fatal(err)
	}
}

func appendBytes(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o640) // #nosec G304 -- a test fixture path
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
}
