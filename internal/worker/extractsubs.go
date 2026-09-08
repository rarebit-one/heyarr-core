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
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// ExtractRecorder records an extracted subtitle as an asset on the video's
// Edition. An interface for the same reason RemuxRecorder is one: the handler
// is testable without a database, and the interesting behaviour here is the
// per-track loop and its failure handling, not SQL.
type ExtractRecorder interface {
	RecordExtractedSubtitle(ctx context.Context, sourceAssetID string, sub catalog.ExtractedSubtitle, now time.Time) error
}

// SubtitleExtractor enumerates and lifts out a video's embedded text subtitle
// tracks. An interface so the handler's per-track loop and failure handling can
// be exercised without a real ffmpeg/ffprobe; *ffmpeg.Extractor satisfies it.
type SubtitleExtractor interface {
	List(ctx context.Context, srcPath string) ([]ffmpeg.SubtitleStream, error)
	Extract(ctx context.Context, srcPath string, stream ffmpeg.SubtitleStream) (ffmpeg.Result, error)
}

// ExtractSubsHandlerOptions configure the extract_subtitles handler.
type ExtractSubsHandlerOptions struct {
	Extractor SubtitleExtractor
	// Store is reused from remux: an extraction needs exactly the same two
	// things — the source bytes as a local path, and somewhere to adopt the
	// output.
	Store    RemuxStore
	Recorder ExtractRecorder
	Now      func() time.Time
	Logger   *slog.Logger
}

// ExtractSubsHandler runs one extract_subtitles job: lift every embedded TEXT
// subtitle track out of a video into its own SubRip asset.
//
// # Failure handling is per-track on purpose
//
// A track FFmpeg cannot convert (a codec that lied about being text, a corrupt
// stream) is a permanent condition for that track and no retry fixes it — so it
// is logged and skipped, and the other tracks still get extracted. Only an
// infrastructure failure (the store or the database) returns an error, because
// those are the transient conditions a queue retry is for. Combined with the
// idempotent recorder (keyed on edition+blob+role) and CAS dedup, a re-run
// re-attempts the failed track and converges on the same rows for the rest.
//
// A video with no embedded text tracks (an external-sidecar release like GoT,
// or one with only bitmap subs) is a clean no-op, not a failure.
func ExtractSubsHandler(opts ExtractSubsHandlerOptions) HandlerFunc {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	return func(ctx context.Context, job jobs.Job) error {
		var payload ffmpeg.ExtractPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("extract-subs: undecodable payload: %w", err)
		}
		if payload.BlobHash == "" || payload.AssetID == "" {
			return errors.New("extract-subs: the payload names no blob or no asset")
		}

		src, err := opts.Store.SourcePath(ctx, payload.BlobHash)
		if err != nil {
			return fmt.Errorf("extract-subs: locating %s: %w", payload.BlobHash, err)
		}

		streams, err := opts.Extractor.List(ctx, src)
		if err != nil {
			return fmt.Errorf("extract-subs: enumerating tracks of %s: %w", payload.BlobHash, err)
		}
		if len(streams) == 0 {
			log.Info("no embedded text subtitle tracks to extract", "blob", payload.BlobHash)
			return nil
		}

		recorded := 0
		for _, s := range streams {
			res, err := opts.Extractor.Extract(ctx, src, s)
			if err != nil {
				// Permanent for this track — skip it, keep the others.
				log.Warn("skipping an embedded subtitle track that could not be extracted",
					"blob", payload.BlobHash, "stream", s.Index, "codec", s.Codec, "language", s.Language, "error", err)
				continue
			}

			hash, size, aErr := opts.Store.Adopt(ctx, res.Path)
			if rmErr := os.Remove(res.Path); rmErr != nil && !os.IsNotExist(rmErr) {
				log.Warn("an extracted subtitle file could not be removed", "path", res.Path, "error", rmErr)
			}
			if aErr != nil {
				return fmt.Errorf("extract-subs: adopting an extracted subtitle: %w", aErr)
			}

			if err := opts.Recorder.RecordExtractedSubtitle(ctx, payload.AssetID, catalog.ExtractedSubtitle{
				BlobHash: hash, Size: size, Language: s.Language, Forced: s.Forced, Title: s.Title,
			}, now()); err != nil {
				return fmt.Errorf("extract-subs: recording an extracted subtitle: %w", err)
			}
			recorded++
			log.Info("extracted an embedded subtitle",
				"source_blob", payload.BlobHash, "subtitle_blob", hash,
				"language", s.Language, "codec", s.Codec, "forced", s.Forced, "size", size)
		}

		log.Info("extracted embedded subtitles",
			"source_blob", payload.BlobHash, "tracks", len(streams), "recorded", recorded)
		return nil
	}
}
