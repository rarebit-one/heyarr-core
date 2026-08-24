package manifests

import "github.com/rarebit-one/heyarr-core/internal/storagefabric/chunking"

// ChunkBlobJobType is §75's chunk_blob: read one blob and write its manifest.
//
// Declared here rather than in the worker, following integrity's verify_blob:
// something enqueues it and a worker runs it, and neither should have to
// import the other (invariant 4).
//
// It has been in §75's list since Milestone 1 with no handler behind it, which
// is the honest state §16 describes — chunking is deferred until something
// needs it — right up until the moment something did.
const ChunkBlobJobType = "chunk_blob"

// ChunkBlobPayload is what a chunk_blob job carries: the blob, and nothing
// else.
//
// No parameters. The chunker's settings come from the node's configuration at
// the moment the job RUNS, not from the moment it was written, so a job that
// sat in the queue across a settings change produces a manifest under the
// settings in force rather than a stale copy of yesterday's. The parameters
// that were actually used are recorded IN the manifest, which is where a
// reader needs them.
//
// No size either, though the handler needs one. A size in the payload is a
// second copy of a fact the store already knows, and the handler stats the
// blob anyway — reading it from the payload would mean a job written before a
// remux replaced the bytes could apply the threshold to the wrong length.
type ChunkBlobPayload struct {
	// BlobHash is the blob to chunk, in canonical form (ADR-0005).
	BlobHash string `json:"blob_hash"`
}

// ChunkBlobDedupeKey is the queue's idempotency key for chunking one blob.
//
// The blob hash and nothing else, following replication.Gap.DedupeKey's
// shape. There is exactly one useful answer to "produce the manifest for these
// bytes", and two jobs saying it are one job said twice: the manifest is a
// pure function of the bytes and the parameters, so the second run would
// re-read a whole blob to write the identical rows.
//
// No destination, no peer, no reason: any of those in the key would make two
// jobs that produce the identical manifest look like different work, which is
// precisely how a large blob gets read four times because four things wanted
// its manifest.
//
// The queue's partial-unique index over live jobs (ADR-0008) turns this into
// an enforced property rather than a convention.
func ChunkBlobDedupeKey(blobHash string) string { return "chunk:" + blobHash }

// ThresholdBytes is the size below which a blob is recorded as never needing a
// manifest (§16: "small Blobs may never require chunk manifests").
//
// # The number is chunking.DefaultMax, and that is the argument
//
// Below Max the chunker cannot produce a manifest worth having. A blob of at
// most Min (256 KiB) is ALWAYS exactly one chunk — the cut function refuses to
// consider a boundary before Min — and a one-chunk manifest is a strictly
// worse copy of the blob's own name: one digest, over all the bytes, at offset
// zero. Between Min and Max the chunker averages 1.09 x Avg, so a blob at the
// threshold is three or four chunks.
//
// # What the manifest costs at that size, relative to the blob
//
// The storage is not the cost and saying so plainly matters: a 4 MiB blob's
// manifest is a chunk_manifests row plus three or four manifest_chunks rows
// plus the same again in local_chunks — call it 1.5 KB, or 0.04% of the blob.
// Nobody would notice that.
//
// The cost is the READ. Generating the manifest means streaming every byte of
// the blob past two hashers, which is the same I/O as sending the blob — so
// below this size the manifest costs as much disk as the transfer it exists to
// optimise, and buys a resume granularity of one third of an object that a
// homelab link moves in well under a second. There is nothing to resume and
// three candidate chunks to deduplicate against.
//
// Above it the arithmetic inverts immediately and keeps going: a 20 GB remux is
// ~18,000 chunks, where a resumed transfer re-fetches a megabyte instead of
// twenty gigabytes, and that is the case §16 defers the work FOR.
//
// # It is a decision, recorded, not an inference
//
// A blob under the threshold gets [StateNotRequired] written down with
// [ReasonBelowThreshold], not left [StateUndecided]. The distinction is the
// whole of M5-03: "undecided" invites the next caller to go and find out, and
// finding out costs the full read this threshold exists to avoid. Recording it
// also means moving this number later is a visible, reviewable change to
// blobs that carry the reason they were exempted, rather than a silent
// recomputation of everything.
const ThresholdBytes int64 = chunking.DefaultMax

// ReasonBelowThreshold is the recorded grounds for a below-threshold
// exemption.
//
// A stable token rather than a sentence with the number in it, so that the day
// the threshold moves an operator can find every blob exempted under the old
// one with an equality query. The number lives in [ThresholdBytes] and in the
// log line; the reason names the POLICY, which is what a review of the
// decision needs.
const ReasonBelowThreshold = "below_chunking_threshold"
