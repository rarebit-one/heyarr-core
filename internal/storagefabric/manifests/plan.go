package manifests

import (
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/chunking"
)

// The destination's reuse decision (§20, M5-06, M5-07, ADR-0034, ADR-0035).
//
// # It is a decision, not a negotiation
//
// Everything here is the DESTINATION reasoning about its own disk. The inputs
// are a manifest it fetched and verified itself, how much of its own partial
// staging file it has re-verified, and its own chunk index. The source is
// never consulted, never told what this node holds, and never computes a
// difference — it is asked for byte ranges afterwards and nothing else
// (ADR-0030).
//
// # It is pure, and that is what makes it assertable
//
// No disk, no database, no HTTP. The plan is a value: given the same manifest,
// the same prefix and the same index, it is the same plan, so the reuse
// arithmetic can be tested as arithmetic rather than as a transfer. The
// transfer loop's job is to CARRY OUT a plan and re-verify every byte it acts
// on; deciding and trusting are deliberately different packages.
//
// # A plan is a claim, and a claim is not bytes
//
// [AvailabilityLocal] says the index believes these bytes are at that offset
// in that blob. The index is a record of what this disk looked like when it
// was written, and this milestone's premise is that records and bytes diverge:
// a blob may have been re-materialised, damaged, or quarantined since. So
// every locally sourced chunk is re-hashed against the manifest entry that
// named it before a single byte of it is used, and a chunk that fails is
// fetched from the source instead. The plan makes that check possible; it does
// not perform it and must never be read as having performed it.

// Availability is where one chunk of a transfer is coming from.
//
// A string enum, compared with equality everywhere — see [State] for why this
// package spells its enums so that none is a substring of another.
type Availability string

const (
	// AvailabilityKept — already on disk in this transfer's own staging file,
	// inside the contiguous prefix the destination re-hashed against this
	// manifest. Nothing moves for it and nothing is read for it again.
	AvailabilityKept Availability = "kept"
	// AvailabilityLocal — this node's chunk index claims to hold these bytes
	// inside some other blob it already has. They are read locally and
	// re-verified against the manifest before use; a mismatch demotes the
	// chunk to AvailabilityFetch at execution time, which is a fact about
	// bytes and therefore not something a pure plan can know.
	AvailabilityLocal Availability = "local"
	// AvailabilityFetch — the source is asked for this byte range.
	AvailabilityFetch Availability = "fetch"
)

// Valid reports whether a is one of the three.
func (a Availability) Valid() bool {
	switch a {
	case AvailabilityKept, AvailabilityLocal, AvailabilityFetch:
		return true
	default:
		return false
	}
}

func (a Availability) String() string { return string(a) }

// PlanEntry is one chunk of a manifest and where it is coming from.
type PlanEntry struct {
	// Index is the chunk's position in the manifest, and the position it must
	// be written at. A manifest is a SEQUENCE (ADR-0034) and a plan preserves
	// its order, because a set of valid chunks assembled in another order is a
	// set of valid chunks and the wrong file.
	Index int
	// Chunk is the manifest's entry: offset, length, digest.
	Chunk chunking.Chunk
	// Availability is where the bytes are expected to come from.
	Availability Availability
	// Donor is where the local chunk index says these bytes are. Meaningful
	// only for AvailabilityLocal, and it is a PLACE (a blob, an offset, a
	// length) rather than an identity — ADR-0034 forbids resolving a chunk to
	// a blob and this field is never used to answer "which blob is this".
	Donor LocalChunk
}

// Plan is one transfer's decision, one entry per manifest chunk, in index
// order.
type Plan struct {
	// BlobHash is the whole-object digest the assembled result must hash to.
	// Carried so that a plan cannot be applied to another blob's staging file
	// by a caller holding two of them.
	BlobHash hashing.Hash
	Entries  []PlanEntry
}

// PlanStats is the number the feature exists to produce.
//
// An operator who cannot see how much was reused cannot tell whether reuse is
// working: a transfer that reused nothing and a transfer that reused
// everything both end in a published blob and a log line saying so.
type PlanStats struct {
	ChunksKept  int
	ChunksLocal int
	ChunksFetch int
	BytesKept   int64
	BytesLocal  int64
	BytesFetch  int64
}

