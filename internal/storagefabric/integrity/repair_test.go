package integrity_test

import (
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
)

// The baseline, and it comes first deliberately. Without it every assertion
// below this one also passes on a repairer that rewrites every blob it is
// handed: the repaired file would hash correctly, the peer would have been
// asked, and the store would look fine. What tells the two apart is that a
// healthy blob costs nothing — no staging file, no quarantine artefact, no
// publish, and not one byte fetched.
func TestRepairLeavesAHealthyBlobExactlyAsItWas(t *testing.T) {
	f := newRepairFixture(t)
	content := pseudoRandom(1, 8<<10)
	h := f.putChunked(content)

	before := treeSnapshot(t, f.store.Root())
	result, err := f.repairer().Repair(t.Context(), h)
	if err != nil {
		t.Fatalf("repairing a healthy blob: %v", err)
	}

	if result.Outcome != integrity.OutcomeHealthy {
		t.Errorf("outcome = %q, want %q", result.Outcome, integrity.OutcomeHealthy)
	}
	if result.ChunksDamaged != 0 {
		t.Errorf("chunks damaged = %d, want 0", result.ChunksDamaged)
	}
	if result.BytesFetched != 0 || f.peer.calls != 0 {
		t.Errorf("a healthy blob cost %d bytes over %d peer calls, want 0 and 0",
			result.BytesFetched, f.peer.calls)
	}
	if len(f.quarantinedFiles()) != 0 {
		t.Errorf("a healthy blob was quarantined: %v", f.quarantinedFiles())
	}
	temps, err := f.store.TempFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Errorf("a healthy blob left %d staging files, want 0", len(temps))
	}
	assertSameTree(t, before, treeSnapshot(t, f.store.Root()))
}

// The headline: damage a region, repair it, and the blob is its own name again
// — with the fetch scoped to the damage rather than to the blob.
func TestRepairReplacesTheDamagedChunksAndNothingElse(t *testing.T) {
	f := newRepairFixture(t)
	content := pseudoRandom(2, 64<<10)
	h := f.putChunked(content)
	m, _, err := f.man.Load(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	if m.ChunkCount() < 8 {
		t.Fatalf("fixture produced %d chunks, too few to say anything about proportionality",
			m.ChunkCount())
	}

	// Damage the middle of one chunk, at the same length: the fault a shallow
	// check cannot see.
	target := m.Chunks[m.ChunkCount()/2]
	path := f.blobFile(h)
	damagedRegion := make([]byte, 32)
	for i := range damagedRegion {
		damagedRegion[i] = 0x5A
	}
	damageFile(t, path, target.Offset+8, damagedRegion)
	damagedSum := sha256File(t, path)

	result, err := f.repairer().Repair(t.Context(), h)
	if err != nil {
		t.Fatalf("repairing: %v", err)
	}

	if result.Outcome != integrity.OutcomeRepaired {
		t.Fatalf("outcome = %q (%s), want %q", result.Outcome, result.Detail, integrity.OutcomeRepaired)
	}
	if result.ChunksDamaged != 1 || result.ChunksFetched != 1 {
		t.Errorf("damaged/fetched = %d/%d, want 1/1", result.ChunksDamaged, result.ChunksFetched)
	}

	// The repaired blob is the original bytes, asserted with a hash that is
	// not the one the code under test uses.
	if got := sha256File(t, f.blobFile(h)); got != sha256Bytes(content) {
		t.Errorf("the repaired blob is not the original bytes")
	}
	if err := f.store.Verify(t.Context(), h); err != nil {
		t.Errorf("the repaired blob does not verify: %v", err)
	}

	// The number the feature exists for. 64 KiB of blob, one damaged chunk:
	// the fetch must be a small fraction of the blob, not all of it.
	const maxFraction = 0.1
	if float64(result.BytesFetched) >= float64(result.BlobSize)*maxFraction {
		t.Errorf("fetched %d bytes of a %d byte blob to repair one chunk — that is not "+
			"proportional to the damage (ADR-0036)", result.BytesFetched, result.BlobSize)
	}
	if result.BytesFetched != target.Length {
		t.Errorf("fetched %d bytes, want exactly the one damaged chunk's %d",
			result.BytesFetched, target.Length)
	}
	t.Logf("repair of one damaged chunk in a %d byte blob fetched %d bytes (%.2f%%) over %d peer calls",
		result.BlobSize, result.BytesFetched,
		100*float64(result.BytesFetched)/float64(result.BlobSize), f.peer.calls)

	// The evidence: the damaged original, preserved before publication, still
	// byte-for-byte what was on disk (ADR-0018).
	quarantined := f.quarantinedFiles()
	if len(quarantined) != 1 {
		t.Fatalf("quarantine holds %d artefacts, want 1", len(quarantined))
	}
	if got := sha256File(t, quarantined[0]); got != damagedSum {
		t.Errorf("the quarantined artefact is not the damaged bytes that were on disk")
	}
	if result.QuarantinePath != quarantined[0] {
		t.Errorf("result names %q, the artefact is at %q", result.QuarantinePath, quarantined[0])
	}
	if _, err := os.ReadFile(quarantined[0]); err != nil { // #nosec G304 -- a test path
		t.Errorf("the quarantined evidence is not readable: %v", err)
	}

	// Nothing was left staged.
	temps, err := f.store.TempFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Errorf("the repair left %d staging files behind, want 0", len(temps))
	}
}

