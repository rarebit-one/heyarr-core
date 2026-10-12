package worker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png" // register PNG decoder
	"io"
	"log/slog"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register WebP decoder

	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/media/thumbnail"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/void-which-binds-go/hashing"
)

// ThumbnailSource opens a source blob's bytes for reading. On a node that does
// not hold the blob, it must return cas.ErrNotFound so the job can be retried
// rather than failed permanently. The int64 is the blob's byte count (from the
// CAS Descriptor), used for the decompression-bomb size guard.
type ThumbnailSource interface {
	OpenThumbnailSource(ctx context.Context, sourceHash string) (io.ReadSeekCloser, int64, error)
}

// ThumbnailBlobStore stores a derived thumbnail blob and returns its hash and
// size. CASArtworkStore satisfies this interface.
type ThumbnailBlobStore interface {
	Put(ctx context.Context, r io.Reader) (hash string, size int64, err error)
}

// ThumbnailRecorder records the outcome of a derive_thumbnail job. The Catalog
// satisfies this interface via its RecordThumbnailReady / RecordThumbnailUndecodable
// high-level wrappers (which open their own transactions and publish events).
type ThumbnailRecorder interface {
	RecordThumbnailReady(ctx context.Context, sourceHash string, bucket, recipe int, thumbHash string, size int64, now time.Time) error
	RecordThumbnailUndecodable(ctx context.Context, sourceHash string, bucket, recipe int, now time.Time) error
	ThumbnailStatus(ctx context.Context, sourceHash string, bucket, recipe int) (status, thumbHash string, err error)
}

// CASThumbnailSource adapts cas.Store for ThumbnailSource. The production
// implementation: Open returns cas.ErrNotFound when this node does not hold the
// blob, which the handler treats as a transient error so the job retries.
type CASThumbnailSource struct{ store cas.Store }

// NewCASThumbnailSource adapts a CAS store for thumbnail source reading.
func NewCASThumbnailSource(store cas.Store) *CASThumbnailSource {
	return &CASThumbnailSource{store: store}
}

var _ ThumbnailSource = (*CASThumbnailSource)(nil)

// OpenThumbnailSource opens the source blob for reading. Returns cas.ErrNotFound
// when the blob is absent from this node.
func (s *CASThumbnailSource) OpenThumbnailSource(ctx context.Context, sourceHash string) (io.ReadSeekCloser, int64, error) {
	h, err := hashing.Parse(sourceHash)
	if err != nil {
		return nil, 0, fmt.Errorf("thumbnail-source: parsing hash %q: %w", sourceHash, err)
	}
	rc, desc, err := s.store.Open(ctx, h)
	if err != nil {
		return nil, 0, err // propagates cas.ErrNotFound as-is
	}
	return rc, desc.Size, nil
}

// DeriveThumbnailHandlerOptions configures the derive_thumbnail handler.
type DeriveThumbnailHandlerOptions struct {
	Source   ThumbnailSource
	Store    ThumbnailBlobStore
	Recorder ThumbnailRecorder
	Now      func() time.Time
	Logger   *slog.Logger
}

// maxThumbnailSourceBytes is the source-file size limit. A source larger than
// this is recorded as undecodable without attempting to decode it — a 64 MiB
// JPEG or PNG is already a decompression-bomb red flag.
const maxThumbnailSourceBytes = 64 << 20 // 64 MiB

// maxThumbnailPixels is the pixel-area bomb guard: width×height must not
// exceed this value.
const maxThumbnailPixels = 40_000_000 // 40 MP

// maxThumbnailDimension is the per-side bomb guard: neither width nor height
// may exceed this value.
const maxThumbnailDimension = 16384

// thumbnailJPEGQuality is the JPEG encoding quality for derived thumbnails.
// 82 is perceptually near-lossless for poster-sized images at web resolutions.
const thumbnailJPEGQuality = 82

