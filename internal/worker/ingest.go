package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/media/probe"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// CASByteStore adapts the content-addressed store to the ingest domain's
// ByteStore port.
//
// It exists because neither side may import the other: the domain must not know
// how bytes are stored, and the storage fabric must stay extractable
// (ADR-0006, ADR-0007). The adapter is the seam, and it is deliberately thin —
// anything clever in here is a decision that belongs on one side or the other.
type CASByteStore struct {
	store cas.Store
}

// NewCASByteStore adapts a CAS store for ingest.
func NewCASByteStore(store cas.Store) *CASByteStore { return &CASByteStore{store: store} }

var _ ingest.ByteStore = (*CASByteStore)(nil)

// Link materialises a source file into the store.
func (a *CASByteStore) Link(ctx context.Context, sourcePath string, mode ingest.Materialisation) (ingest.Blob, error) {
	desc, err := a.store.Link(ctx, sourcePath, cas.Materialisation(mode))
	if err != nil {
		return ingest.Blob{}, err
	}
	return ingest.Blob{
		Hash:         desc.Hash.String(),
		Size:         desc.Size,
		Materialised: ingest.Materialisation(desc.Materialised),
		Deduplicated: desc.Deduplicated,
	}, nil
}

// OpenBlob gives the pipeline random access to stored bytes, for reading a
// publication container's own index (§69).
func (a *CASByteStore) OpenBlob(ctx context.Context, hash string) (ingest.ReaderAtCloser, int64, error) {
	h, err := hashing.Parse(hash)
	if err != nil {
		return nil, 0, err
	}
	rc, desc, err := a.store.Open(ctx, h)
	if err != nil {
		return nil, 0, err
	}
	return readerAt{rc}, desc.Size, nil
}

// readerAt adapts the CAS's seekable reader to io.ReaderAt.
//
// It is here rather than in the CAS because ReaderAt's contract — concurrent
// calls, no shared seek position — is stricter than ReadSeeker's, and this
// adapter is explicitly single-goroutine: one ingest examines one archive.
// Promising the stronger contract from the store would be promising something
// its callers do not need and its implementation does not provide.
type readerAt struct{ rs cas.ReadSeekCloser }

func (r readerAt) ReadAt(p []byte, off int64) (int, error) {
	if _, err := r.rs.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	return io.ReadFull(r.rs, p)
}

func (r readerAt) Close() error { return r.rs.Close() }

// IngestHandler runs the ingest pipeline for one ingest_artifact job.
//
// The handler is a decode and a delegation on purpose. A job handler that
// contains the logic is a job handler that cannot be tested without a queue,
// and the pipeline is the part worth testing.
func IngestHandler(p *ingest.Pipeline, probes ProbeEnqueuer) HandlerFunc {
	return func(ctx context.Context, job jobs.Job) error {
		var payload ingest.Payload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			// A payload that cannot be decoded will never decode. Retrying it
			// five times is five identical failures and a longer wait before
			// anyone sees the real problem, but the queue owns retry policy —
			// so say clearly what happened and let it exhaust attempts.
			return fmt.Errorf("worker: ingest_artifact payload is not decodable: %w", err)
		}
		res, err := p.Ingest(ctx, ingest.Request{
			RootID:     payload.RootID,
			SourcePath: payload.Path,
			RelPath:    payload.RelPath,
			MIME:       payload.MIME,
		})
		if err != nil {
			return err
		}

		// §66 puts probe in the pipeline. It is enqueued here rather than run
		// there because a probe is a JOB (§75) and jobs are how roles talk
		// (invariant 4): the probe may need a capability this worker does not
		// have, in which case another worker claims it — or none does, and it
		// waits, which is ADR-0023's degrade path and not a failure.
		//
		// After the ingest, never before: a probe job for a blob whose ingest
		// then rolled back is a job that can only fail.
		if probes != nil {
			enqueueProbe(ctx, probes, res, payload.RelPath)
		}
		return nil
	}
}

// ProbeEnqueuer queues a probe. It is an interface so the ingest handler is
// testable without a real queue.
type ProbeEnqueuer interface {
	Enqueue(ctx context.Context, opts jobs.EnqueueOptions) (jobs.Job, error)
}

// enqueueProbe queues a probe for freshly ingested bytes.
//
// A failure to enqueue is logged and swallowed rather than failing the ingest.
// The bytes are under management, hashed, replicated and servable; losing that
// because a follow-up job could not be queued would be trading the whole asset
// for its metadata. The next scan re-enqueues it.
func enqueueProbe(ctx context.Context, probes ProbeEnqueuer, res ingest.Result, relPath string) {
	// Only media. Probing a subtitle or a cover image costs a subprocess and a
	// job slot to learn nothing — ffprobe would describe a JPEG as a one-frame
	// video stream, which is true and useless.
	if !probe.IsProbable(ingest.Ext(ingest.Base(relPath))) {
		return
	}
	// A deduplicated blob has been probed already, or has a probe pending
	// under the same dedupe key. Either way there is nothing to add.
	if res.Deduplicated {
		return
	}

	if _, err := probes.Enqueue(ctx, jobs.EnqueueOptions{
		Type:               probe.JobType,
		Payload:            probe.Payload{BlobHash: res.BlobHash, Size: res.BlobSize},
		DedupeKey:          probe.DedupeKey(res.BlobHash),
		RequiredCapability: probe.Capability,
	}); err != nil {
		// Deliberately not returned. See above.
		_ = err
	}
}