// A repair that cannot find a source changes nothing. Under ADR-0038 that is
// an ordinary outcome — this node cannot reach anywhere that holds the bytes —
// and the assertion is byte-identity with the damaged file, not mere presence:
// "still there" is also true of a file the repairer half-rewrote.
func TestRepairWithNoReachablePeerLeavesTheDamagedBytesByteIdentical(t *testing.T) {
	f := newRepairFixture(t)
	content := pseudoRandom(3, 16<<10)
	h := f.putChunked(content)
	// The peer forgets it ever held these bytes.
	delete(f.peer.bytesByBlob, h.String())

	path := f.blobFile(h)
	damageFile(t, path, 512, []byte("no peer will replace this"))
	damagedSum := sha256File(t, path)
	before := treeSnapshot(t, f.store.Root())

	result, err := f.repairer().Repair(t.Context(), h)
	if err != nil {
		t.Fatalf("an unreachable peer is not an error: %v", err)
	}
	if result.Outcome != integrity.OutcomeUnreachable {
		t.Errorf("outcome = %q, want %q", result.Outcome, integrity.OutcomeUnreachable)
	}
	if result.Detail == "" {
		t.Error("a repair that could not complete must say why")
	}
	if got := sha256File(t, path); got != damagedSum {
		t.Error("the damaged blob is not byte-identical to its damaged self")
	}
	if len(f.quarantinedFiles()) != 0 {
		t.Error("a repair that did not happen quarantined the original anyway")
	}
	assertSameTree(t, before, treeSnapshot(t, f.store.Root()))
}

// The peer's copy is damaged too. This is where a repairer that trusted its
// source writes garbage over a file that was still recoverable.
func TestRepairAbandonsWhenThePeersChunksDoNotVerify(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*fakePeer)
	}{
		{"corrupt bytes", func(p *fakePeer) { p.corrupt = true }},
		{"short chunk", func(p *fakePeer) { p.short = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepairFixture(t)
			content := pseudoRandom(4, 16<<10)
			h := f.putChunked(content)
			tc.apply(f.peer)

			path := f.blobFile(h)
			damageFile(t, path, 1024, []byte("damaged locally as well"))
			damagedSum := sha256File(t, path)
			before := treeSnapshot(t, f.store.Root())

			result, err := f.repairer().Repair(t.Context(), h)
			if err != nil {
				t.Fatalf("a damaged source is an outcome, not an error: %v", err)
			}
			if result.Outcome != integrity.OutcomeSourceCorrupt {
				t.Errorf("outcome = %q (%s), want %q",
					result.Outcome, result.Detail, integrity.OutcomeSourceCorrupt)
			}
			if got := sha256File(t, path); got != damagedSum {
				t.Error("the local original was overwritten with the peer's corruption")
			}
			if len(f.quarantinedFiles()) != 0 {
				t.Error("a repair that was abandoned quarantined the original anyway")
			}
			assertSameTree(t, before, treeSnapshot(t, f.store.Root()))
		})
	}
}

// A complete reconstruction that does not hash to the blob's name publishes
// nothing. Every chunk verified against the manifest and the whole did not,
// which means the manifest describes different bytes than the name does —
// exactly the case ADR-0034 says a manifest is never allowed to settle.
func TestRepairRefusesAnAssemblyThatDoesNotHashToTheBlob(t *testing.T) {
	f := newRepairFixture(t)
	content := pseudoRandom(5, 16<<10)
	h := f.putChunked(content)

	// A manifest that names this blob but describes somebody else's bytes,
	// and a peer holding those bytes. Nothing in the chunk-level checks can
	// catch this; only the whole-object digest can (ADR-0005).
	other := pseudoRandom(6, 16<<10)
	wrong := buildManifest(t, h, other)
	if err := f.man.Save(t.Context(), wrong); err != nil {
		t.Fatal(err)
	}
	f.peer.hold(h, other)

	path := f.blobFile(h)
	damagedSum := sha256File(t, path)
	before := treeSnapshot(t, f.store.Root())

	result, err := f.repairer().Repair(t.Context(), h)
	if err != nil {
		t.Fatalf("a mismatched assembly is an outcome, not an error: %v", err)
	}
	if result.Outcome != integrity.OutcomeAssemblyMismatch {
		t.Fatalf("outcome = %q (%s), want %q",
			result.Outcome, result.Detail, integrity.OutcomeAssemblyMismatch)
	}
	if got := sha256File(t, path); got != damagedSum {
		t.Error("the original was replaced by an assembly that does not hash to its name")
	}
	if len(f.quarantinedFiles()) != 0 {
		t.Error("the original was quarantined for a repair that never published")
	}
	assertSameTree(t, before, treeSnapshot(t, f.store.Root()))
}

