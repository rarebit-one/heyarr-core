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
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
)

// VerifyBlobHandler runs one verify_blob job.
//
// It succeeds when the blob is corrupt. That is not a swallowed error: the job
// asked "are these bytes still what they claim to be", the answer was recorded,
// the replica is marked and the bytes are quarantined. Failing the job would
// retry it four more times, re-hashing bytes already known to be bad and then
// declaring the job dead — which loses the finding in a queue error instead of
// leaving it in the event log where an operator will see it (ADR-0008).
func VerifyBlobHandler(checker *integrity.Checker, log *slog.Logger) HandlerFunc {
	return func(ctx context.Context, job jobs.Job) error {
		var payload integrity.VerifyPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("worker: verify_blob payload is not decodable: %w", err)
		}
		h, err := hashing.Parse(payload.Hash)
		if err != nil {
			return fmt.Errorf("worker: verify_blob was given %q: %w", payload.Hash, err)
		}

		finding, err := checker.VerifyBlob(ctx, h)
		switch {
		case errors.Is(err, integrity.ErrUnknownBlob):
			// Enqueued before a sweep reclaimed it. Nothing to verify and
			// nothing wrong.
			log.Info("verify_blob skipped a blob the catalog no longer has", "blob_hash", payload.Hash)
			return nil
		case err != nil:
			return err
		}
		if finding.Kind == "" {
			log.Debug("blob verified", "blob_hash", payload.Hash)
			return nil
		}
		log.Warn("blob failed verification", "blob_hash", payload.Hash,
			"kind", string(finding.Kind), "quarantined", finding.Quarantined, "path", finding.Path)
		return nil
	}
}

// GCHandler runs one gc_blobs job.
//
// The payload's zero value is a dry run (ADR-0018). A scheduled sweep that
// reports is useful; a scheduled sweep that deletes because a field was
// omitted is how a library disappears overnight.
func GCHandler(collector *integrity.Collector, log *slog.Logger) HandlerFunc {
	return func(ctx context.Context, job jobs.Job) error {
		var payload integrity.GCPayload
		if len(job.Payload) > 0 {
			if err := json.Unmarshal(job.Payload, &payload); err != nil {
				return fmt.Errorf("worker: gc_blobs payload is not decodable: %w", err)
			}
		}
		result, err := collector.Collect(ctx, integrity.CollectOptions{
			Apply: payload.Apply,
			Grace: time.Duration(payload.GraceSeconds) * time.Second,
		})
		if err != nil {
			return err
		}
		log.Info("gc_blobs finished",
			"dry_run", result.DryRun, "considered", result.Considered,
			"reclaimed", len(result.Reclaimed), "untracked", len(result.Untracked),
			"bytes_reclaimed", result.BytesReclaimed)
		return nil
	}
}
