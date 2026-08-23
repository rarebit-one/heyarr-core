package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/chunking"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// maxConcurrentChunkings bounds how many blobs this node chunks at once.
//
// The disk-head argument from maxConcurrentTransfers applies almost unchanged.
// Chunking is a full sequential read of one file: running sixteen of them
// against one spindle makes the disk interleave sixteen streams, which
// measures slower than doing them one at a time and is exactly the case
// Registration.MaxConcurrent exists for.
//
// It is 2 rather than 1 for a reason a transfer does not have. Every byte read
// here goes through two BLAKE3 passes — the chunk digests and the whole-object
// hash — so a single slot leaves the disk idle while the CPU hashes and the
// CPU idle while the disk seeks. Two slots overlap those halves while still
// presenting the disk with only two sequential streams, which is a pattern it
// handles honestly. It is also the same small number the transfer path
// settled on, and two job types with the same shape should not disagree about
// it without an argument.
//
// Deliberately not a configuration knob. A number nobody has needed to change
// is a number that should stay a constant with its argument next to it.
const maxConcurrentChunkings = 2

// ChunkDeps is what the chunk_blob handler needs.
//
// A struct rather than positional parameters, following TransferDeps: a
// dependency set assembled from arguments of the same shape is a set whose
// order can be got wrong silently.
type ChunkDeps struct {
	// Store is where the bytes are. Chunking is a local read of local bytes.
	Store cas.Store
	// Manifests is where the manifest lands, and where the three-state
	// question is asked. Asking is a READ (ADR-0034) — see the handler.
	Manifests manifests.Store
	// Index is this node's local chunk index: which bytes it holds, and where
	// inside which blob (M5-07).
	Index manifests.Index
	// Checker is the EXISTING corruption path (ADR-0018). Bytes that do not
	// hash to their own name are reported through it rather than through
	// anything this handler invents, so a blob found corrupt by chunking is
	// quarantined, recorded and announced identically to one found corrupt by
	// a verify_blob job.
	Checker *integrity.Checker
	// Params are the chunker's settings. Zero means chunking.DefaultConfig.
	Params chunking.Config
	// Clock stamps the manifest's GeneratedAt. Injected so a test does not
	// need one (ADR-0017); it is deliberately NOT part of the manifest's
	// digest, which is what makes two runs byte-identical.
	Clock func() time.Time
	// Threshold is the size below which a blob is recorded as never needing a
	// manifest. Zero means manifests.ThresholdBytes.
	//
	// A field rather than only a constant because the property worth testing
	// is that the threshold is APPLIED and RECORDED, and a test that had to
	// write four megabytes to reach it is a test that measures the disk.
	Threshold int64
	Logger    *slog.Logger
}

// ChunkBlobRegistration is how the chunking job is registered, as one value.
//
// A function rather than a literal at the call site, following
// ReplicateBlobRegistration, so that the properties the registration IS — a
// bounded number of concurrent chunkings, and no required capability — can be
// asserted rather than read.
//
// No RequiredCapability. Chunking needs a disk and a CPU, which every node
// has: no toolchain, no indexer, no download client, and not even a reachable
// peer. A node that cannot transcode anything can still produce the manifest
// that makes its own outbound transfers resumable, and that is what a Full
// Peer is for (§6).
func ChunkBlobRegistration(deps ChunkDeps) Registration {
	return Registration{
		Handler:       ChunkBlobHandler(deps),
		MaxConcurrent: maxConcurrentChunkings,
	}
}

