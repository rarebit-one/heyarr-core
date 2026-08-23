package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/peer/inventory"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/chunking"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// The replicate_blob handler over a source that has a manifest (§16, §84,
// ADR-0034, ADR-0035, M5-06).
//
// internal/peer/transfer asserts what the chunked transfer DOES. This file
// asserts the two things only the handler and the catalog can answer: that the
// branch between whole and chunked is taken on the source's state, and that a
// partial transfer is not a replica by any of the four routes a partial could
// become one.

// fabricManifests is the source's manifest store on the peer surface.
type fabricManifests struct {
	mu     sync.Mutex
	stored map[string]manifests.Manifest
	held   map[string]bool
}

func newFabricManifests() *fabricManifests {
	return &fabricManifests{stored: map[string]manifests.Manifest{}, held: map[string]bool{}}
}

// hold records that the source has the bytes and has decided nothing about
// chunking them — §16's third state, and the state that means "pull whole".
func (f *fabricManifests) hold(blob hashing.Hash) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held[blob.String()] = true
}

func (f *fabricManifests) store(m manifests.Manifest) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held[m.BlobHash.String()] = true
	f.stored[m.BlobHash.String()] = m
}

func (f *fabricManifests) ChunkManifest(
	_ context.Context, blob hashing.Hash,
) (manifests.Manifest, manifests.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := blob.String()
	if !f.held[key] {
		return manifests.Manifest{}, "", fmt.Errorf("%w: %s", peerapi.ErrNoSuchBlob, blob)
	}
	if m, ok := f.stored[key]; ok {
		return m, manifests.StatePresent, nil
	}
	return manifests.Manifest{}, manifests.StateUndecided, nil
}

// chunkingForFabric is small enough that a 512 KiB fixture has many chunks.
func chunkingForFabric() chunking.Config {
	return chunking.Config{Min: 1 << 10, Avg: 4 << 10, Max: 16 << 10}
}

// publishManifest chunks content and puts the manifest on the source.
func (f *transferFabric) publishManifest(content []byte, blob hashing.Hash) manifests.Manifest {
	f.t.Helper()
	cfg := chunkingForFabric()
	c, err := chunking.New(bytes.NewReader(content), cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	var chunks []chunking.Chunk
	for {
		chunk, err := c.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			f.t.Fatal(err)
		}
		chunks = append(chunks, chunk)
	}
	m, err := manifests.Build(blob, cfg, chunks, time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC))
	if err != nil {
		f.t.Fatal(err)
	}
	if len(m.Chunks) < 8 {
		f.t.Fatalf("the fixture chunked into %d chunks, too few to interrupt part-way", len(m.Chunks))
	}
	f.sourceMans.store(m)
	return m
}

// A source holding a manifest is replicated over the chunked path, and the
// path is OBSERVED — the source records that every request carried a Range
// header — rather than inferred from a transfer that worked.
func TestReplicateBlobTakesTheChunkedPathWhenTheSourceHasAManifest(t *testing.T) {
	f := newTransferFabric(t)
	content := transferPayload(1)
	hash := f.seedBlob(content)
	m := f.publishManifest(content, hash)

	if err := f.run(hash); err != nil {
		t.Fatalf("replicate_blob over a source with a manifest: %v", err)
	}

	if got := f.sourceBlobs.rangedRequests.Load(); got != int64(len(m.Chunks)) {
		t.Errorf("the source served %d ranged requests for a %d chunk manifest — the chunked "+
			"path reads one range per chunk", got, len(m.Chunks))
	}
	state, bytesPresent, ok := f.replicaState(hash)
	if !ok {
		t.Fatal("the destination recorded no replica row")
	}
	// assert_eq on the enum, never a substring.
	if state != "present" {
		t.Errorf("replica state = %q, want %q", state, "present")
	}
	if bytesPresent != int64(len(content)) {
		t.Errorf("bytes_present = %d, want %d", bytesPresent, len(content))
	}
	if err := f.store.Verify(t.Context(), hash); err != nil {
		t.Errorf("the blob assembled from chunks does not verify: %v", err)
	}
	if got := readStoredBlob(t, f.store, hash); !bytes.Equal(got, content) {
		t.Error("the blob assembled from chunks is not the source's content")
	}
}

