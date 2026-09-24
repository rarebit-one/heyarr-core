package worker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/downloads"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/media"
	"github.com/rarebit-one/heyarr-core/internal/media/ffmpeg"
	"github.com/rarebit-one/heyarr-core/internal/media/probe"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/providers"
	"github.com/rarebit-one/heyarr-core/internal/scanner"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// The job registrations Worker.Run wires, one function per area. Each is the
// code that used to sit inline in Run, moved rather than rewritten: what a
// handler needs is in workerDeps, built once by Run after startup has opened
// the database, the store and the provider registry.

// workerDeps is what the registrations are built from.
type workerDeps struct {
	cfg       config.Config
	log       *slog.Logger
	db        *sqlite.DB
	store     *cas.FS
	cat       *catalog.Catalog
	queue     *jobs.Queue
	pipeline  *ingest.Pipeline
	checker   *integrity.Checker
	collector *integrity.Collector
	scan      *scanner.Scanner
	peerID    string
	providers *providers.Registry
	toolchain media.Toolchain
}

// registerStorageJobs registers the CAS and library jobs: ingest, the scanner,
// blob verification and garbage collection.
func registerStorageJobs(reg *Registry, d workerDeps) {
	reg.RegisterFunc(ingest.JobType, IngestHandler(d.pipeline, d.queue))
	reg.RegisterFunc(scanner.JobType, ScanHandler(d.scan))
	reg.RegisterFunc(integrity.VerifyJobType, VerifyBlobHandler(d.checker, d.log))
	// One garbage collection at a time. Two concurrent sweeps would each walk
	// the store while the other unlinked from it, and the loser would spend the
	// pass reporting the winner's deletions as missing blobs.
	reg.Register(integrity.GCJobType, Registration{
		Handler:       GCHandler(d.collector, d.log),
		MaxConcurrent: 1,
	})
}

// registerAcquisitionJobs registers the acquisition sweeps that need nothing but
// the database — reconciliation and the upgrade scan — and the ingest of
// completed acquisitions.
func registerAcquisitionJobs(reg *Registry, d workerDeps) {
	// Reconciliation answers §56's two questions for every want (§57, M3-05).
	//
	// One at a time, for the same reason as garbage collection: two concurrent
	// sweeps would each read the library while the other wrote its
	// conclusions, and the loser would spend the pass recording answers that
	// were already stale.
	//
	// No RequiredCapability. It needs nothing but the database — no toolchain,
	// no indexer, no download client — so a fully degraded node still knows
	// what it is missing, which is exactly the node whose operator most needs
	// to be told.
	reg.Register(acquisition.ReconcileJobType, Registration{
		Handler:       ReconcileHandler(d.cat, d.log),
		MaxConcurrent: 1,
	})
	// The upgrade scan (§60, M3-06). One at a time, for the same reason
	// reconciliation is: two concurrent scans would each read the library
	// while the other concluded.
	//
	// No RequiredCapability. It reads the database and decides; it needs no
	// toolchain, no indexer and no download client — and a node that cannot
	// acquire anything can still tell an operator what could be better, which
	// is exactly the node whose operator most wants to know.
	reg.Register(acquisition.UpgradeScanJobType, Registration{
		Handler:       UpgradeScanHandler(d.cat, d.log),
		MaxConcurrent: 1,
	})

	// Ingest of completed acquisitions (§65, M3-13).
	//
	// Registered UNCONDITIONALLY, unlike the download poll, and the asymmetry is
	// deliberate. Polling needs a download client; hashing a file that is
	// already on disk needs nothing but the disk. A node with no download
	// client configured can still finish an acquisition another node started —
	// which is what a compute peer is for (§6) — and refusing to register the
	// handler would make that impossible for no reason.
	//
	// One at a time: hashing is I/O-bound on the same storage the CAS writes
	// to, and running several against one disk is slower than running one.
	reg.Register(acquisition.IngestJobType, Registration{
		Handler:       IngestAcquisitionHandler(d.cat, d.cat, d.pipeline, d.queue, d.log),
		MaxConcurrent: 1,
	})
}