// ChunkBlobHandler is §75's chunk_blob: read one blob and write its manifest
// (§16, ADR-0034, M5-04).
//
// # It answers §16's policy question, which is WHEN
//
// Not at ingest: chunking every blob as it arrives turns a 20 GB remux's
// ingest into an extra full read for a manifest that may never be used, and
// §16 exists to say don't. Not inside the thing that discovered it wanted one
// either: a transfer that generated a manifest on finding none has turned a
// transfer into a full local read at the moment an operator is watching it,
// and — worse — has made the third state unobservable, because asking would
// then always produce "present".
//
// So: a job, enqueued by something that decided a manifest would be worth
// having (see reconcilePeerHandler), and a consumer that finds no manifest
// proceeds without one.
//
// # Asking must never generate
//
// The first thing this does is READ the state, and the read is the ordinary
// one every other caller makes. Nothing about being the chunking handler makes
// the question different: `present` and `not_required` are both final answers
// and both mean this job is done. It is `undecided` — and only `undecided` —
// that this handler is licensed to resolve, because a job is the one place
// where resolving it was somebody's decision rather than a side effect of a
// lookup.
//
// # Self-verifying, and what happens when it is not
//
// The blob is streamed ONCE: the chunker cuts and digests, and the same bytes
// go through a whole-object hasher on the way past. If the whole-object digest
// is not the blob's own name, NO manifest is written and the corruption goes
// out on the existing path — quarantined, never deleted (ADR-0018). A manifest
// built from unchecked bytes is the worst possible artefact: every chunk
// digest correct, describing a file that is not the one it is named after, and
// every later reassembly verifying happily against it.
//
// # Idempotent, and it says nothing when it does nothing (Invariant 9)
//
// It will be re-run. A blob whose manifest exists returns immediately having
// written no row and emitted no event; a blob already recorded as exempt does
// the same. The manifest itself is a pure function of the bytes and the
// parameters — nothing in its digest comes from a clock or a counter — so even
// a re-run that did write would write byte-identical rows.
//
// # No event, deliberately (Invariant 7)
//
// The precedent in internal/events is strict and cuts both ways: `blob.probed`
// exists because a probe describes bytes an operator did not previously have a
// description of; `blob.verified` deliberately does NOT exist, because a deep
// fsck over a healthy library would emit one per blob to say that nothing
// happened. Chunking is the second shape. A manifest existing is STATE — it is
// a row, and `chunk_manifest_state` is the question anything asks about it —
// not a transition anybody subscribes to, and a first sync over a hundred
// thousand blobs would otherwise write a hundred thousand events saying "a
// cache was warmed". The work itself is already visible: job.enqueued,
// job.succeeded and job.failed report every one of these jobs by construction.
// And the one genuinely new fact this handler can discover — that a blob is
// corrupt — is emitted, as replica.corrupt, by the path it is reported on.
func ChunkBlobHandler(deps ChunkDeps) HandlerFunc {
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	params := deps.Params
	if params == (chunking.Config{}) {
		params = chunking.DefaultConfig()
	}
	threshold := deps.Threshold
	if threshold == 0 {
		threshold = manifests.ThresholdBytes
	}
	now := deps.Clock
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	return func(ctx context.Context, job jobs.Job) error {
		var payload manifests.ChunkBlobPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("worker: chunk_blob payload is not decodable: %w", err)
		}
		hash, err := hashing.Parse(payload.BlobHash)
		if err != nil {
			// Validated before anything turns an identifier into a path.
			return fmt.Errorf("worker: chunk_blob names %q, which is not a blob identifier: %w",
				payload.BlobHash, err)
		}

		state, err := deps.Manifests.StateOf(ctx, hash)
		if errors.Is(err, catalog.ErrManifestBlobUnknown) {
			// Enqueued before a sweep reclaimed the blob. Stale rather than
			// wrong, and the same answer verify_blob gives: there is nothing
			// to chunk and nothing has gone wrong, so failing the job four
			// more times would only bury that in a queue error (ADR-0008).
			log.Info("chunk_blob skipped a blob the catalog no longer has", "blob_hash", hash.String())
			return nil
		}
		if err != nil {
			return err
		}
		switch state {
		case manifests.StatePresent:
			// The re-run case. Nothing is rewritten — not the manifest, not
			// its generated_at — because a re-run that changes a row is a
			// re-run that makes "changed nothing" unassertable.
			log.Debug("chunk_blob found a manifest already present", "blob_hash", hash.String())
			return nil
		case manifests.StateNotRequired:
			// A decision already taken, and re-taking it would move its
			// timestamp for no reason.
			log.Debug("chunk_blob found a recorded exemption", "blob_hash", hash.String())
			return nil
		case manifests.StateUndecided:
			// The only state this handler resolves.
		default:
			return fmt.Errorf("worker: chunk_blob got chunk-manifest state %q for %s", state, hash)
		}

		desc, err := deps.Store.Stat(ctx, hash)
		if errors.Is(err, cas.ErrNotFound) {
			// This node does not hold the bytes. Not a failure: chunking is a
			// local read of local bytes, and a job offered to a node that has
			// none of them has nothing to do. Failing would retry a read of a
			// file that is not here five times and then declare the job dead,
			// which says "chunking is broken" about a node that is simply not
			// the one holding the blob.
			//
			// Nothing is recorded either. "Absent from this node" is not a
			// decision that these bytes never need a manifest — the node that
			// does hold them still gets to decide.
			log.Info("chunk_blob skipped a blob this node does not hold", "blob_hash", hash.String())
			return nil
		}
		if err != nil {
			return err
		}

		if desc.Size < threshold {
			// §16's "small Blobs may never require chunk manifests", given a
			// number. Recorded as a decision with its grounds, so the day the
			// threshold moves the blobs exempted under the old one can be
			// found — and NOT left undecided, which would invite the next
			// caller to spend the full read finding out.
			//
			// No bytes were read to reach this: a Stat, and the answer.
			if err := deps.Manifests.RecordNotRequired(
				ctx, hash, manifests.ReasonBelowThreshold); err != nil {
				return err
			}
			log.Debug("a blob was recorded as never needing a chunk manifest",
				"blob_hash", hash.String(), "size", desc.Size, "threshold", threshold)
			return nil
		}

		rc, _, err := deps.Store.Open(ctx, hash)
		if err != nil {
			return err
		}
		defer func() { _ = rc.Close() }()

		manifest, err := manifests.Generate(ctx, rc, hash, params, now())
		var mismatch *manifests.WholeObjectMismatch
		switch {
		case errors.As(err, &mismatch):
			return deps.reportCorruption(ctx, log, hash, mismatch)
		case err != nil:
			// Cancellation lands here, and lands here having written nothing:
			// the manifest is one write, after the whole blob has been read
			// and verified, so there is no partial state to clean up.
			return err
		}

		if err := deps.Manifests.Save(ctx, manifest); err != nil {
			return err
		}
		// The reuse index second, and derived from the manifest rather than
		// collected alongside it, so the two cannot disagree about what was
		// chunked (M5-07).
		if err := deps.Index.RecordLocal(ctx, hash, manifests.LocalChunks(manifest)); err != nil {
			return err
		}

		log.Info("chunked a blob",
			"blob_hash", hash.String(), "size", desc.Size,
			"chunks", manifest.ChunkCount(), "manifest_digest", manifest.Digest.String())
		return nil
	}
}

