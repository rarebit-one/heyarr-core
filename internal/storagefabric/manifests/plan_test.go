package manifests_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/chunking"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// M5-07's reuse decision, asserted as arithmetic (§20, ADR-0034).
//
// The planner is pure, so every assertion here is about a value rather than
// about a transfer. What it decides is re-verified against the manifest at
// execution time — the plan is a claim and the transfer package's tests are
// where claims meet bytes.

// planFixture builds a manifest whose chunk digests are digests of real bytes
// and are NOT in index order.
//
// The ordering check is the point. A fixture that numbered its digests
// monotonically would make index order and digest order coincide, and an
// implementation that sorted, reordered or keyed by digest would satisfy every
// ordering assertion below by coincidence — which is how one sabotage passed
// on this milestone already.
func planFixture(t *testing.T, lengths ...int64) (manifests.Manifest, [][]byte) {
	t.Helper()
	var (
		chunks  []chunking.Chunk
		content [][]byte
		off     int64
		whole   = hashing.New()
	)
	for i, n := range lengths {
		body := bytes.Repeat([]byte{byte('A' + (i*7)%26), byte(i)}, int(n/2))
		if int64(len(body)) != n {
			t.Fatalf("fixture chunk %d is %d bytes, wanted %d", i, len(body), n)
		}
		h := hashing.New()
		_, _ = h.Write(body)
		chunks = append(chunks, chunking.Chunk{Offset: off, Length: n, Digest: h.Sum()})
		_, _ = whole.Write(body)
		content = append(content, body)
		off += n
	}
	ascending, descending := true, true
	for i := 1; i < len(chunks); i++ {
		if chunks[i].Digest.Hex() <= chunks[i-1].Digest.Hex() {
			ascending = false
		}
		if chunks[i].Digest.Hex() >= chunks[i-1].Digest.Hex() {
			descending = false
		}
	}
	if len(chunks) > 2 && (ascending || descending) {
		t.Fatal("the fixture's chunk digests are sorted, so index order and digest order coincide " +
			"and an implementation that reordered would still satisfy every assertion here")
	}
	m, err := manifests.Build(whole.Sum(), chunking.DefaultConfig(), chunks,
		time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return m, content
}

// With nothing kept and nothing held, every chunk is fetched. The control for
// everything below: a planner that returned "reuse" for an empty index would
// pass a saving assertion by fetching nothing at all.
func TestAPlanWithNoPrefixAndNoIndexFetchesEverything(t *testing.T) {
	m, _ := planFixture(t, 1024, 2048, 512, 4096)

	plan := manifests.PlanFor(m, 0, nil)

	if len(plan.Entries) != len(m.Chunks) {
		t.Fatalf("the plan has %d entries and the manifest has %d chunks",
			len(plan.Entries), len(m.Chunks))
	}
	for i, e := range plan.Entries {
		// assert_eq on the enum, never a substring test.
		if e.Availability != manifests.AvailabilityFetch {
			t.Errorf("chunk %d is %q, want %q", i, e.Availability, manifests.AvailabilityFetch)
		}
	}
	stats := plan.Stats()
	if stats.ChunksFetch != 4 || stats.ChunksKept != 0 || stats.ChunksLocal != 0 {
		t.Errorf("stats = %+v, want four fetched and nothing else", stats)
	}
	if stats.BytesFetch != m.CoveredSize {
		t.Errorf("bytes to fetch = %d, the blob is %d", stats.BytesFetch, m.CoveredSize)
	}
}

// A verified prefix is kept and the rest is not, and the boundary is a chunk
// count rather than a byte offset — ADR-0035's refusal, expressed in the type.
func TestAVerifiedPrefixIsKeptAndTheRestIsNot(t *testing.T) {
	m, _ := planFixture(t, 1024, 2048, 512, 4096)

	plan := manifests.PlanFor(m, 2, nil)

	want := []manifests.Availability{
		manifests.AvailabilityKept, manifests.AvailabilityKept,
		manifests.AvailabilityFetch, manifests.AvailabilityFetch,
	}
	for i, e := range plan.Entries {
		if e.Availability != want[i] {
			t.Errorf("chunk %d is %q, want %q", i, e.Availability, want[i])
		}
	}
	stats := plan.Stats()
	if stats.BytesKept != 1024+2048 {
		t.Errorf("kept bytes = %d, want %d", stats.BytesKept, 1024+2048)
	}
	if stats.BytesFetch != 512+4096 {
		t.Errorf("bytes to fetch = %d, want %d", stats.BytesFetch, 512+4096)
	}
}

// A prefix count outside the manifest is clamped rather than panicking or
// believed. It arrives from a scan of a file, and a file is the thing this
// milestone assumes is wrong.
func TestAPrefixCountOutsideTheManifestIsClamped(t *testing.T) {
	m, _ := planFixture(t, 1024, 2048)

	for _, kept := range []int{-5, 99} {
		plan := manifests.PlanFor(m, kept, nil)
		if len(plan.Entries) != 2 {
			t.Fatalf("kept=%d produced %d entries", kept, len(plan.Entries))
		}
		want := manifests.AvailabilityFetch
		if kept > 0 {
			want = manifests.AvailabilityKept
		}
		for i, e := range plan.Entries {
			if e.Availability != want {
				t.Errorf("kept=%d: chunk %d is %q, want %q", kept, i, e.Availability, want)
			}
		}
	}
}

// A chunk this node holds elsewhere is planned as a local read, and the plan
// records WHERE — a blob, an offset, a length — rather than resolving the
// chunk to an identity (ADR-0034).
func TestAHeldChunkIsPlannedAsALocalReadFromWhereItIs(t *testing.T) {
	m, _ := planFixture(t, 1024, 2048, 512)
	donorBlob := hashing.MustParse("blake3:" + strings.Repeat("11", 32))
	held := map[hashing.Hash][]manifests.LocalChunk{
		m.Chunks[1].Digest: {{
			Digest: m.Chunks[1].Digest, BlobHash: donorBlob, Offset: 7777, Length: 2048,
		}},
	}

	plan := manifests.PlanFor(m, 0, held)

	if got := plan.Entries[1].Availability; got != manifests.AvailabilityLocal {
		t.Fatalf("chunk 1 is %q, want %q", got, manifests.AvailabilityLocal)
	}
	if plan.Entries[1].Donor.Offset != 7777 || !plan.Entries[1].Donor.BlobHash.Equal(donorBlob) {
		t.Errorf("the donor is %s at %d, want %s at 7777",
			plan.Entries[1].Donor.BlobHash, plan.Entries[1].Donor.Offset, donorBlob)
	}
	for _, i := range []int{0, 2} {
		if got := plan.Entries[i].Availability; got != manifests.AvailabilityFetch {
			t.Errorf("chunk %d is %q, want %q", i, got, manifests.AvailabilityFetch)
		}
	}
	stats := plan.Stats()
	if stats.ChunksLocal != 1 || stats.BytesLocal != 2048 {
		t.Errorf("stats = %+v, want one local chunk of 2048 bytes", stats)
	}
	if stats.BytesFetch != 1024+512 {
		t.Errorf("bytes to fetch = %d, want %d", stats.BytesFetch, 1024+512)
	}
}

// An index entry that does not describe the chunk it is filed under is not
// scheduled. Not the integrity check — that is re-hashing at execution — but a
// plan declining to schedule a read it can already see describes something
// else.
func TestAnIndexEntryThatDoesNotDescribeTheChunkIsNotScheduled(t *testing.T) {
	m, _ := planFixture(t, 1024, 2048, 512)
	donorBlob := hashing.MustParse("blake3:" + strings.Repeat("22", 32))

	tests := map[string]manifests.LocalChunk{
		"a length that disagrees with the manifest": {
			Digest: m.Chunks[1].Digest, BlobHash: donorBlob, Offset: 0, Length: 2047,
		},
		"a negative offset": {
			Digest: m.Chunks[1].Digest, BlobHash: donorBlob, Offset: -1, Length: 2048,
		},
		"no blob at all": {
			Digest: m.Chunks[1].Digest, Offset: 0, Length: 2048,
		},
		"a digest that is not the one it is filed under": {
			Digest: m.Chunks[0].Digest, BlobHash: donorBlob, Offset: 0, Length: 2048,
		},
	}
	for name, entry := range tests {
		t.Run(name, func(t *testing.T) {
			held := map[hashing.Hash][]manifests.LocalChunk{m.Chunks[1].Digest: {entry}}
			plan := manifests.PlanFor(m, 0, held)
			if got := plan.Entries[1].Availability; got != manifests.AvailabilityFetch {
				t.Errorf("chunk 1 is %q, want %q", got, manifests.AvailabilityFetch)
			}
		})
	}
}

// The first usable candidate wins, deterministically, so that two runs of the
// same plan schedule the same reads.
func TestTheFirstUsableDonorIsChosen(t *testing.T) {
	m, _ := planFixture(t, 1024, 2048)
	first := hashing.MustParse("blake3:" + strings.Repeat("33", 32))
	second := hashing.MustParse("blake3:" + strings.Repeat("44", 32))
	held := map[hashing.Hash][]manifests.LocalChunk{
		m.Chunks[0].Digest: {
			{Digest: m.Chunks[0].Digest, BlobHash: first, Offset: 100, Length: 999},
			{Digest: m.Chunks[0].Digest, BlobHash: first, Offset: 200, Length: 1024},
			{Digest: m.Chunks[0].Digest, BlobHash: second, Offset: 300, Length: 1024},
		},
	}

	plan := manifests.PlanFor(m, 0, held)

	if got := plan.Entries[0].Donor.Offset; got != 200 {
		t.Errorf("the donor offset is %d, want 200 — the first candidate whose length agrees", got)
	}
	if !plan.Entries[0].Donor.BlobHash.Equal(first) {
		t.Errorf("the donor blob is %s, want %s", plan.Entries[0].Donor.BlobHash, first)
	}
}

// A kept chunk is never demoted to a local read even when the index also holds
// it: the bytes are already in the right place and reading them again would be
// work with no result.
func TestAKeptChunkIsNotRePlannedAsALocalRead(t *testing.T) {
	m, _ := planFixture(t, 1024, 2048)
	held := map[hashing.Hash][]manifests.LocalChunk{
		m.Chunks[0].Digest: {{
			Digest: m.Chunks[0].Digest, BlobHash: hashing.MustParse("blake3:" + strings.Repeat("55", 32)),
			Offset: 0, Length: 1024,
		}},
	}

	plan := manifests.PlanFor(m, 1, held)

	if got := plan.Entries[0].Availability; got != manifests.AvailabilityKept {
		t.Errorf("chunk 0 is %q, want %q", got, manifests.AvailabilityKept)
	}
}

// A plan is a sequence and Validate says so: it refuses a plan built for
// another manifest, one of another length, and one whose entries have been
// reordered.
func TestValidateRefusesAPlanThatIsNotThisManifests(t *testing.T) {
	m, _ := planFixture(t, 1024, 2048, 512)
	other, _ := planFixture(t, 4096, 4096)

	if err := manifests.PlanFor(m, 0, nil).Validate(m); err != nil {
		t.Fatalf("a plan built from this manifest was refused: %v", err)
	}
	if err := manifests.PlanFor(other, 0, nil).Validate(m); err == nil {
		t.Error("a plan for another blob was accepted")
	}

	reordered := manifests.PlanFor(m, 0, nil)
	reordered.Entries[0], reordered.Entries[2] = reordered.Entries[2], reordered.Entries[0]
	if err := reordered.Validate(m); err == nil {
		t.Error("a reordered plan was accepted — the order of a manifest is the data, and a set " +
			"of valid chunks in the wrong order is the wrong file")
	}

	short := manifests.PlanFor(m, 0, nil)
	short.Entries = short.Entries[:2]
	if err := short.Validate(m); err == nil {
		t.Error("a plan with fewer entries than the manifest has chunks was accepted")
	}
}

// The stats are the number the feature exists to produce, so they are asserted
// against arithmetic done here rather than against the planner's own totals.
func TestStatsTotalWhatThePlanSays(t *testing.T) {
	m, _ := planFixture(t, 1000, 2000, 3000, 4000)
	donor := hashing.MustParse("blake3:" + strings.Repeat("66", 32))
	held := map[hashing.Hash][]manifests.LocalChunk{
		m.Chunks[2].Digest: {{Digest: m.Chunks[2].Digest, BlobHash: donor, Offset: 0, Length: 3000}},
	}

	stats := manifests.PlanFor(m, 1, held).Stats()

	// Written out rather than derived from the manifest, so that a planner
	// that mis-sorted its categories cannot agree with an expectation computed
	// the same wrong way.
	if stats.ChunksKept != 1 || stats.BytesKept != 1000 {
		t.Errorf("kept = %d chunks / %d bytes, want 1 / 1000", stats.ChunksKept, stats.BytesKept)
	}
	if stats.ChunksLocal != 1 || stats.BytesLocal != 3000 {
		t.Errorf("local = %d chunks / %d bytes, want 1 / 3000", stats.ChunksLocal, stats.BytesLocal)
	}
	if stats.ChunksFetch != 2 || stats.BytesFetch != 6000 {
		t.Errorf("fetch = %d chunks / %d bytes, want 2 / 6000", stats.ChunksFetch, stats.BytesFetch)
	}
	if total := stats.BytesKept + stats.BytesLocal + stats.BytesFetch; total != m.CoveredSize {
		t.Errorf("the three categories total %d bytes and the blob is %d — a chunk is missing "+
			"from the plan or counted twice", total, m.CoveredSize)
	}
}
