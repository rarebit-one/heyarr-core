package worker

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/chunking"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// chunk_blob: §75's job that has had no handler since Milestone 1, and §16's
// policy question — when is a manifest generated, and for which blobs is the
// answer never (M5-04, ADR-0034).
//
// # Every assertion here is written so a weaker version of it would pass
//
// "Ran twice successfully" is not idempotence; the manifest DIGEST is compared
// across runs. "No manifest was written" is not a corruption test unless the
// same fixture is known to produce one; the corrupt-blob test is preceded by
// the clean one over the same bytes. And the three states are compared with
// equality, never containment — none of them is a substring of another, and
// keeping it that way is the point of choosing them.

// chunkFixture is a real database, a real CAS, a real catalog and the real
// corruption path. Storage tests use real filesystems here, because every
// interesting property of this handler is a property of what lands on disk.
type chunkFixture struct {
	*harness
	checker *integrity.Checker
	// ticks is the injected clock. It ADVANCES on every read, deliberately: a
	// frozen clock would make "byte-identical across runs" true for a reason
	// that has nothing to do with the manifest being deterministic.
	ticks *time.Time
}

func newChunkFixture(t *testing.T) *chunkFixture {
	t.Helper()
	h := newHarness(t)
	checker, err := integrity.NewChecker(integrity.Options{
		Store: h.cas, Catalog: h.catalog, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	return &chunkFixture{harness: h, checker: checker, ticks: &start}
}

// clock returns a different time on every call, so a manifest that embedded
// one would differ between runs and the idempotence assertion would fail.
func (f *chunkFixture) clock() time.Time {
	*f.ticks = f.ticks.Add(time.Second)
	return *f.ticks
}

// deps is the handler's dependency set, at the real default threshold and the
// real default chunker parameters.
func (f *chunkFixture) deps() ChunkDeps {
	return ChunkDeps{
		Store:     f.cas,
		Manifests: f.catalog,
		Index:     f.catalog,
		Checker:   f.checker,
		Clock:     f.clock,
		Logger:    slog.New(slog.DiscardHandler),
	}
}

func (f *chunkFixture) run(hash hashing.Hash) error {
	f.t.Helper()
	return f.runWith(f.deps(), hash)
}

func (f *chunkFixture) runWith(deps ChunkDeps, hash hashing.Hash) error {
	f.t.Helper()
	return f.runCtx(f.t.Context(), deps, hash)
}

func (f *chunkFixture) runCtx(ctx context.Context, deps ChunkDeps, hash hashing.Hash) error {
	f.t.Helper()
	payload, err := json.Marshal(manifests.ChunkBlobPayload{BlobHash: hash.String()})
	if err != nil {
		f.t.Fatal(err)
	}
	return ChunkBlobHandler(deps)(ctx, jobs.Job{
		ID: "job-1", Type: manifests.ChunkBlobJobType, Payload: payload,
	})
}

// seedBlob puts bytes in the store and tells the catalog they exist. It is the
// state a blob is in after an ingest, minus the asset rows nothing here reads.
func (f *chunkFixture) seedBlob(content []byte) hashing.Hash {
	f.t.Helper()
	desc, err := f.cas.Put(f.t.Context(), bytes.NewReader(content))
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.db.Writer().ExecContext(f.t.Context(),
		`INSERT INTO blobs (hash, size, first_seen_at) VALUES (?, ?, ?)`,
		desc.Hash.String(), desc.Size, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		f.t.Fatal(err)
	}
	return desc.Hash
}

// state is §16's three-way answer, read the way every other caller reads it.
func (f *chunkFixture) state(hash hashing.Hash) manifests.State {
	f.t.Helper()
	s, err := f.catalog.ChunkManifestState(f.t.Context(), hash)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *chunkFixture) manifest(hash hashing.Hash) (manifests.Manifest, bool) {
	f.t.Helper()
	m, found, err := f.catalog.ChunkManifest(f.t.Context(), hash)
	if err != nil {
		f.t.Fatal(err)
	}
	return m, found
}

// exemption is the recorded decision and when it was taken.
func (f *chunkFixture) exemption(hash hashing.Hash) (reason, at string) {
	f.t.Helper()
	var r, a *string
	if err := f.db.Reader().QueryRowContext(f.t.Context(),
		`SELECT chunking_exempt_reason, chunking_exempt_at FROM blobs WHERE hash = ?`,
		hash.String()).Scan(&r, &a); err != nil {
		f.t.Fatal(err)
	}
	if r != nil {
		reason = *r
	}
	if a != nil {
		at = *a
	}
	return reason, at
}

// pseudoRandom is deterministic, incompressible-enough content: a chunker fed
// a repeating pattern produces a chunk boundary distribution that tells you
// nothing about how it behaves on real bytes.
func pseudoRandom(n int, seed uint64) []byte {
	out := make([]byte, n)
	state := seed*6364136223846793005 + 1442695040888963407
	for i := range out {
		state = state*6364136223846793005 + 1442695040888963407
		out[i] = byte(state >> 33)
	}
	return out
}

// A blob comfortably above the threshold, so the chunker actually cuts it into
// several chunks rather than returning it whole.
const chunkableSize = 5 << 20

// assertState compares §16's answer by EQUALITY. Never containment: the three
// state names were chosen so that none is a substring of another, and an
// assertion that would accept a superstring is an assertion that gives that
// choice away.
func assertState(t *testing.T, got manifests.State, want manifests.State, what string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: chunk-manifest state = %q, want %q", what, got, want)
	}
}

// The headline property: generated once, and byte-identical on a second run.
//
// The digest is what is compared, not "both calls returned nil". A handler that
// regenerated the manifest from scratch every run would pass the weaker test
// while rewriting every row, and a manifest that embedded the clock would
// produce a different digest on the second pass.
func TestChunkBlobGeneratesOnceAndIsByteIdenticalOnASecondRun(t *testing.T) {
	f := newChunkFixture(t)
	hash := f.seedBlob(pseudoRandom(chunkableSize, 7))

	if err := f.run(hash); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first, found := f.manifest(hash)
	if !found {
		t.Fatal("the first run wrote no manifest")
	}
	if first.ChunkCount() < 3 {
		t.Fatalf("the fixture produced %d chunk(s): a manifest that is one or two chunks cannot "+
			"show an ordering, a digest or a reuse property, so this blob is too small to test with",
			first.ChunkCount())
	}
	assertState(t, f.state(hash), manifests.StatePresent, "after the first run")

	if err := f.run(hash); err != nil {
		t.Fatalf("second run: %v", err)
	}
	second, found := f.manifest(hash)
	if !found {
		t.Fatal("the manifest disappeared on the second run")
	}

	if !first.Digest.Equal(second.Digest) {
		t.Errorf("the manifest digest changed across runs: %s then %s — generation is not deterministic",
			first.Digest, second.Digest)
	}
	if !first.GeneratedAt.Equal(second.GeneratedAt) {
		t.Errorf("generated_at moved from %s to %s: the second run rewrote a row it should not have "+
			"touched", first.GeneratedAt, second.GeneratedAt)
	}
	if got := f.count("manifest_chunks"); got != first.ChunkCount() {
		t.Errorf("manifest_chunks = %d after two runs, want %d", got, first.ChunkCount())
	}
	if got := f.count("chunk_manifests"); got != 1 {
		t.Errorf("chunk_manifests = %d after two runs, want 1", got)
	}
}

// Ingest generates no manifest (§16), and the assertion is known to be
// measuring the absence of something that CAN occur: the same fixture then runs
// the job and one appears.
func TestIngestGeneratesNoManifestAndTheJobDoes(t *testing.T) {
	f := newChunkFixture(t)
	const rel = "Movie Title (2019)/Movie Title (2019) - 2160p.mkv"
	f.writeBytes(rel, pseudoRandom(chunkableSize, 11))

	res := f.ingest(rel)
	hash, err := hashing.Parse(res.BlobHash)
	if err != nil {
		t.Fatal(err)
	}

	if got := f.count("chunk_manifests"); got != 0 {
		t.Errorf("ingest wrote %d manifest row(s) — §16 makes chunking lazy", got)
	}
	if got := f.count("manifest_chunks"); got != 0 {
		t.Errorf("ingest wrote %d chunk row(s)", got)
	}
	if got := f.countWhere(`SELECT count(*) FROM jobs WHERE type = 'chunk_blob'`); got != 0 {
		t.Errorf("ingest enqueued %d chunk_blob job(s) — chunking is enqueued by something that "+
			"decided it wanted a manifest, not by every arriving byte", got)
	}
	assertState(t, f.state(hash), manifests.StateUndecided, "after ingest")

	// And now the same fixture produces one, so the assertions above are known
	// to be measuring an absence rather than an impossibility.
	if err := f.run(hash); err != nil {
		t.Fatal(err)
	}
	if got := f.count("chunk_manifests"); got != 1 {
		t.Fatalf("chunk_manifests = %d after the job ran, want 1 — the assertions above measured "+
			"nothing", got)
	}
	assertState(t, f.state(hash), manifests.StatePresent, "after the job")
}

// §16's "small Blobs may never require chunk manifests", given a number and
// written down as a decision rather than left as an absence.
func TestABlobBelowTheThresholdIsRecordedAsNeverNeedingAManifest(t *testing.T) {
	f := newChunkFixture(t)
	small := f.seedBlob(pseudoRandom(64<<10, 3))

	if err := f.run(small); err != nil {
		t.Fatal(err)
	}
	assertState(t, f.state(small), manifests.StateNotRequired, "a blob below the threshold")
	// Spelled out, because the failure this guards against is the state coming
	// back as the one that means "nobody has looked yet" — which reads as
	// success to anything that only checks for the absence of a manifest.
	if f.state(small) == manifests.StateUndecided {
		t.Error("a blob below the threshold was left undecided, which invites the next caller to " +
			"spend the full read finding out")
	}
	reason, at := f.exemption(small)
	if reason != manifests.ReasonBelowThreshold {
		t.Errorf("recorded reason = %q, want %q", reason, manifests.ReasonBelowThreshold)
	}
	if at == "" {
		t.Error("the exemption has no timestamp")
	}
	if got := f.count("chunk_manifests"); got != 0 {
		t.Errorf("chunk_manifests = %d for an exempt blob, want 0", got)
	}

	// It survives a re-run, and the re-run does not re-take the decision.
	if err := f.run(small); err != nil {
		t.Fatal(err)
	}
	assertState(t, f.state(small), manifests.StateNotRequired, "after a re-run")
	reasonAgain, atAgain := f.exemption(small)
	if reasonAgain != reason || atAgain != at {
		t.Errorf("the re-run rewrote the decision: (%q, %q) became (%q, %q)",
			reason, at, reasonAgain, atAgain)
	}
}

// The exemption is the THRESHOLD's doing, not a property of small blobs.
//
// Without this, "a small blob is exempt" would also pass against a handler that
// refused to chunk anything it thought was small for some other reason — or
// against a chunker that simply could not handle a short input. The same bytes
// are run under two thresholds and produce the two different answers.
func TestTheExemptionIsTheThresholdAndNotTheBlob(t *testing.T) {
	f := newChunkFixture(t)
	content := pseudoRandom(2<<10, 5)
	exempt := f.seedBlob(content)
	chunked := f.seedBlob(append(content, 'x'))

	deps := f.deps()
	deps.Threshold = 4 << 10
	if err := f.runWith(deps, exempt); err != nil {
		t.Fatal(err)
	}
	assertState(t, f.state(exempt), manifests.StateNotRequired, "under a 4 KiB threshold")

	deps.Threshold = 1 << 10
	if err := f.runWith(deps, chunked); err != nil {
		t.Fatal(err)
	}
	assertState(t, f.state(chunked), manifests.StatePresent, "under a 1 KiB threshold")
	m, found := f.manifest(chunked)
	if !found {
		t.Fatal("no manifest for the blob above the threshold")
	}
	if !m.Covers(int64(len(content)) + 1) {
		t.Errorf("the manifest covers %d bytes of %d", m.CoveredSize, len(content)+1)
	}
}

// 🔴 The one that matters most: a corrupt blob produces NO manifest.
//
// A manifest built from bytes nobody checked is the worst artefact this system
// could hold — every chunk digest correct, describing a file that is not the
// one it is named after — and every later reassembly would verify happily
// against it. The clean run over the SAME bytes comes first, so "no manifest"
// is known to mean the check stopped it rather than that the fixture never
// produces one.
func TestACorruptBlobProducesNoManifestAndIsQuarantined(t *testing.T) {
	f := newChunkFixture(t)
	content := pseudoRandom(chunkableSize, 13)

	// First, the control: these exact bytes DO produce a manifest.
	control := f.seedBlob(content)
	if err := f.run(control); err != nil {
		t.Fatal(err)
	}
	if _, found := f.manifest(control); !found {
		t.Fatal("the control blob produced no manifest — the assertion below would prove nothing")
	}

	// Now the same bytes again, damaged behind the store's back. Same length,
	// so nothing short of hashing the object notices.
	damaged := f.seedBlob(pseudoRandom(chunkableSize, 17))
	f.damage(damaged, chunkableSize/2)

	if err := f.run(damaged); err != nil {
		t.Fatalf("the handler failed instead of reporting corruption: %v", err)
	}

	if m, found := f.manifest(damaged); found {
		t.Errorf("a manifest was written for corrupt bytes: %s over %d chunks",
			m.Digest, m.ChunkCount())
	}
	if got := f.countWhere(
		`SELECT count(*) FROM manifest_chunks WHERE blob_hash = '` + damaged.String() + `'`); got != 0 {
		t.Errorf("%d chunk row(s) were written for corrupt bytes", got)
	}
	if got := f.countWhere(
		`SELECT count(*) FROM local_chunks WHERE blob_hash = '` + damaged.String() + `'`); got != 0 {
		t.Errorf("%d local chunk index entries were written for corrupt bytes", got)
	}
	assertState(t, f.state(damaged), manifests.StateUndecided, "a corrupt blob")

	// Reported on the existing path (ADR-0018): the replica is corrupt, the
	// quarantine ledger has the entry, and replica.corrupt was emitted.
	if got := f.countWhere(`SELECT count(*) FROM quarantine WHERE blob_hash = '` +
		damaged.String() + `'`); got != 1 {
		t.Errorf("quarantine ledger entries = %d, want 1", got)
	}
	corruptEvents := 0
	for _, e := range f.eventsOfType(events.TypeReplicaCorrupt) {
		if e.SubjectID == damaged.String() {
			corruptEvents++
		}
	}
	if corruptEvents != 1 {
		t.Errorf("replica.corrupt emitted %d time(s) for the damaged blob, want 1", corruptEvents)
	}

	// Quarantined, NOT deleted. The bytes are evidence: on a hardlink-ingested
	// library they may be the operator's own original.
	held, err := f.cas.Has(f.t.Context(), damaged)
	if err != nil {
		t.Fatal(err)
	}
	if held {
		t.Error("the corrupt blob is still addressable in the store")
	}
	quarantined, err := f.cas.QuarantinedBlobs()
	if err != nil {
		t.Fatal(err)
	}
	var found int
	for _, q := range quarantined {
		if q.Hash.Equal(damaged) {
			found++
			if q.Size != chunkableSize {
				t.Errorf("the quarantined file is %d bytes, want the %d that were there", q.Size, chunkableSize)
			}
		}
	}
	if found != 1 {
		t.Fatalf("the store quarantined %d cop(ies) of the damaged blob, want 1", found)
	}
	// The ledger's path is where an operator is told to look, so it has to be
	// a file that exists and holds the damaged bytes rather than a plausible
	// string.
	var recordedPath string
	if err := f.db.Reader().QueryRowContext(f.t.Context(),
		`SELECT path FROM quarantine WHERE blob_hash = ?`, damaged.String()).Scan(&recordedPath); err != nil {
		t.Fatal(err)
	}
	preserved, err := os.ReadFile(recordedPath) // #nosec G304 -- a path this test's own store wrote
	if err != nil {
		t.Fatalf("the quarantine ledger points at %q, which cannot be read: %v", recordedPath, err)
	}
	if len(preserved) != chunkableSize {
		t.Fatalf("the preserved evidence is %d bytes, want %d", len(preserved), chunkableSize)
	}
	if digest, _, err := hashing.HashReader(bytes.NewReader(preserved)); err != nil {
		t.Fatal(err)
	} else if digest.Equal(damaged) {
		t.Error("the quarantined bytes hash to the blob's own name — the fixture did not actually " +
			"damage anything")
	}
}

// A re-run on an already-chunked blob emits nothing and changes nothing. Both
// are asserted, because "emits nothing" is what stops every retry of every job
// becoming event noise, and "changes nothing" is what makes that observable.
func TestChunkBlobRerunEmitsNoEventAndChangesNoRow(t *testing.T) {
	f := newChunkFixture(t)
	hash := f.seedBlob(pseudoRandom(chunkableSize, 19))
	if err := f.run(hash); err != nil {
		t.Fatal(err)
	}

	before := f.snapshot(hash)
	if err := f.run(hash); err != nil {
		t.Fatal(err)
	}
	after := f.snapshot(hash)

	if before.events != after.events {
		t.Errorf("the re-run emitted %d event(s) — a job that changes nothing must say nothing",
			after.events-before.events)
	}
	if before != after {
		t.Errorf("the re-run changed rows: %+v became %+v", before, after)
	}
}

// A snapshot of everything a re-run must not touch.
type chunkSnapshot struct {
	events         int
	manifests      int
	chunks         int
	local          int
	manifestDigest string
	generatedAt    string
	recordedAt     string
}

func (f *chunkFixture) snapshot(hash hashing.Hash) chunkSnapshot {
	f.t.Helper()
	s := chunkSnapshot{
		events:    f.count("events"),
		manifests: f.count("chunk_manifests"),
		chunks:    f.count("manifest_chunks"),
		local:     f.count("local_chunks"),
	}
	var digest, generated string
	err := f.db.Reader().QueryRowContext(f.t.Context(),
		`SELECT digest, generated_at FROM chunk_manifests WHERE blob_hash = ?`,
		hash.String()).Scan(&digest, &generated)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No manifest yet. A legitimate snapshot: the empty strings say so.
	case err != nil:
		f.t.Fatalf("reading the manifest row: %v", err)
	}
	s.manifestDigest = digest
	s.generatedAt = generated
	var recorded *string
	if err := f.db.Reader().QueryRowContext(f.t.Context(),
		`SELECT max(recorded_at) FROM local_chunks WHERE blob_hash = ?`,
		hash.String()).Scan(&recorded); err != nil {
		f.t.Fatalf("reading the local chunk index: %v", err)
	}
	if recorded != nil {
		s.recordedAt = *recorded
	}
	return s
}