// A source with no manifest is replicated WHOLE, and that path is observed the
// same way: no request carried a Range header. §16's third state doing its job,
// asserted rather than assumed (ADR-0035).
func TestReplicateBlobPullsWholeWhenTheSourceHasNoManifest(t *testing.T) {
	f := newTransferFabric(t)
	content := transferPayload(2)
	hash := f.seedBlob(content)
	// The source holds the bytes and has decided nothing about chunking them.
	f.sourceMans.hold(hash)

	if err := f.run(hash); err != nil {
		t.Fatalf("replicate_blob over a source with no manifest: %v", err)
	}

	if got := f.sourceBlobs.rangedRequests.Load(); got != 0 {
		t.Errorf("%d requests carried a Range header for a blob with no manifest — there are no "+
			"chunk boundaries to ask for", got)
	}
	if got := f.sourceBlobs.requests.Load(); got != 1 {
		t.Errorf("the source served %d blob requests, want 1 whole read", got)
	}
	state, _, ok := f.replicaState(hash)
	if !ok || state != "present" {
		t.Errorf("replica state = %q (present row: %v), want %q", state, ok, "present")
	}
	if got := readStoredBlob(t, f.store, hash); !bytes.Equal(got, content) {
		t.Error("the whole-pulled blob is not the source's content")
	}
}

