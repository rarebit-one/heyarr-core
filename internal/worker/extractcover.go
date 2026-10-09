package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/media/cover"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// CoverSource locates a blob's bytes as a local file. CASRemuxStore satisfies it:
// a cover is read out of the same CAS file a remux reads.
type CoverSource interface {
	SourcePath(ctx context.Context, blobHash string) (string, error)
}

// CoverRecorder records the outcome of looking inside a book file. An interface
// so the handler's outcome logic is testable without a database.
type CoverRecorder interface {
	RecordExtractedCover(ctx context.Context, sourceAssetID, format string, art catalog.FetchedArtwork, now time.Time) error
	RecordCoverAttempt(ctx context.Context, blobHash, format, detail string, now time.Time) error
}

// PageRenderer renders a PDF's first page. *cover.PDFRenderer satisfies it; nil
// on a node without pdftoppm, which never claims a PDF job (the job requires the
// capability).
type PageRenderer interface {
	RenderFirstPage(ctx context.Context, src string) (cover.Image, error)
}

// ExtractCoverHandlerOptions configure the extract_cover handler.
type ExtractCoverHandlerOptions struct {
	Source   CoverSource
	Blobs    ArtworkBlobStore
	Recorder CoverRecorder
	Renderer PageRenderer
	Now      func() time.Time
	Logger   *slog.Logger
}

// ExtractCoverHandler runs one extract_cover job (ADR-0105): read the cover a book
// file carries — the EPUB's declared cover image, or the PDF's first page — and
// land it as the Work's artwork.
//
// # Outcomes
//
// A file that declares no cover, or that cannot be read or rendered, is a
// permanent answer about those bytes: it is recorded as 'none' and the job
// succeeds, so the beat does not open the file again. Only the environment
// failing — the source bytes missing, the store or the database erroring — is
// returned, because those are what a queue retry is for. Re-running is safe: the
// CAS dedups the image and the recorder converges on the same rows.
func ExtractCoverHandler(opts ExtractCoverHandlerOptions) HandlerFunc {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	return func(ctx context.Context, job jobs.Job) error {
		p, err := decodePayload[cover.Payload](job)
		if err != nil {
			return err
		}
		if p.BlobHash == "" || p.AssetID == "" {
			return errors.New("extract-cover: the payload names no blob or no asset")
		}
		src, err := opts.Source.SourcePath(ctx, p.BlobHash)
		if err != nil {
			return fmt.Errorf("extract-cover: locating %s: %w", p.BlobHash, err)
		}

		var (
			img    cover.Image
			format string
			found  error // a permanent "no cover in these bytes", or nil
		)
		switch p.MIME {
		case cover.MIMEEPUB:
			format = "epub"
			img, found, err = readEPUBCover(src)
		case cover.MIMEPDF:
			format = "pdf"
			if opts.Renderer == nil {
				// Unreachable through the queue — a PDF job requires pdftoppm — so
				// reaching it is a wiring fault worth a retry and a log line, not a
				// 'none' recorded against bytes that may well have a cover.
				return errors.New("extract-cover: a PDF job reached a worker with no renderer")
			}
			img, err = opts.Renderer.RenderFirstPage(ctx, src)
			if errors.Is(err, cover.ErrRender) {
				found, err = err, nil
			}
		default:
			format = p.MIME
			found = fmt.Errorf("no cover can be taken from %q", p.MIME)
		}
		if err != nil {
			return fmt.Errorf("extract-cover: %w", err)
		}

		if found != nil {
			log.Info("a book file yielded no cover", "blob", p.BlobHash, "format", format, "reason", found)
			if err := opts.Recorder.RecordCoverAttempt(ctx, p.BlobHash, format, found.Error(), now()); err != nil {
				return fmt.Errorf("extract-cover: recording the attempt: %w", err)
			}
			return nil
		}

		hash, size, err := opts.Blobs.Put(ctx, bytes.NewReader(img.Data))
		if err != nil {
			return fmt.Errorf("extract-cover: storing the cover: %w", err)
		}
		if err := opts.Recorder.RecordExtractedCover(ctx, p.AssetID, format, catalog.FetchedArtwork{
			BlobHash: hash, Size: size, MIME: img.MIME, Source: format,
		}, now()); err != nil {
			return fmt.Errorf("extract-cover: recording the cover: %w", err)
		}
		log.Info("extracted a book cover", "source_blob", p.BlobHash, "cover_blob", hash, "format", format, "size", size)
		return nil
	}
}

// readEPUBCover opens the EPUB and reads its cover. found is the permanent
// "these bytes have no usable cover" answer — every failure inside the container
// is one — and err is reserved for the file being unopenable.
func readEPUBCover(src string) (img cover.Image, found, err error) {
	f, err := os.Open(src) //nolint:gosec // a CAS path this node owns
	if err != nil {
		return cover.Image{}, nil, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return cover.Image{}, nil, err
	}
	img, cErr := cover.FromEPUB(f, st.Size())
	if cErr != nil {
		return cover.Image{}, cErr, nil
	}
	return img, nil, nil
}