// registerReplicationJobs registers peer convergence, the blob transfer and lazy
// chunking.
func registerReplicationJobs(reg *Registry, d workerDeps) {
	// Peer convergence (§19, §57, M4-08). §19's desired blob set against what
	// the peers report holding, emitting replicate_blob for the difference.
	//
	// One at a time, for the reason reconciliation and the upgrade scan are: two concurrent
	// cycles would each read the fabric while the other enqueued against it,
	// and the loser would spend the pass deciding against a picture that had
	// already moved. The dedupe key keeps the RESULT correct either way — it
	// is a unique index, not a convention — but the second cycle would still
	// be wasted work whose counts described a fabric nobody saw.
	//
	// No RequiredCapability, following the precedent above. It needs nothing
	// but the database: no toolchain, no indexer, no download client, and not
	// even a reachable peer — the diff is against the last inventory a peer
	// reported, not against a live probe. A fully degraded node still knows
	// what it is missing, which is exactly the node whose operator most needs
	// to be told.
	reg.Register(replication.ReconcilePeerJobType,
		ReconcilePeerRegistration(d.cat, d.queue, d.log))
	// The transfer itself (§21, §32, ADR-0030, M4-09). The destination pulls:
	// this node opens a pinned mTLS connection to a peer that holds the bytes,
	// reads the ordinary blob endpoint, and hashes what arrives against what it
	// asked for. The controller is not on that hop and there is no code path
	// here that would put it there.
	//
	// # Why the puller is built lazily
	//
	// It needs this node's PRIVATE key, and the roles start concurrently
	// (ADR-0002): a worker started before any controller has ever run is
	// legitimately alive on a data directory that has no key in it yet. Building
	// it at startup would turn that ordinary state into a startup failure, and
	// building it once per job would sign a fresh certificate for every
	// transfer. So it is built on first use and kept — and a node that still has
	// no identity fails the JOB, with a message that names the missing key,
	// rather than refusing to start at all.
	//
	// MaxConcurrent lives in the registration, where its argument can be read
	// next to the number.
	// The catalog is handed in as the chunk index (M5-07): a transfer that can
	// see what this node already holds fetches only what it does not. It is a
	// CLAIM about this disk and the transfer re-verifies every chunk it
	// supplies against the manifest, so a stale entry costs a refetch rather
	// than a wrong file.
	// The membership the puller re-reads mid-session, so a peer revoked while a
	// piece transfer is running stops being asked within one generation rather
	// than at the next session (#290, ADR-0012). It reads the SAME rows
	// pullPieces surveys from — Peers() already excludes this node — so the two
	// cannot disagree about who a member is.
	members := func(ctx context.Context) (map[string]bool, error) {
		peers, err := d.cat.Peers(ctx)
		if err != nil {
			return nil, err
		}
		roster := make(map[string]bool, len(peers))
		for _, p := range peers {
			roster[p.PeerID] = true
		}
		return roster, nil
	}
	transferPuller := lazyPuller(d.cfg.DataDir, d.peerID, d.store, d.cat, members, d.log)
	reg.Register(replication.ReplicateBlobJobType, ReplicateBlobRegistration(TransferDeps{
		Catalog: d.cat,
		Store:   d.store,
		Puller:  transferPuller,
		Logger:  d.log,
	}))

	// Lazy chunking (§16, §75, ADR-0034, M5-04). §75 has listed chunk_blob
	// since Milestone 1 and nothing has ever handled it, which was the honest
	// state: §16 defers the work until something needs it, and until peer
	// convergence existed nothing did.
	//
	// It is registered UNCONDITIONALLY and with no RequiredCapability, for the
	// reason acquisition ingest is: reading a file that is already on this disk
	// and hashing it needs nothing but the disk. MaxConcurrent lives in the
	// registration, where its argument can be read next to the number.
	//
	// There is deliberately NO scheduled beat behind it. A sweep that chunked
	// the whole library would read every byte in the store for manifests
	// nothing asked for, which is the I/O storm §16 exists to avoid; the work
	// is enqueued by the reconciliation that decided the bytes are about to
	// move (see reconcilePeerHandler).
	reg.Register(manifests.ChunkBlobJobType, ChunkBlobRegistration(ChunkDeps{
		Store:     d.store,
		Manifests: d.cat,
		Index:     d.cat,
		Checker:   d.checker,
		Logger:    d.log,
	}))
}