// reportCorruption puts a whole-object mismatch onto the EXISTING corruption
// path and writes no manifest.
//
// It re-reads the blob, through integrity.Checker, and that second read is
// deliberate rather than an oversight. Quarantining is the store's own
// operation and reaching around it would mean this handler had its own private
// idea of where evidence goes; going through the checker means the bytes are
// quarantined, the replica is marked corrupt and replica.corrupt is emitted by
// exactly the code a verify_blob job uses. The cost is one extra read of a blob
// already known to be bad, which is the same trade integrity's own checker
// makes when a size mismatch escalates it to a full verification.
//
// The job SUCCEEDS, following VerifyBlobHandler. The question "can a manifest
// be made from these bytes" was answered — no, and here is why — and failing
// would re-read known-bad bytes four more times before losing the finding in a
// queue error instead of leaving it in the event log (ADR-0008).
func (d ChunkDeps) reportCorruption(
	ctx context.Context, log *slog.Logger, hash hashing.Hash, mismatch *manifests.WholeObjectMismatch,
) error {
	if d.Checker == nil {
		// Nothing to report through, so the finding must not be lost: fail the
		// job loudly with the mismatch attached rather than panicking, and
		// still write no manifest. A node wired without a corruption path is a
		// misconfiguration, and this is the one moment it matters.
		return fmt.Errorf("worker: chunking %s read bytes hashing to %s, and this handler has no "+
			"corruption path wired to report it on: %w", hash, mismatch.Actual, mismatch)
	}
	finding, err := d.Checker.VerifyBlob(ctx, hash)
	if err != nil {
		return errors.Join(mismatch, err)
	}
	if finding.Kind == "" {
		// The chunking pass says these bytes are not the blob and a full
		// re-verification says they are. Something is rewriting the file while
		// it is being read, which is neither a clean blob nor a quarantinable
		// one, and the honest response is to fail the job rather than to write
		// a manifest built from bytes that changed underneath the reader.
		return fmt.Errorf("worker: chunking %s read bytes hashing to %s, and a re-verification "+
			"found the blob intact — the file is being written while it is read: %w",
			hash, mismatch.Actual, mismatch)
	}
	log.Warn("chunking found a blob that is not what it is named after — no manifest was written",
		"blob_hash", hash.String(), "actual", mismatch.Actual.String(),
		"kind", string(finding.Kind), "quarantined", finding.Quarantined, "path", finding.Path)
	return nil
}
