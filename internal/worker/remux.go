package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/media/ffmpeg"
)

// RemuxStore is what a remux needs from storage: the source bytes by path, and
// somewhere to put the result.
type RemuxStore interface {
	// SourcePath returns a local filesystem path for a blob's bytes.
	//
	// FFmpeg needs a seekable file, and a remux reads the whole input by
	// definition — so unlike probing (§29) there is nothing to be gained by
	// ranging over HTTP here. A remote source would have to be materialised
	// anyway, which is Milestone 4's problem when a second peer exists.
	SourcePath(ctx context.Context, blobHash string) (string, error)
	// Adopt takes a finished file into the store and returns its blob.
	Adopt(ctx context.Context, path string) (string, int64, error)
}

// RemuxRecorder records the derived asset.
type RemuxRecorder interface {
	RecordDerived(ctx context.Context, sourceAssetID, blobHash string,
		size int64, container string, now time.Time) error
}

// RemuxHandlerOptions configure the transcode handler.
type RemuxHandlerOptions struct {
	Remuxer  *ffmpeg.Remuxer
	Store    RemuxStore
	Recorder RemuxRecorder
	Now      func() time.Time
	Logger   *slog.Logger
}

// RemuxHandler runs one transcode job.
//
// # Idempotency (invariant 9)
//
// The handler will be re-run: a lease that expires mid-remux returns the job
// to the queue. Re-running is safe because every step converges — the output
// goes to a fresh temporary file, the CAS deduplicates by hash on adoption,
// and the derived asset is an upsert. A half-written output is removed by the
// remuxer itself rather than left to be mistaken for a finished one.
func RemuxHandler(opts RemuxHandlerOptions) HandlerFunc {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	return func(ctx context.Context, job jobs.Job) error {
		var payload ffmpeg.Payload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("remux: undecodable payload: %w", err)
		}
		if payload.BlobHash == "" || payload.AssetID == "" {
			return errors.New("remux: the payload names no blob or no asset")
		}
		target, err := ffmpeg.ParseContainer(string(payload.Container))
		if err != nil {
			return err
		}

		src, err := opts.Store.SourcePath(ctx, payload.BlobHash)
		if err != nil {
			return fmt.Errorf("remux: locating %s: %w", payload.BlobHash, err)
		}

		res, err := opts.Remuxer.Remux(ctx, src, target)
		if err != nil {
			return err
		}
		// The output is ours until the store takes it. Removing it here rather
		// than trusting Adopt to consume it means a failure between the two
		// leaves nothing behind.
		defer func() {
			if rmErr := os.Remove(res.Path); rmErr != nil && !os.IsNotExist(rmErr) {
				log.Warn("a remux output could not be removed", "path", res.Path, "error", rmErr)
			}
		}()

		// The store hashes what it takes in and verifies it (invariant 1): a
		// remux output is not exempt from having its bytes identified just
		// because we produced it.
		hash, size, err := opts.Store.Adopt(ctx, res.Path)
		if err != nil {
			return fmt.Errorf("remux: taking the output into the store: %w", err)
		}

		if err := opts.Recorder.RecordDerived(ctx, payload.AssetID, hash, size,
			string(target), now()); err != nil {
			return err
		}

		log.Info("remuxed",
			"source_blob", payload.BlobHash, "derived_blob", hash,
			"container", string(target), "size", size, "elapsed", res.Elapsed)
		return nil
	}
}