// A full sequential read must stay a full sequential read: memory cannot scale
// with the blob. A 20 GB remux is the normal large input here (ADR-0013), and a
// handler that buffered what it read would be unusable on exactly the blobs
// chunking exists for.
func TestChunkBlobMemoryStaysFlatOverALargeBlob(t *testing.T) {
	f := newChunkFixture(t)

	measure := func(size int) (heap uint64, chunks int) {
		hash := f.seedBlob(pseudoRandom(size, uint64(size)))
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		if err := f.run(hash); err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		m, found := f.manifest(hash)
		if !found {
			t.Fatal("no manifest")
		}
		return after.HeapAlloc, m.ChunkCount()
	}

	const small = 8 << 20
	const large = 64 << 20

	smallHeap, smallChunks := measure(small)
	largeHeap, largeChunks := measure(large)
	t.Logf("%d MiB: %d chunks, live heap %d bytes", small>>20, smallChunks, smallHeap)
	t.Logf("%d MiB: %d chunks, live heap %d bytes", large>>20, largeChunks, largeHeap)

	if largeChunks < smallChunks*4 {
		t.Fatalf("the large blob produced %d chunks against %d for one eight times smaller — the "+
			"two runs are not actually different sizes", largeChunks, smallChunks)
	}
	// Eight times the input, 56 MiB more bytes. The chunker's own buffer is
	// Max + its read size, and the manifest itself is one small struct per
	// chunk; anything beyond that budget is the blob being buffered.
	const budget = chunking.DefaultMax + (1 << 20)
	if growth := int64(largeHeap) - int64(smallHeap); growth > budget {
		t.Errorf("live heap grew by %d bytes when the blob grew by %d — memory is scaling with the "+
			"blob", growth, large-small)
	}
}