// DeriveThumbnailHandler runs one derive_thumbnail job (ADR-0109).
//
// # Peer routing
//
// The handler calls ThumbnailSource.OpenThumbnailSource, which returns
// cas.ErrNotFound when the source blob is absent from the executing node. The
// handler treats this as a transient (non-permanent) error so the job stays
// in the queue and retries on the next available worker. In the current
// single-node deployment the API server and the worker are co-located with the
// CAS, so a job enqueued by a request handler is always retried on the same
// node that holds the bytes. This comment is the explicit documentation
// required by ADR-0109 §Peer-routing.
//
// # Re-running safety
//
// If the source blob has already been decoded and the derived blob stored, the
// job is a no-op: idempotency is ensured by checking ThumbnailStatus first.
// CAS Put is content-addressed, so storing the same JPEG twice converges to
// one blob. RecordThumbnailReady upserts on (source_hash, bucket, recipe).
func DeriveThumbnailHandler(opts DeriveThumbnailHandlerOptions) HandlerFunc {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	return func(ctx context.Context, job jobs.Job) error {
		p, err := decodePayload[thumbnail.Payload](job)
		if err != nil {
			return err
		}
		if p.SourceHash == "" {
			return errors.New("derive-thumbnail: payload names no source hash")
		}
		if !thumbnail.ValidBucket(p.Bucket) {
			return fmt.Errorf("derive-thumbnail: invalid bucket %d in payload", p.Bucket)
		}

		// Idempotency: if this (source, bucket, recipe) is already terminal,
		// there is nothing to do. A job re-run after a node restart converges
		// to the same answer rather than re-deriving.
		status, _, err := opts.Recorder.ThumbnailStatus(ctx, p.SourceHash, p.Bucket, thumbnail.Recipe)
		if err != nil {
			return fmt.Errorf("derive-thumbnail: reading thumbnail status: %w", err)
		}
		if status == "ready" || status == "undecodable" {
			log.Info("derive-thumbnail: skipping, terminal status already recorded",
				"source", p.SourceHash, "bucket", p.Bucket, "status", status)
			return nil
		}

		// Open the source blob. cas.ErrNotFound means the bytes are not on
		// this node — return a transient error so the job retries (see
		// Peer routing above). Other errors are environment failures.
		rc, size, err := opts.Source.OpenThumbnailSource(ctx, p.SourceHash)
		if err != nil {
			if errors.Is(err, cas.ErrNotFound) {
				// Transient: the bytes are not local. The job will retry.
				return fmt.Errorf("derive-thumbnail: source blob %s not on this node: %w", p.SourceHash, err)
			}
			return fmt.Errorf("derive-thumbnail: opening source blob %s: %w", p.SourceHash, err)
		}
		defer func() { _ = rc.Close() }()

		// Decompression-bomb guard: refuse if the raw source is too large.
		if size > maxThumbnailSourceBytes {
			log.Info("derive-thumbnail: source exceeds size limit, marking undecodable",
				"source", p.SourceHash, "size", size, "limit", maxThumbnailSourceBytes)
			if recErr := opts.Recorder.RecordThumbnailUndecodable(ctx, p.SourceHash, p.Bucket, thumbnail.Recipe, now()); recErr != nil {
				return fmt.Errorf("derive-thumbnail: recording undecodable (size bomb) for %s: %w", p.SourceHash, recErr)
			}
			return jobs.ErrPermanent
		}

		// Probe the image dimensions before a full decode so we can bail on
		// decompression bombs without allocating the full pixel buffer.
		cfg, fmtName, err := image.DecodeConfig(rc)
		if err != nil {
			log.Info("derive-thumbnail: source cannot be decoded, marking undecodable",
				"source", p.SourceHash, "error", err)
			if recErr := opts.Recorder.RecordThumbnailUndecodable(ctx, p.SourceHash, p.Bucket, thumbnail.Recipe, now()); recErr != nil {
				return fmt.Errorf("derive-thumbnail: recording undecodable (decode config) for %s: %w", p.SourceHash, recErr)
			}
			return jobs.ErrPermanent
		}
		if cfg.Width > maxThumbnailDimension || cfg.Height > maxThumbnailDimension ||
			int64(cfg.Width)*int64(cfg.Height) > maxThumbnailPixels {
			log.Info("derive-thumbnail: source dimensions exceed limit, marking undecodable",
				"source", p.SourceHash, "width", cfg.Width, "height", cfg.Height)
			if recErr := opts.Recorder.RecordThumbnailUndecodable(ctx, p.SourceHash, p.Bucket, thumbnail.Recipe, now()); recErr != nil {
				return fmt.Errorf("derive-thumbnail: recording undecodable (pixel bomb) for %s: %w", p.SourceHash, recErr)
			}
			return jobs.ErrPermanent
		}

		// Seek back to the beginning for the full decode.
		if _, err := rc.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("derive-thumbnail: seeking source blob %s: %w", p.SourceHash, err)
		}

		src, _, err := image.Decode(rc)
		if err != nil {
			log.Info("derive-thumbnail: source image decode failed, marking undecodable",
				"source", p.SourceHash, "format", fmtName, "error", err)
			if recErr := opts.Recorder.RecordThumbnailUndecodable(ctx, p.SourceHash, p.Bucket, thumbnail.Recipe, now()); recErr != nil {
				return fmt.Errorf("derive-thumbnail: recording undecodable (full decode) for %s: %w", p.SourceHash, recErr)
			}
			return jobs.ErrPermanent
		}

		// EXIF orientation correction: applicable to JPEG only. Seek back and
		// read the EXIF orientation, then apply the corresponding transform.
		if fmtName == "jpeg" {
			if _, err := rc.Seek(0, io.SeekStart); err != nil {
				return fmt.Errorf("derive-thumbnail: seeking for EXIF read on %s: %w", p.SourceHash, err)
			}
			orient, exifErr := readJPEGOrientation(rc)
			if exifErr == nil && orient > 1 {
				src = applyOrientation(src, orient)
			}
		}

		// Scale to the bucket (long edge ≤ bucket px; never upscale).
		thumb := scaleToBucket(src, p.Bucket)

		// Encode as JPEG.
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, thumb, &jpeg.Options{Quality: thumbnailJPEGQuality}); err != nil {
			return fmt.Errorf("derive-thumbnail: encoding thumbnail for %s bucket %d: %w", p.SourceHash, p.Bucket, err)
		}

		// Store the derived blob in the CAS. Because CAS is content-addressed
		// the call is idempotent: storing the same bytes twice returns the same
		// hash and simply increments the store's internal refcount (or is a
		// no-op).
		thumbHash, thumbSize, err := opts.Store.Put(ctx, &buf)
		if err != nil {
			return fmt.Errorf("derive-thumbnail: storing thumbnail for %s bucket %d: %w", p.SourceHash, p.Bucket, err)
		}

		if err := opts.Recorder.RecordThumbnailReady(ctx, p.SourceHash, p.Bucket, thumbnail.Recipe, thumbHash, thumbSize, now()); err != nil {
			return fmt.Errorf("derive-thumbnail: recording ready for %s bucket %d: %w", p.SourceHash, p.Bucket, err)
		}

		log.Info("derive-thumbnail: thumbnail ready",
			"source", p.SourceHash, "bucket", p.Bucket, "thumb", thumbHash, "format", fmtName)
		return nil
	}
}