// 🔴 A partial transfer is not a replica, by any of the four routes it could
// become one. Four assertions because these are four different code paths and
// M4-12 found that the record of who holds what is easy to get wrong.
func TestAPartialChunkedTransferIsNotAReplicaByAnyRoute(t *testing.T) {
	f := newTransferFabric(t)
	content := transferPayload(3)
	hash := f.seedBlob(content)
	m := f.publishManifest(content, hash)

	// The source goes away part-way through, leaving a real partial on the
	// destination's disk.
	f.sourceBlobs.failRangesAfter.Store(int64(len(m.Chunks) / 3))

	if err := f.run(hash); err == nil {
		t.Fatal("a transfer whose source went away part-way reported success")
	}

	staged := filepath.Join(f.store.Root(), "tmp", cas.PartialName(hash))
	info, err := os.Stat(staged)
	if err != nil {
		t.Fatalf("the interrupted transfer left no staging file, so there is no partial to be "+
			"wrong about: %v", err)
	}
	if info.Size() == 0 || info.Size() >= int64(len(content)) {
		t.Fatalf("the staging file is %d bytes of a %d byte blob, which is not a partial",
			info.Size(), len(content))
	}

	// 1. The store does not have it.
	if held, err := f.store.Has(t.Context(), hash); err != nil || held {
		t.Errorf("Has = %v (err %v) for a blob that is %d/%d received",
			held, err, info.Size(), len(content))
	}

	// 2. It is not in this node's inventory report. The report is collected
	//    from the STORE, so a partial that appeared here would be offered to
	//    every other peer as a copy that exists.
	snapshot, err := inventory.Collect(t.Context(), inventory.Options{
		Store: f.store, Quarantine: f.store,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range snapshot.Entries() {
		if e.BlobHash == hash.String() {
			t.Errorf("the inventory reports the partially received blob as %q", e.State)
		}
	}

	// 3. There is no `present` replica row for this node. `pending` is
	//    correct and honest — a transfer was in flight and is not any more.
	state, bytesPresent, ok := f.replicaState(hash)
	if !ok {
		t.Fatal("no replica row at all, so the state assertion below asserts nothing")
	}
	if state == "present" {
		t.Errorf("replica state = %q for a blob that is %d/%d received", state, info.Size(), len(content))
	}
	// `missing` is what the handler records for an attempt that did not
	// deliver, and it is the honest state: a transfer was in flight, it is not
	// any more, and this node does not hold the bytes. What matters is that it
	// is not `present` — asserted above — and that the state is a stated one
	// rather than whatever the row happened to hold.
	if state != "missing" {
		t.Errorf("replica state = %q, want %q after an attempt that did not deliver", state, "missing")
	}
	if bytesPresent != 0 {
		t.Errorf("bytes_present = %d for a blob that was never assembled — a partial's length is "+
			"progress telemetry and never an address (ADR-0035)", bytesPresent)
	}

	// 4. It is not a durability witness. Replicas() is what the GC sweep reads
	//    to decide whether a blob is safe to reclaim elsewhere, and a partial
	//    offered as one is how a deployment collects its last real copy.
	replicas, err := f.cat.Replicas(t.Context(), hash)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range replicas {
		if r.Peer.PeerID == f.self {
			t.Errorf("this node appears as a durability witness for a blob it holds %d/%d bytes of",
				info.Size(), len(content))
		}
	}
	evidence, err := f.cat.DurabilityEvidence(t.Context(), hash)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evidence {
		if e.PeerID == f.self {
			t.Errorf("this node recorded durability evidence for a partial: %+v", e)
		}
	}
}

// And the resume: the same job, re-run once the source is answering again,
// finishes from what is on disk and writes `present` exactly once.
func TestAnInterruptedChunkedTransferResumesOnTheNextRun(t *testing.T) {
	f := newTransferFabric(t)
	content := transferPayload(4)
	hash := f.seedBlob(content)
	m := f.publishManifest(content, hash)

	f.sourceBlobs.failRangesAfter.Store(int64(len(m.Chunks) / 3))
	if err := f.run(hash); err == nil {
		t.Fatal("the interrupted run reported success")
	}
	firstRanged := f.sourceBlobs.rangedRequests.Load()

	// The source comes back. Nothing was handed between the runs: the second
	// re-reads the staging file and re-hashes it against the manifest.
	f.sourceBlobs.failRangesAfter.Store(0)
	if err := f.run(hash); err != nil {
		t.Fatalf("the resumed run: %v", err)
	}
	secondRanged := f.sourceBlobs.rangedRequests.Load() - firstRanged

	if secondRanged >= int64(len(m.Chunks)) {
		t.Errorf("the resumed run made %d ranged requests for a %d chunk manifest, so it resumed "+
			"nothing", secondRanged, len(m.Chunks))
	}
	state, bytesPresent, ok := f.replicaState(hash)
	if !ok || state != "present" {
		t.Errorf("replica state = %q (row: %v), want %q", state, ok, "present")
	}
	if bytesPresent != int64(len(content)) {
		t.Errorf("bytes_present = %d, want %d", bytesPresent, len(content))
	}
	if got := readStoredBlob(t, f.store, hash); !bytes.Equal(got, content) {
		t.Error("the resumed blob is not the source's content")
	}
	// `present` written once. A resumed transfer that announced arrival twice
	// would make every retry event noise (invariant 9).
	var arrived int
	for _, e := range f.transferEvents(hash) {
		if e == "pending->present" || e == "->present" {
			arrived++
		}
	}
	if arrived > 1 {
		t.Errorf("the blob announced its arrival %d times across an interrupted and a resumed "+
			"run: %v", arrived, f.transferEvents(hash))
	}
	t.Logf("worker resume: %d chunks; the interrupted run made %d ranged requests, the resumed "+
		"run made %d", len(m.Chunks), firstRanged, secondRanged)
}

func readStoredBlob(t *testing.T, store *cas.FS, h hashing.Hash) []byte {
	t.Helper()
	rc, _, err := store.Open(t.Context(), h)
	if err != nil {
		t.Fatalf("opening %s: %v", h, err)
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
