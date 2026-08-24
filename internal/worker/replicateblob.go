package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/peer/transfer"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// maxConcurrentTransfers bounds how many blobs this node pulls at once.
//
// The disk-head argument from runtime.go:33 applies, with one adjustment. A
// transfer is not CPU work: it is a network read whose bytes are then written
// sequentially to one disk and hashed on the way past. Running many at once
// does not make the link faster — it makes one spindle interleave several large
// sequential writes, which is the case that measures slower than doing them one
// at a time.
//
// It is 2 rather than 1 because a single slot makes the whole fabric's
// convergence hostage to its slowest source: one peer that accepts connections
// and then trickles would stop every other transfer for as long as it took to
// hit the dial timeout. Two slots means a stalled source costs half the
// throughput rather than all of it, and two large sequential writes is still a
// pattern a disk handles honestly.
//
// It is deliberately not a configuration knob yet. A number nobody has ever
// needed to change is a number that should stay a constant with an argument
// next to it.
const maxConcurrentTransfers = 2

// TransferDeps is what the replicate_blob handler needs to move bytes.
//
// It is a struct rather than five parameters because the handler is
// constructed lazily — see [ReplicateBlobRegistration] — and a lazily built
// dependency set assembled from positional arguments is a set whose order can
// be got wrong silently.
type TransferDeps struct {
	// Catalog answers who holds the bytes and records what happened.
	Catalog *catalog.Catalog
	// Store is this node's content store: where verified bytes land.
	Store cas.Store
	// Puller opens the pinned connection and verifies what arrives. It is
	// built lazily, because this node's private key may not exist yet when the
	// worker starts — the roles start concurrently and the controller is the
	// one that writes it (ADR-0002, ADR-0010).
	Puller func() (*transfer.Puller, error)
	Logger *slog.Logger
}

// ReplicateBlobRegistration is how the transfer job is registered, as one
// value.
//
// A function rather than a literal at the call site, following
// ReconcilePeerRegistration, so that the property the registration IS — a
// bounded number of concurrent transfers, and no required capability — can be
// asserted rather than read.
//
// No RequiredCapability. Pulling bytes needs a network and a disk, which every
// node has; there is no toolchain, indexer or download client involved. A node
// that cannot transcode anything can still hold a replica, and that is what a
// Full Peer is for (§6).
func ReplicateBlobRegistration(deps TransferDeps) Registration {
	return Registration{
		Handler:       ReplicateBlobHandler(deps),
		MaxConcurrent: maxConcurrentTransfers,
	}
}