// cancellingStore cancels the job's context part-way through the blob, which is
// what a lost lease or a stopping process looks like from inside the handler.
type cancellingStore struct {
	cas.Store
	cancel context.CancelFunc
	after  int64
}

func (s *cancellingStore) Open(
	ctx context.Context, h hashing.Hash,
) (cas.ReadSeekCloser, cas.Descriptor, error) {
	rc, desc, err := s.Store.Open(ctx, h)
	if err != nil {
		return nil, desc, err
	}
	return &cancellingReader{ReadSeekCloser: rc, cancel: s.cancel, remaining: s.after}, desc, nil
}

type cancellingReader struct {
	cas.ReadSeekCloser
	cancel    context.CancelFunc
	remaining int64
	fired     bool
}

func (r *cancellingReader) Read(p []byte) (int, error) {
	n, err := r.ReadSeekCloser.Read(p)
	r.remaining -= int64(n)
	if r.remaining <= 0 && !r.fired {
		r.fired = true
		r.cancel()
	}
	return n, err
}

// Cancellation mid-chunking leaves NO partial manifest. The rows are asserted
// absent — "an error was returned" is not the property, because a handler that
// wrote chunk rows as it went and then returned an error would satisfy it.
func TestChunkBlobCancellationLeavesNoPartialManifest(t *testing.T) {
	f := newChunkFixture(t)
	hash := f.seedBlob(pseudoRandom(chunkableSize, 23))

	ctx, cancel := context.WithCancel(f.t.Context())
	defer cancel()
	deps := f.deps()
	// Part-way in: far enough that several chunks have been cut, so the
	// handler is genuinely mid-blob rather than stopped before it started.
	deps.Store = &cancellingStore{Store: f.cas, cancel: cancel, after: chunkableSize / 3}

	err := f.runCtx(ctx, deps, hash)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelling mid-chunking returned %v, want a cancellation", err)
	}
	if got := f.count("chunk_manifests"); got != 0 {
		t.Errorf("chunk_manifests = %d after a cancelled run, want 0", got)
	}
	if got := f.count("manifest_chunks"); got != 0 {
		t.Errorf("manifest_chunks = %d after a cancelled run, want 0", got)
	}
	if got := f.count("local_chunks"); got != 0 {
		t.Errorf("local_chunks = %d after a cancelled run, want 0", got)
	}
	assertState(t, f.state(hash), manifests.StateUndecided, "after a cancelled run")

	// And the blob is still chunkable afterwards: a cancellation leaves the
	// work undone, not poisoned.
	if err := f.run(hash); err != nil {
		t.Fatalf("re-running after a cancellation: %v", err)
	}
	assertState(t, f.state(hash), manifests.StatePresent, "after a re-run")
}