// registerProviderJobs registers the provider health pass and the handlers that
// are routed by a provider capability.
func registerProviderJobs(reg *Registry, d workerDeps) {
	// The health pass. One at a time, and no RequiredCapability: a node with
	// NO providers still runs it, finds nothing, and does nothing — which is
	// cheaper than a capability check and means the job is never mysteriously
	// pending on a node that simply has nothing to check.
	reg.Register(providers.HealthJobType, Registration{
		Handler:       ProviderHealthHandler(d.providers, d.cat, d.log),
		MaxConcurrent: 1,
	})

	// Capability routing's second and third users, after the media toolchain
	// (ADR-0023). A node with no indexer configured advertises no `indexer`
	// capability, so a search job stays PENDING AND VISIBLE rather than being
	// claimed and failed — which is ADR-0025's whole claim.
	//
	// The search handler, registered only when this worker has an indexer —
	// the same reasoning as the probe and poll handlers. Not registering it at
	// all makes the degraded state visible in the startup log, which lists the
	// types this worker will claim, and "why is nothing being searched for"
	// should be answerable from that log.
	//
	// On a node with no indexer the job stays PENDING AND VISIBLE rather than
	// being claimed and failed, which is ADR-0025's whole claim: a search that
	// cannot run is work waiting for a capability, not work that went wrong.
	//
	// MaxConcurrent is deliberately NOT 1. Unlike reconciliation and the
	// upgrade scan, each search is scoped to one want and writes only that
	// want's rows, so two running at once contend over nothing — and a library
	// of two hundred wants would otherwise take two hundred sequential
	// provider round trips to make one pass.
	if d.providers.Has(providers.CapabilityIndexer) {
		d.log.Info("indexing is available",
			"providers", strings.Join(indexerNames(d.providers), ", "))
		reg.Register(acquisition.SearchJobType, Registration{
			Handler:            SearchHandler(d.providers, d.cat, d.queue, d.log),
			RequiredCapability: providers.CapabilityIndexer.JobCapability(),
		})
	}
	// The source-poll handler (§55, M12), registered only when this worker has a
	// feed adapter — the same reasoning as the search handler. A node with no
	// metadata provider never advertises `metadata`, so a poll_source job stays
	// PENDING AND VISIBLE rather than being claimed and failed (ADR-0025), and the
	// startup log names the types this worker will claim so "why is nothing being
	// followed" is answerable from it.
	//
	// MaxConcurrent is deliberately NOT 1: each poll is scoped to one source and
	// writes only that source's items and wants, so two running at once contend
	// over nothing, and a library of many followed sources coming due at once must
	// not be polled one feed round-trip at a time.
	if d.providers.Has(providers.CapabilityMetadata) {
		d.log.Info("a feed adapter is available",
			"providers", strings.Join(feedProviderNames(d.providers), ", "))
		reg.Register(followed.PollSourceJobType, Registration{
			Handler:            PollSourceHandler(d.providers, d.cat, d.queue, d.log),
			RequiredCapability: providers.CapabilityMetadata.JobCapability(),
		})
	}
	// The poll handler, registered only when this worker has a download client
	// — the same reasoning as the probe handler below. Not registering it at
	// all makes the degraded state visible in the startup log, which lists the
	// types this worker will claim, and "why is nothing being acquired" should
	// be answerable from that log.
	//
	// One at a time: two concurrent passes would each read a client's queue
	// while the other wrote its conclusions, and the loser would record
	// progress that was already stale.
	if d.providers.Has(providers.CapabilityDownload) {
		d.log.Info("a download client is available",
			"providers", strings.Join(downloadClientNames(d.providers), ", "))
		reg.Register(downloads.PollJobType, Registration{
			Handler:            PollDownloadsHandler(d.providers, d.cat, d.queue, d.log),
			MaxConcurrent:      1,
			RequiredCapability: providers.CapabilityDownload.JobCapability(),
		})
		// The grab — §64's SELECTED → QUEUED edge (#225). Registered under the
		// same condition as the poll and for the same reason: a node with no
		// download client cannot start a transfer, and ADR-0025 says work that
		// needs an absent capability stays PENDING AND VISIBLE rather than
		// being claimed and failed.
		//
		// MaxConcurrent is deliberately NOT 1, unlike the poll. A poll reads a
		// whole client's queue and two passes would conclude over each other;
		// a grab is scoped to one want, writes only that want's rows, and
		// contends with another grab over nothing. Serialising them would make
		// a library that just became satisfiable start its transfers one
		// provider round trip at a time.
		reg.Register(acquisition.GrabJobType, Registration{
			Handler:            GrabReleaseHandler(d.providers, d.cat, d.log),
			RequiredCapability: providers.CapabilityDownload.JobCapability(),
		})
	}

	// The subtitle fetch handler (ADR-0085), registered only when this worker has
	// a subtitle provider — the same degrade discipline as the search and poll
	// handlers. A node with no CapabilitySubtitle provider never advertises it, so
	// a fetch_subtitle job stays PENDING AND VISIBLE rather than being claimed and
	// failed (ADR-0025). Not MaxConcurrent 1: each fetch is scoped to one want, a
	// small download, and two contend over nothing — but the provider's own rate
	// limiter (in the adapter) paces the calls.
	if d.providers.Has(providers.CapabilitySubtitle) {
		d.log.Info("a subtitle provider is available",
			"providers", strings.Join(subtitleProviderNames(d.providers), ", "))
		reg.Register(acquisition.FetchSubtitleJobType, Registration{
			Handler: FetchSubsHandler(FetchSubsHandlerOptions{
				Providers:  d.providers.SubtitleProviders(),
				Recorder:   d.cat,
				Store:      NewCASSubtitleStore(d.store),
				Downloader: NewHTTPSubtitleDownloader(),
				Logger:     d.log,
			}),
			RequiredCapability: providers.CapabilitySubtitle.JobCapability(),
		})
	}

	// The enrich handler (ADR-0087), registered only when this worker has an
	// enrich provider — the same degrade discipline as the subtitle handler. A node
	// with no CapabilityEnrich provider never advertises it, so an enrich_work job
	// stays PENDING AND VISIBLE rather than being claimed and failed (ADR-0025).
	if d.providers.Has(providers.CapabilityEnrich) {
		d.log.Info("an enrich provider is available",
			"providers", strings.Join(enrichProviderNames(d.providers), ", "))
		reg.Register(acquisition.EnrichWorkJobType, Registration{
			Handler: EnrichHandler(EnrichHandlerOptions{
				Providers: d.providers.EnrichProviders(),
				Recorder:  d.cat,
				Store:     NewCASArtworkStore(d.store),
				Fetcher:   NewHTTPArtworkFetcher(),
				Logger:    d.log,
			}),
			RequiredCapability: providers.CapabilityEnrich.JobCapability(),
		})
	}
}

