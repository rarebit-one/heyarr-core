package worker

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
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

// IngestHandler runs the ingest pipeline for one ingest_artifact job.
//
// The handler is a decode and a delegation on purpose. A job handler that
// contains the logic is a job handler that cannot be tested without a queue,
// and the pipeline is the part worth testing.
func IngestHandler(p *ingest.Pipeline) HandlerFunc {
	return func(ctx context.Context, job jobs.Job) error {
		var payload ingest.Payload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			// A payload that cannot be decoded will never decode. Retrying it
			// five times is five identical failures and a longer wait before
			// anyone sees the real problem, but the queue owns retry policy —
			// so say clearly what happened and let it exhaust attempts.
			return fmt.Errorf("worker: ingest_artifact payload is not decodable: %w", err)
		}
		_, err := p.Ingest(ctx, ingest.Request{
			RootID:     payload.RootID,
			SourcePath: payload.Path,
			RelPath:    payload.RelPath,
			MIME:       payload.MIME,
		})
		return err
	}
}