// scaleToBucket returns img scaled so its long edge is at most bucket pixels.
// If the image already fits (both dimensions ≤ bucket) it is returned unchanged.
// The scaler is Catmull-Rom (golang.org/x/image/draw).
func scaleToBucket(img image.Image, bucket int) image.Image {
	bounds := img.Bounds()
	w := bounds.Dx()
	h := bounds.Dy()
	if w <= bucket && h <= bucket {
		return img
	}
	var dw, dh int
	if w >= h {
		dw = bucket
		dh = (h * bucket) / w
		if dh < 1 {
			dh = 1
		}
	} else {
		dh = bucket
		dw = (w * bucket) / h
		if dw < 1 {
			dw = 1
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, bounds, draw.Over, nil)
	return dst
}

// applyOrientation applies the EXIF orientation transform to img, returning a
// new image with the orientation corrected. orient must be in the range 1–8;
// orient 1 is identity (no transform). Pixel-by-pixel to avoid a third-party
// imaging library dependency.
//
// EXIF orientation values:
//
//	1 — normal (top-left)
//	2 — mirror horizontal
//	3 — rotate 180°
//	4 — mirror vertical
//	5 — transpose (mirror horizontal, then rotate 90° CW) = mirror across main diagonal
//	6 — rotate 90° CW
//	7 — transverse (mirror horizontal, then rotate 90° CCW) = mirror across anti-diagonal
//	8 — rotate 90° CCW (= rotate 270° CW)
func applyOrientation(img image.Image, orient int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()

	// Orientations 5–8 swap width and height.
	transposed := orient >= 5
	var dst *image.NRGBA
	if transposed {
		dst = image.NewNRGBA(image.Rect(0, 0, h, w))
	} else {
		dst = image.NewNRGBA(image.Rect(0, 0, w, h))
	}

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.NRGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA)
			var dx, dy int
			switch orient {
			case 2: // mirror horizontal
				dx, dy = w-1-x, y
			case 3: // rotate 180°
				dx, dy = w-1-x, h-1-y
			case 4: // mirror vertical
				dx, dy = x, h-1-y
			case 5: // transpose
				dx, dy = y, x
			case 6: // 90° CW
				dx, dy = h-1-y, x
			case 7: // transverse
				dx, dy = h-1-y, w-1-x
			case 8: // 90° CCW
				dx, dy = y, w-1-x
			default: // 1 — identity
				dx, dy = x, y
			}
			dst.SetNRGBA(dx, dy, c)
		}
	}
	return dst
}