// A blob with no manifest is reported as unrepairable by this path, and
// deliberately so: the remedy is a whole re-pull, which is replication's job.
// Asserted rather than assumed, because "repair generates the manifest it
// needs" is the obvious wrong turn here (ADR-0034).
func TestRepairOfABlobWithNoManifestIsReportedNotImprovised(t *testing.T) {
	f := newRepairFixture(t)
	content := pseudoRandom(7, 4<<10)
	h := f.putChunked(content)
	if err := f.man.Discard(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	damageFile(t, f.blobFile(h), 100, []byte("damaged"))
	before := treeSnapshot(t, f.store.Root())

	result, err := f.repairer().Repair(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != integrity.OutcomeNoManifest {
		t.Errorf("outcome = %q, want %q", result.Outcome, integrity.OutcomeNoManifest)
	}
	if !strings.Contains(result.Detail, "re-pull") {
		t.Errorf("the detail does not say what the remedy is: %q", result.Detail)
	}
	if state, _ := f.man.StateOf(t.Context(), h); state == "present" {
		t.Error("repair produced the manifest whose absence it was reporting")
	}
	assertSameTree(t, before, treeSnapshot(t, f.store.Root()))
}

// Nothing local to repair at all: not an addressable blob, not a quarantined
// artefact. An answer, not an error.
func TestRepairWithNothingLocalToRepairSaysSo(t *testing.T) {
	f := newRepairFixture(t)
	content := pseudoRandom(8, 4<<10)
	h := f.putChunked(content)
	if err := f.store.Delete(t.Context(), h); err != nil {
		t.Fatal(err)
	}

	result, err := f.repairer().Repair(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != integrity.OutcomeNoLocalBytes {
		t.Errorf("outcome = %q, want %q", result.Outcome, integrity.OutcomeNoLocalBytes)
	}
}

// The path fsck actually takes: the checker finds corruption and quarantines
// it (ADR-0018), so by the time a repair runs the damaged bytes are no longer
// at the blob path. They are still the only local source of the chunks that
// were never damaged, and a repair that ignored them would refetch the whole
// blob to replace one chunk.
func TestRepairReadsIntactChunksFromAnAlreadyQuarantinedOriginal(t *testing.T) {
	f := newRepairFixture(t)
	content := pseudoRandom(9, 32<<10)
	h := f.putChunked(content)
	m, _, err := f.man.Load(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	target := m.Chunks[2]
	damageFile(t, f.blobFile(h), target.Offset+4, []byte("locally damaged region"))
	damagedSum := sha256File(t, f.blobFile(h))

	// The checker quarantines it, exactly as `fsck --deep` does.
	checker, err := integrity.NewChecker(integrity.Options{Store: f.store, Catalog: f.cat, Clock: f.clock})
	if err != nil {
		t.Fatal(err)
	}
	finding, err := checker.VerifyBlob(t.Context(), h)
	if err != nil {
		t.Fatal(err)
	}
	if finding.Kind != integrity.KindCorrupt {
		t.Fatalf("finding kind = %q, want %q", finding.Kind, integrity.KindCorrupt)
	}
	if has, _ := f.store.Has(t.Context(), h); has {
		t.Fatal("the checker left the corrupt blob addressable")
	}

	result, err := f.repairer().Repair(t.Context(), h)
	if err != nil {
		t.Fatalf("repairing from quarantine: %v", err)
	}
	if result.Outcome != integrity.OutcomeRepaired {
		t.Fatalf("outcome = %q (%s), want %q", result.Outcome, result.Detail, integrity.OutcomeRepaired)
	}
	if result.ChunksFetched != 1 {
		t.Errorf("fetched %d chunks from the peer, want 1 — the intact chunks were on disk",
			result.ChunksFetched)
	}
	if got := sha256File(t, f.blobFile(h)); got != sha256Bytes(content) {
		t.Error("the repaired blob is not the original bytes")
	}
	// The evidence survived the repair rather than being consumed by it.
	quarantined := f.quarantinedFiles()
	if len(quarantined) != 1 {
		t.Fatalf("quarantine holds %d artefacts, want the 1 the checker made", len(quarantined))
	}
	if got := sha256File(t, quarantined[0]); got != damagedSum {
		t.Error("the quarantined evidence is no longer the damaged bytes")
	}
}

// Invariant 1, stated as a property, in the shape M4-12's garbage-collection
// property test is written in: whatever the damage and whatever the peer does,
// no repair ever leaves the store holding bytes that answer to a digest they
// do not have — and the blob is only ever exactly its damaged self or exactly
// its repaired self, never anything in between.
func TestRepairNeverLeavesBytesUnderADigestTheyDoNotHave(t *testing.T) {
	const cases = 60
	rng := rand.New(rand.NewSource(20260823)) // #nosec G404 -- deterministic test input

	for i := range cases {
		f := newRepairFixture(t)
		size := 4<<10 + rng.Intn(28<<10)
		content := pseudoRandom(int64(1000+i), size)
		h := f.putChunked(content)
		m, _, err := f.man.Load(t.Context(), h)
		if err != nil {
			t.Fatal(err)
		}

		// A randomised peer: healthy, absent, corrupt or short.
		switch rng.Intn(4) {
		case 1:
			delete(f.peer.bytesByBlob, h.String())
		case 2:
			f.peer.corrupt = true
		case 3:
			f.peer.short = true
		}

		// A randomised damage pattern: between one and four regions, each
		// inside a different chunk, at the same length.
		path := f.blobFile(h)
		regions := 1 + rng.Intn(4)
		for r := 0; r < regions && r < m.ChunkCount(); r++ {
			chunk := m.Chunks[rng.Intn(m.ChunkCount())]
			at := chunk.Offset + int64(rng.Intn(int(chunk.Length)))
			damageFile(t, path, at, []byte{byte(rng.Intn(256)), byte(rng.Intn(256))})
		}
		damagedSum := sha256File(t, path)
		goodSum := sha256Bytes(content)

		result, err := f.repairer().Repair(t.Context(), h)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}

		// The property: every addressable file in the store hashes to its own
		// name, and the ONE file that does not is the pre-existing damage the
		// test wrote, byte for byte. Nothing else is asserted about the
		// outcome — a repair that refused and one that succeeded must both
		// satisfy it.
		//
		// The exemption is stated as a sha256 rather than as a path, so a
		// repairer that rewrote the damaged file into some other set of
		// non-matching bytes fails here instead of being waved through as
		// "the damage we already knew about".
		assertNothingAnswersToADigestItDoesNotHave(t, f, damagedSum)

		// And the corollary: the blob is one of exactly two files.
		blobPath := f.blobFile(h)
		switch {
		case blobPath == "" && result.Outcome == integrity.OutcomeRepaired:
			t.Fatalf("case %d: reported repaired and the blob is gone", i)
		case blobPath == "":
			// Quarantined by a repair that then could not complete: the
			// damaged bytes are evidence and the blob is missing, which
			// replication fixes. Not a state this path reaches today, but it
			// is a legal one (ADR-0036).
		default:
			got := sha256File(t, blobPath)
			if got != damagedSum && got != goodSum {
				t.Fatalf("case %d: the blob is neither its damaged self nor its repaired self "+
					"(outcome %q)", i, result.Outcome)
			}
			if result.Outcome == integrity.OutcomeRepaired && got != goodSum {
				t.Fatalf("case %d: reported repaired, holds the damaged bytes", i)
			}
			if result.Outcome != integrity.OutcomeRepaired && got != damagedSum {
				t.Fatalf("case %d: outcome %q did not repair, yet the bytes changed",
					i, result.Outcome)
			}
		}
	}
}

// assertNothingAnswersToADigestItDoesNotHave walks the addressable tree and
// re-hashes everything in it.
//
// exemptSum is the sha256 of the damage the test itself wrote before the
// repair ran: a file that is still exactly those bytes was left alone, which
// is the correct outcome for every repair that could not complete. Any OTHER
// file whose contents do not match its name was produced by the repairer, and
// that is the property being tested.
//
// Quarantine and tmp/ are deliberately not walked: neither is addressable, and
// bytes that do not match their name are exactly what both exist to hold.
func assertNothingAnswersToADigestItDoesNotHave(t *testing.T, f *repairFixture, exemptSum string) {
	t.Helper()
	base := filepath.Join(f.store.Root(), "blobs")
	err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		named, pErr := hashing.Parse("blake3:" + d.Name())
		if pErr != nil {
			t.Errorf("a file in the addressable tree is not named by a digest: %s", path)
			return nil
		}
		got, _, hErr := hashing.HashFile(path)
		if hErr != nil {
			return hErr
		}
		if !got.Equal(named) {
			if sha256File(t, path) == exemptSum {
				// Untouched pre-existing damage. That is the premise, not a
				// violation.
				return nil
			}
			t.Errorf("%s holds bytes that hash to %s — a name that does not describe its "+
				"contents is the one thing content addressing exists to make impossible", path, got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