// registerMediaJobs registers the handlers that need the media toolchain: probe,
// remux and embedded-subtitle extraction.
func registerMediaJobs(reg *Registry, d workerDeps) error {
	// The probe handler, registered only when this worker can actually run it.
	//
	// Registering it unconditionally with RequiredCapability set would also
	// work — claimableTypes() filters on the capability — but not registering
	// it at all makes the degraded state visible in the startup log, which
	// lists the types this worker will claim. "Why is nothing probing" should
	// be answerable from the log a worker prints when it starts.
	if d.toolchain.FFprobe.Available {
		prober, err := probe.New(probe.Options{
			FFprobePath: d.toolchain.FFprobe.Path,
			TempDir:     d.cfg.DataDir,
			Logger:      d.log,
		})
		if err != nil {
			return fmt.Errorf("worker: building the prober: %w", err)
		}
		endpoint := d.cfg.PeerEndpoint()
		client, baseURL, err := probe.EndpointClient(endpoint, 30*time.Second)
		if err != nil {
			// A node that can probe but cannot reach itself is a
			// misconfiguration worth stopping for: the alternative is every
			// probe job failing at runtime with the same error, five times
			// each, forever.
			return fmt.Errorf("worker: %w", err)
		}
		prober.SetHTTPClient(client)

		authStore, err := auth.NewStore(auth.StoreOptions{Writer: d.db.Writer(), Reader: d.db.Reader()})
		if err != nil {
			return fmt.Errorf("worker: opening the credential store for probes: %w", err)
		}
		reg.Register(probe.JobType, Registration{
			RequiredCapability: probe.Capability,
			Handler: ProbeHandler(ProbeHandlerOptions{
				Prober: prober, Recorder: d.cat, Tokens: authStore,
				BaseURL: baseURL, Logger: d.log,
			}),
			// Probes are subprocesses that read over the network. Two at once
			// is fine; twenty is a worker that has stopped doing anything else
			// and a peer serving twenty concurrent range storms.
			MaxConcurrent: 2,
		})
		d.log.Info("probing is available", "endpoint", endpoint, "ffprobe", d.toolchain.FFprobe.Version)
	}

	// The remux handler, registered only when this worker can run it — same
	// reasoning as the prober: not registering it makes the degraded state
	// visible in the startup log, which lists the types this worker claims.
	if d.toolchain.FFmpeg.Available {
		remuxer, err := ffmpeg.New(ffmpeg.Options{
			FFmpegPath: d.toolchain.FFmpeg.Path,
			// Inside the data directory, so the output shares a filesystem
			// with the store and adoption is metadata rather than a copy
			// (ADR-0014).
			WorkDir: d.cfg.DataDir,
			Logger:  d.log,
		})
		if err != nil {
			return fmt.Errorf("worker: building the remuxer: %w", err)
		}
		reg.Register(ffmpeg.JobType, Registration{
			RequiredCapability: ffmpeg.Capability,
			Handler: RemuxHandler(RemuxHandlerOptions{
				Remuxer:  remuxer,
				Store:    NewCASRemuxStore(d.store),
				Recorder: d.cat,
				Logger:   d.log,
			}),
			// One at a time. A remux is bounded by disk rather than CPU, and
			// two concurrent ones on the same spindle are slower than two in
			// sequence while also filling the work directory twice as fast.
			MaxConcurrent: 1,
		})
		d.log.Info("remuxing is available", "ffmpeg", d.toolchain.FFmpeg.Version)

		// Embedded-subtitle extraction (ADR-0084). It needs BOTH binaries —
		// ffprobe to enumerate the tracks, ffmpeg to lift each out — so it is
		// registered only when both resolved. On a node with ffmpeg but no
		// ffprobe the job stays pending, visibly, rather than failing at
		// runtime; the same degrade discipline as the two handlers above.
		if d.toolchain.FFprobe.Available {
			extractor, err := ffmpeg.NewExtractor(ffmpeg.ExtractorOptions{
				FFmpegPath:  d.toolchain.FFmpeg.Path,
				FFprobePath: d.toolchain.FFprobe.Path,
				WorkDir:     d.cfg.DataDir,
				Logger:      d.log,
			})
			if err != nil {
				return fmt.Errorf("worker: building the subtitle extractor: %w", err)
			}
			reg.Register(ffmpeg.ExtractJobType, Registration{
				RequiredCapability: ffmpeg.Capability,
				Handler: ExtractSubsHandler(ExtractSubsHandlerOptions{
					Extractor: extractor,
					Store:     NewCASRemuxStore(d.store),
					Recorder:  d.cat,
					Logger:    d.log,
				}),
				// One at a time, like the remux beside it: extraction is a
				// subprocess reading a whole video off the same spindle.
				MaxConcurrent: 1,
			})
			d.log.Info("subtitle extraction is available", "ffmpeg", d.toolchain.FFmpeg.Version, "ffprobe", d.toolchain.FFprobe.Version)
		}
	}
	return nil
}