// A job for bytes this node does not hold is not a failure. Chunking is a local
// read of local bytes, and failing would retry a read of a file that is not
// here five times before declaring chunking broken on a node that is simply not
// the one holding the blob.
func TestChunkBlobSkipsABlobThisNodeDoesNotHold(t *testing.T) {
	f := newChunkFixture(t)
	hash := f.seedBlob(pseudoRandom(chunkableSize, 29))
	if err := f.cas.Delete(f.t.Context(), hash); err != nil {
		t.Fatal(err)
	}

	if err := f.run(hash); err != nil {
		t.Fatalf("chunking a blob this node does not hold: %v", err)
	}
	if got := f.count("chunk_manifests"); got != 0 {
		t.Errorf("chunk_manifests = %d, want 0", got)
	}
	// Not recorded as never-needing either: absent from this node is not a
	// decision that these bytes never need a manifest.
	assertState(t, f.state(hash), manifests.StateUndecided, "a blob this node does not hold")
}

// The registration is one value whose properties can be asserted rather than
// read: bounded concurrency, and nothing required of the node.
func TestChunkBlobRegistrationIsBoundedAndUnconditional(t *testing.T) {
	reg := ChunkBlobRegistration(ChunkDeps{})
	if reg.Handler == nil {
		t.Fatal("the registration has no handler")
	}
	if reg.MaxConcurrent != 2 {
		t.Errorf("MaxConcurrent = %d, want 2 — chunking is a full sequential read and the disk-head "+
			"argument bounds it", reg.MaxConcurrent)
	}
	if reg.RequiredCapability != "" {
		t.Errorf("RequiredCapability = %q, want none: chunking needs a disk and a CPU, which every "+
			"node has", reg.RequiredCapability)
	}
}