// readJPEGOrientation parses the EXIF orientation tag (0x0112) from a JPEG
// stream without a third-party EXIF library. It scans JPEG markers until it
// finds APP1 (0xFFE1), verifies the "Exif\x00\x00" magic, parses the TIFF
// header and IFD0, and returns the orientation value (1–8). If no orientation
// is found, it returns 1 (identity). An error is returned only on genuine IO
// failures; a missing or malformed APP1 is silently treated as orientation 1.
func readJPEGOrientation(r io.Reader) (int, error) {
	buf := make([]byte, 2)
	// Skip the SOI marker (0xFFD8).
	if _, err := io.ReadFull(r, buf); err != nil {
		return 1, err
	}
	if buf[0] != 0xFF || buf[1] != 0xD8 {
		return 1, nil // not a JPEG
	}

	for {
		if _, err := io.ReadFull(r, buf); err != nil {
			return 1, nil //nolint:nilerr // EOF or short read: no EXIF
		}
		if buf[0] != 0xFF {
			return 1, nil // lost sync
		}
		marker := buf[1]

		// Markers without a length field.
		if marker == 0xD8 || marker == 0xD9 || (marker >= 0xD0 && marker <= 0xD7) {
			continue
		}

		// Read the 2-byte length (includes the 2 bytes of the length itself).
		lenBuf := make([]byte, 2)
		if _, err := io.ReadFull(r, lenBuf); err != nil {
			return 1, nil //nolint:nilerr
		}
		segLen := int(lenBuf[0])<<8 | int(lenBuf[1])
		if segLen < 2 {
			return 1, nil
		}
		payloadLen := segLen - 2
		payload := make([]byte, payloadLen)
		if _, err := io.ReadFull(r, payload); err != nil {
			return 1, nil //nolint:nilerr
		}

		// We are looking for APP1 (0xFFE1) with an Exif header.
		if marker != 0xE1 || payloadLen < 8 {
			// Stop scanning once we are past the headers into image data.
			if marker == 0xDA { // SOS — start of scan
				return 1, nil
			}
			continue
		}

		// Verify "Exif\x00\x00" prefix.
		if string(payload[:6]) != "Exif\x00\x00" {
			continue
		}
		tiff := payload[6:]
		if len(tiff) < 8 {
			return 1, nil
		}

		// TIFF byte order: "II" = little-endian, "MM" = big-endian.
		var order binary.ByteOrder
		switch string(tiff[:2]) {
		case "II":
			order = binary.LittleEndian
		case "MM":
			order = binary.BigEndian
		default:
			return 1, nil
		}

		// IFD0 offset from the start of the TIFF header.
		ifd0Offset := int(order.Uint32(tiff[4:8]))
		if ifd0Offset+2 > len(tiff) {
			return 1, nil
		}

		// Number of IFD entries.
		entryCount := int(order.Uint16(tiff[ifd0Offset : ifd0Offset+2]))
		for i := 0; i < entryCount; i++ {
			entryStart := ifd0Offset + 2 + i*12
			if entryStart+12 > len(tiff) {
				break
			}
			tag := order.Uint16(tiff[entryStart : entryStart+2])
			if tag != 0x0112 { // Orientation
				continue
			}
			// Type (2 bytes) + count (4 bytes) + value (4 bytes).
			// Orientation is SHORT (type 3), count 1; value is in the first 2 bytes.
			val := int(order.Uint16(tiff[entryStart+8 : entryStart+10]))
			if val < 1 || val > 8 {
				return 1, nil
			}
			return val, nil
		}

		return 1, nil
	}
}