// Stats totals a plan.
func (p Plan) Stats() PlanStats {
	var s PlanStats
	for _, e := range p.Entries {
		switch e.Availability {
		case AvailabilityKept:
			s.ChunksKept++
			s.BytesKept += e.Chunk.Length
		case AvailabilityLocal:
			s.ChunksLocal++
			s.BytesLocal += e.Chunk.Length
		case AvailabilityFetch:
			s.ChunksFetch++
			s.BytesFetch += e.Chunk.Length
		}
	}
	return s
}

// PlanFor decides, for every chunk of m, where the destination will get it.
//
// keptChunks is how many leading chunks the destination has already re-hashed
// out of its own staging file and found to match this manifest — the
// contiguous verified prefix of ADR-0035, expressed as a count of chunks
// rather than as a byte offset on purpose. A byte offset is the thing that
// record refuses to trust.
//
// held is this node's chunk index, already gathered: digest to every place
// this node believes it holds those bytes. It is passed in rather than looked
// up here so that this function stays pure and so that the lookup — which is a
// database read per distinct digest — happens once, in the caller, where it
// can be batched and cancelled.
//
// A donor whose recorded length disagrees with the manifest's is not chosen.
// That is not the integrity check — the integrity check is re-hashing the
// bytes, and it happens at execution — it is a plan declining to schedule a
// read it can already see is describing something else.
func PlanFor(m Manifest, keptChunks int, held map[hashing.Hash][]LocalChunk) Plan {
	if keptChunks < 0 {
		keptChunks = 0
	}
	if keptChunks > len(m.Chunks) {
		keptChunks = len(m.Chunks)
	}
	plan := Plan{BlobHash: m.BlobHash, Entries: make([]PlanEntry, 0, len(m.Chunks))}
	for i, c := range m.Chunks {
		entry := PlanEntry{Index: i, Chunk: c, Availability: AvailabilityFetch}
		switch {
		case i < keptChunks:
			entry.Availability = AvailabilityKept
		default:
			if donor, ok := pickDonor(c, held[c.Digest]); ok {
				entry.Availability = AvailabilityLocal
				entry.Donor = donor
			}
		}
		plan.Entries = append(plan.Entries, entry)
	}
	return plan
}

// pickDonor chooses where a locally held chunk will be read from.
//
// The first usable candidate in the order the index reported, which is stable,
// rather than "the best" one: every candidate is the same bytes if the index
// is right, and the re-verification at execution decides whether it was. A
// cleverer choice would be optimising the ordering of a read whose result is
// checked either way.
func pickDonor(c chunking.Chunk, candidates []LocalChunk) (LocalChunk, bool) {
	for _, cand := range candidates {
		if cand.Length != c.Length || cand.Offset < 0 || cand.BlobHash.IsZero() {
			continue
		}
		if !cand.Digest.Equal(c.Digest) {
			// The index answered with a different digest than the one asked
			// for. Nothing legitimate produces this; refusing it here means a
			// mis-keyed index cannot smuggle bytes past the manifest.
			continue
		}
		return cand, true
	}
	return LocalChunk{}, false
}

// Validate checks that a plan still describes the manifest it came from.
//
// The transfer loop calls this before it writes anything, so that a plan built
// for one manifest and applied against another — two transfers in flight, a
// refetched manifest after a source changed — fails before bytes move rather
// than at the whole-object verification, where the diagnosis is much worse.
func (p Plan) Validate(m Manifest) error {
	if !p.BlobHash.Equal(m.BlobHash) {
		return fmt.Errorf("manifests: this plan is for blob %s and the manifest describes %s",
			p.BlobHash, m.BlobHash)
	}
	if len(p.Entries) != len(m.Chunks) {
		return fmt.Errorf("manifests: this plan has %d entries and the manifest has %d chunks",
			len(p.Entries), len(m.Chunks))
	}
	for i, e := range p.Entries {
		if e.Index != i {
			return fmt.Errorf("manifests: plan entry %d records index %d — a plan is a sequence "+
				"and its order is the data", i, e.Index)
		}
		if !e.Availability.Valid() {
			return fmt.Errorf("manifests: plan entry %d has availability %q", i, e.Availability)
		}
		if e.Chunk != m.Chunks[i] {
			return fmt.Errorf("manifests: plan entry %d describes %d+%d %s, the manifest has "+
				"%d+%d %s", i, e.Chunk.Offset, e.Chunk.Length, e.Chunk.Digest,
				m.Chunks[i].Offset, m.Chunks[i].Length, m.Chunks[i].Digest)
		}
	}
	return nil
}