// ReplicateBlobHandler is §75's replicate_blob: get one blob onto this peer
// (§20, §21, §32, ADR-0030, M4-09).
//
// # This is the handler the milestone is named after
//
// Everything before it authenticated peers and decided what should move. This
// moves it: the destination opens a pinned connection to a source the
// controller named, reads the ordinary blob endpoint, hashes what arrives
// against the digest it expected, and publishes only if they match.
//
// # The three things it must not do, and how each fails quietly
//
//  1. **Trust the source.** The expectation comes from the job — from the
//     reconciliation that scheduled the work — and is handed to
//     cas.PutExpecting unmodified. Nothing here parses a digest out of a
//     response header or assumes the URL and the body agree; both are trusting
//     the source with extra steps (invariant 1, §21).
//  2. **Put the controller in the data path.** The connection goes to the
//     source's own endpoint and the client follows no redirects. There is no
//     proxy path here, not for NAT and not for a first sync, because either
//     one makes controller availability into playback availability (§32,
//     ADR-0030, §53).
//  3. **Let a partial transfer become a replica.** `pending` while in flight,
//     `present` only after verification, and neither on failure. The `present`
//     write is a claim garbage collection acts on (ADR-0018), so it happens
//     once, after the bytes are on this disk and this node has hashed them.
//
// # Idempotent, and retried whole (invariant 9)
//
// It will be re-run. A blob this node already holds short-circuits to
// recording the replica and emits nothing — the same rule inventory
// reconciliation follows, where a re-run that changes nothing must also SAY
// nothing, or every retry becomes event noise.
//
// A failed transfer of a blob with no chunk manifest is still retried WHOLE,
// and that is §16's lazy chunking doing its job rather than a gap: a small
// blob's whole retry costs less than producing a manifest would.
//
// A blob that HAS a manifest is resumed, and the handler is idempotent in a
// stronger sense than it used to be. It used to be idempotent because a
// receive that did not finish left nothing to be right about. It is now
// idempotent because a re-run re-verifies what an earlier attempt left against
// a manifest this node fetched and verified itself, and keeps only the
// contiguous prefix that checks out (ADR-0035). One run and ten interrupted
// runs publish byte-identical bytes, and nothing partial is ever a replica:
// `pending` in flight, `present` written once, after the whole-object digest
// has been verified on this disk.
func ReplicateBlobHandler(deps TransferDeps) HandlerFunc {
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return func(ctx context.Context, job jobs.Job) error {
		var payload replication.ReplicateBlobPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("worker: replicate_blob payload is not decodable: %w", err)
		}
		hash, err := hashing.Parse(payload.BlobHash)
		if err != nil {
			// Validated before anything reaches a store that turns identifiers
			// into paths, and before a URL is built out of it.
			return fmt.Errorf("worker: replicate_blob names %q, which is not a blob identifier: %w",
				payload.BlobHash, err)
		}

		self, err := deps.Catalog.SelfPeer(ctx)
		if err != nil {
			return err
		}
		if payload.DestinationPeerID != self {
			// Replication is a destination PULL (ADR-0030): the machine that
			// ends up holding the bytes is the machine that fetches them, and
			// this node cannot write to another peer's disk.
			//
			// A job for another peer therefore belongs in that peer's queue,
			// and moving it there is job distribution across the fabric —
			// which does not exist yet. Failing loudly is the honest
			// intermediate state: the job is visible in the queue as work that
			// was offered to the wrong node, rather than silently completed by
			// a node that did nothing. The destination's own reconciliation
			// cycle emits the same gap into its own queue and closes it.
			return fmt.Errorf("worker: this replicate_blob names peer %s as the destination and this "+
				"node is peer %s — a destination pulls its own bytes (ADR-0030), so this job belongs "+
				"in that peer's queue", payload.DestinationPeerID, self)
		}

		record := catalog.BlobTransfer{BlobHash: hash.String(), DestinationPeerID: self}

		// Already held. The re-run case, and the concurrent-ingest case: the
		// bytes may have arrived by any route since the job was written.
		// Recording the replica is still right — the row may say `pending`
		// from an attempt that raced this one — but there is no transition to
		// announce, so nothing is emitted.
		held, err := deps.Store.Has(ctx, hash)
		if err != nil {
			return err
		}
		if held {
			desc, err := deps.Store.Stat(ctx, hash)
			if err != nil {
				return err
			}
			record.Bytes = desc.Size
			log.Debug("a replication found the bytes already present",
				"blob_hash", hash.String(), "peer_id", self)
			return deps.Catalog.RecordBlobTransferred(ctx, record)
		}

		sources, err := deps.Catalog.BlobSources(ctx, hash.String())
		if err != nil {
			return err
		}
		if len(sources) == 0 {
			// Nothing to pull from. Refused before a connection exists, which
			// is also what a revoked source looks like from here: revocation is
			// the deletion of the membership record (ADR-0012), and the
			// replicas rows referencing it go with it.
			//
			// Not recorded as a failed replica: nothing was attempted and
			// nothing about this peer's disk changed. It stays a gap, which the
			// next reconciliation cycle offers again once a source is enrolled
			// or comes back.
			return fmt.Errorf("%w: %s, wanted on peer %s", replication.ErrNoSource, hash, self)
		}

		puller, err := deps.Puller()
		if err != nil {
			return err
		}

		record.SourcePeerID = sources[0].PeerID
		if err := deps.Catalog.BeginBlobTransfer(ctx, record); err != nil {
			return err
		}

		var attempts error
		for _, src := range sources {
			if err := ctx.Err(); err != nil {
				// The lease is gone or the process is stopping. The row stays
				// `pending`, which is honest — a transfer was in flight and is
				// not any more — and the next cycle recomputes the gap.
				return err
			}
			record.SourcePeerID = src.PeerID
			outcome, err := pullFrom(ctx, puller, src, hash, log)
			if err == nil {
				record.Bytes = outcome.Bytes
				record.Reason = ""
				if err := deps.Catalog.RecordBlobTransferred(ctx, record); err != nil {
					return err
				}
				log.Info("replicated a blob",
					"blob_hash", hash.String(), "source_peer_id", src.PeerID,
					"destination_peer_id", self, "bytes", outcome.Bytes,
					"mode", outcome.Mode.String(),
					"chunks_kept", outcome.ChunksKept, "bytes_kept", outcome.BytesKept,
					"chunks_reused", outcome.ChunksReused, "bytes_reused", outcome.BytesReused,
					"chunks_fetched", outcome.ChunksFetched, "bytes_fetched", outcome.BytesFetched,
					"deduplicated", outcome.Deduplicated)
				return nil
			}

			// Bytes that arrived and did not verify are terminal for this
			// transfer and are NOT retried against another source in the same
			// run. The evidence is in quarantine (ADR-0018) and a second peer
			// racing to overwrite it would destroy exactly the thing worth
			// keeping. The queue retries the job whole, later.
			var corrupt *cas.Corruption
			if errors.As(err, &corrupt) {
				record.Bytes = 0
				record.Reason = "verification_failed"
				log.Warn("a peer served bytes that are not what was asked for",
					"blob_hash", hash.String(), "source_peer_id", src.PeerID,
					"actual", corrupt.Actual.String(), "quarantined_at", corrupt.Path)
				if recErr := deps.Catalog.RecordBlobTransferFailed(ctx, record, "corrupt"); recErr != nil {
					return errors.Join(err, recErr)
				}
				return err
			}

			attempts = errors.Join(attempts, err)
			log.Warn("a replication source did not deliver",
				"blob_hash", hash.String(), "source_peer_id", src.PeerID, "error", err)
		}

		record.Bytes = 0
		record.Reason = "no_source_delivered"
		if recErr := deps.Catalog.RecordBlobTransferFailed(ctx, record, "missing"); recErr != nil {
			return errors.Join(attempts, recErr)
		}
		return fmt.Errorf("worker: no source delivered %s to peer %s: %w", hash, self, attempts)
	}
}