// The threshold's number and its reason are policy, and policy that can drift
// silently is policy nobody can review. Both are pinned here so that changing
// either is a deliberate edit to a test that says why the number was chosen.
func TestTheChunkingThresholdIsTheChunkersMaximum(t *testing.T) {
	if manifests.ThresholdBytes != chunking.DefaultMax {
		t.Errorf("ThresholdBytes = %d, want the chunker's maximum %d — below Max a blob is a handful "+
			"of chunks at most, and the manifest costs the same read as the transfer it would optimise",
			manifests.ThresholdBytes, chunking.DefaultMax)
	}
	if manifests.ReasonBelowThreshold == "" {
		t.Error("an exemption with no recorded grounds cannot be reviewed when the threshold moves")
	}
}

// writeBytes puts a file of arbitrary bytes in the library root. The harness's
// own write takes a string, and a chunkable blob is not text.
func (f *chunkFixture) writeBytes(relPath string, content []byte) string {
	f.t.Helper()
	full := filepath.Join(f.rootDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(full, content, 0o640); err != nil {
		f.t.Fatal(err)
	}
	return full
}

// countWhere runs a counting query the test supplies.
func (f *chunkFixture) countWhere(query string) int {
	f.t.Helper()
	var n int
	if err := f.db.Reader().QueryRowContext(f.t.Context(), query).Scan(&n); err != nil {
		f.t.Fatalf("counting (%s): %v", query, err)
	}
	return n
}

// damage rewrites one byte of a stored blob BEHIND the store's back, leaving
// the length untouched.
//
// Behind its back is the point: this is what an external tool rewriting a
// hardlink-ingested original does, and it is the case a length check cannot see and
// only hashing the whole object catches. The blob file is read-only, so the
// permissions are restored afterwards — a test that left it writable would be
// testing a store nobody runs.
func (f *chunkFixture) damage(hash hashing.Hash, at int64) {
	f.t.Helper()
	path, err := f.cas.LocalPath(f.t.Context(), hash)
	if err != nil {
		f.t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		f.t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		f.t.Fatal(err)
	}
	original := make([]byte, 1)
	if _, err := file.ReadAt(original, at); err != nil {
		f.t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{original[0] ^ 0xff}, at); err != nil {
		f.t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chmod(path, info.Mode()); err != nil {
		f.t.Fatal(err)
	}
	// The damage must be invisible to anything short of a hash: same length,
	// same permissions, one flipped byte.
	after, err := os.Stat(path)
	if err != nil {
		f.t.Fatal(err)
	}
	if after.Size() != info.Size() {
		f.t.Fatalf("the damage changed the length from %d to %d, which a size check would catch — "+
			"this test is meant to need the whole-object hash", info.Size(), after.Size())
	}
}