// pullFrom moves one blob from one source, chunked if it can be and whole if
// it cannot (§16, ADR-0034, ADR-0035, M5-06).
//
// The branch is a question asked of the SOURCE and answered by the source's
// own state: does it hold a chunk manifest for these bytes. There is exactly
// one branch and it is here rather than inside the puller, so that "which path
// ran" is a decision with a name and an outcome field rather than an inference
// from how a transfer behaved.
//
// A source with no manifest is not a failure and is not retried elsewhere:
// §16 makes chunking lazy, so a peer holding bytes it never chunked is in an
// ordinary permanent state and a whole pull from it is always correct. A
// manifest that does not check out is treated the same way — the manifest is
// an optimisation and discarding a bad one costs a whole pull (ADR-0034),
// which is cheaper than reasoning about a description this node cannot trust.
func pullFrom(
	ctx context.Context, puller *transfer.Puller, src replication.Source,
	hash hashing.Hash, log *slog.Logger,
) (transfer.Outcome, error) {
	m, err := puller.FetchManifest(ctx, src, hash)
	switch {
	case err == nil:
		return puller.PullChunked(ctx, src, hash, m)
	case errors.Is(err, transfer.ErrSourceLacksBlob):
		// This source does not hold the bytes at all, so there is nothing here
		// to pull whole either. Try the next candidate.
		return transfer.Outcome{}, err
	case errors.Is(err, transfer.ErrSourceHasNoManifest):
		log.Debug("a source holds these bytes and has no chunk manifest for them, so they are "+
			"pulled whole", "blob_hash", hash.String(), "source_peer_id", src.PeerID)
	case errors.Is(err, transfer.ErrManifestCorrupt):
		log.Warn("a source served a chunk manifest that does not check out; pulling whole instead",
			"blob_hash", hash.String(), "source_peer_id", src.PeerID, "error", err)
	default:
		// Anything else — a peer running an older build with no manifest route,
		// a node serving bytes with no manifest store behind it, a refusal, a
		// timeout — degrades to a whole pull rather than failing the source.
		// That is ADR-0034's operational test applied at the transfer: a
		// manifest is an optimisation and everything must still work without
		// one. A source that cannot be reached at all will fail the pull too,
		// on the path that already reports it.
		log.Info("could not read a chunk manifest from a source; pulling whole instead",
			"blob_hash", hash.String(), "source_peer_id", src.PeerID, "error", err)
	}
	return puller.Pull(ctx, src, hash)
}

// lazyPuller builds this node's blob puller once, on first use.
//
// The private key is the reason it is lazy. It lives at 0600 in the data
// directory and is written by the controller at first start (ADR-0010); the
// roles start concurrently and are independently runnable as OS processes
// (ADR-0002), so a worker whose data directory has no key yet is an ordinary
// startup state rather than a fault. Resolving it eagerly would make that state
// a startup failure; resolving it per job would mint a certificate for every
// transfer.
//
// A failure is NOT cached. "The key is not there yet" is a condition that
// resolves itself the moment the controller finishes starting, and memoising
// the error would mean this worker never transferred anything again until it
// was restarted.
func lazyPuller(
	dataDir, peerID string, store cas.Store, index transfer.Index, log *slog.Logger,
) func() (*transfer.Puller, error) {
	var (
		mu     sync.Mutex
		puller *transfer.Puller
	)
	return func() (*transfer.Puller, error) {
		mu.Lock()
		defer mu.Unlock()
		if puller != nil {
			return puller, nil
		}
		priv, err := identity.Signer(dataDir)
		if err != nil {
			return nil, fmt.Errorf("worker: this node cannot present a peer identity, so it cannot "+
				"pull a replica: %w", err)
		}
		material, err := mtls.NewMaterial(mtls.MaterialOptions{PrivateKey: priv, PeerID: peerID})
		if err != nil {
			return nil, fmt.Errorf("worker: %w", err)
		}
		built, err := transfer.New(transfer.Options{
			Material: material, Store: store, Index: index, Logger: log,
		})
		if err != nil {
			return nil, err
		}
		puller = built
		return puller, nil
	}
}
