package worker

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/media/thumbnail"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// --- fakes ---

type fakeThumbnailSource struct {
	rc   io.ReadSeekCloser
	size int64
	err  error
}

func (f *fakeThumbnailSource) OpenThumbnailSource(_ context.Context, _ string) (io.ReadSeekCloser, int64, error) {
	return f.rc, f.size, f.err
}

type nopSeekCloser struct{ *bytes.Reader }

func (nopSeekCloser) Close() error { return nil }

type fakeThumbnailStore struct {
	hash string
	put  []byte
}

func (f *fakeThumbnailStore) Put(_ context.Context, r io.Reader) (string, int64, error) {
	b, err := io.ReadAll(r)
	f.put = b
	return f.hash, int64(len(b)), err
}

type fakeThumbnailRecorder struct {
	status      string
	thumbHash   string
	ready       []string
	undecodable []string
}

func (f *fakeThumbnailRecorder) ThumbnailStatus(_ context.Context, _ string, _, _ int) (string, string, error) {
	return f.status, f.thumbHash, nil
}

func (f *fakeThumbnailRecorder) RecordThumbnailReady(_ context.Context, _ string, _, _ int, thumbHash string, _ int64, _ time.Time) error {
	f.ready = append(f.ready, thumbHash)
	return nil
}

func (f *fakeThumbnailRecorder) RecordThumbnailUndecodable(_ context.Context, sourceHash string, _, _ int, _ time.Time) error {
	f.undecodable = append(f.undecodable, sourceHash)
	return nil
}

// --- image helpers ---

// makeJPEG encodes a minimal 4×4 JPEG in memory.
func makeJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := range 4 {
		for x := range 4 {
			img.Set(x, y, color.NRGBA{R: 128, G: 64, B: 32, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// makePNG encodes a minimal 4×4 PNG in memory.
func makePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// makeJPEGWithOrientation builds a JPEG with an injected APP1/EXIF segment that
// sets the Orientation tag to orient (e.g. 6 = 90° CW).
func makeJPEGWithOrientation(t *testing.T, orient uint16) []byte {
	t.Helper()
	base := makeJPEG(t)

	// TIFF payload (26 bytes, little-endian): byte-order mark, magic, IFD0
	// offset, entry count, one Orientation entry, next-IFD pointer.
	tiff := make([]byte, 26)
	copy(tiff[0:], "II")                             // little-endian
	binary.LittleEndian.PutUint16(tiff[2:], 42)      // TIFF magic
	binary.LittleEndian.PutUint32(tiff[4:], 8)       // IFD0 starts at offset 8
	binary.LittleEndian.PutUint16(tiff[8:], 1)       // one IFD entry
	binary.LittleEndian.PutUint16(tiff[10:], 0x0112) // tag: Orientation
	binary.LittleEndian.PutUint16(tiff[12:], 3)      // type: SHORT
	binary.LittleEndian.PutUint32(tiff[14:], 1)      // count: 1
	binary.LittleEndian.PutUint16(tiff[18:], orient) // value
	binary.LittleEndian.PutUint16(tiff[20:], 0)      // value padding
	binary.LittleEndian.PutUint32(tiff[22:], 0)      // next IFD: none

	// APP1 marker + length (2 + 6 + 26 = 34) + "Exif\x00\x00" + TIFF payload.
	app1 := make([]byte, 2+2+6+len(tiff))
	app1[0] = 0xFF
	app1[1] = 0xE1
	binary.BigEndian.PutUint16(app1[2:], uint16(2+6+len(tiff)))
	copy(app1[4:], "Exif\x00\x00")
	copy(app1[10:], tiff)

	// Insert APP1 between JPEG SOI (first 2 bytes) and the rest.
	out := make([]byte, 0, len(base)+len(app1))
	out = append(out, base[:2]...)
	out = append(out, app1...)
	out = append(out, base[2:]...)
	return out
}

// bigPNG returns a minimal well-formed PNG byte sequence whose IHDR reports a
// width of maxThumbnailDimension+1. image.DecodeConfig reads only the IHDR and
// does not need IDAT, so no pixel data is required.
func bigPNG() []byte {
	const w = maxThumbnailDimension + 1
	const h = 1

	// IHDR data (13 bytes).
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], uint32(w))
	binary.BigEndian.PutUint32(ihdr[4:], uint32(h))
	ihdr[8] = 8 // bit depth
	ihdr[9] = 2 // color type: RGB
	// compression, filter, interlace all zero

	// CRC32 of "IHDR" + ihdrData.
	checksum := crc32.ChecksumIEEE(append([]byte("IHDR"), ihdr...))

	var buf bytes.Buffer
	buf.Write([]byte("\x89PNG\r\n\x1a\n")) // PNG signature
	l := make([]byte, 4)
	binary.BigEndian.PutUint32(l, 13)
	buf.Write(l)
	buf.WriteString("IHDR")
	buf.Write(ihdr)
	cs := make([]byte, 4)
	binary.BigEndian.PutUint32(cs, checksum)
	buf.Write(cs)
	return buf.Bytes()
}

// minimalWebP is a lossless VP8L WebP taken from golang.org/x/image testdata
// (gopher-doc.1bpp.lossless.webp). Embedded as a literal so no file dependency
// is needed.
var minimalWebP = []byte(
	"\x52\x49\x46\x46\xb2\x01\x00\x00\x57\x45\x42\x50\x56\x50\x38\x4c\xa5\x01\x00\x00\x2f\x4a\xc0\x18\x00\x0f\x30\xff\xf3\x3f\xff\xf3" +
		"\x1f\x78\x90\x24\x6d\x7b\xda\x48\x6e\xe6\xf1\x0d\xc6\x7d\x84\x81\x25\xe9\x30\x43\x3b\x66\xfc\x87\x19\x96\x0c\x27\x99\x62\x26\x9f" +
		"\x60\x4a\xed\xa1\x66\x06\xd9\xd5\x8a\xbe\xaa\xff\xff\x15\x3a\x41\x44\xff\x19\xb8\x6d\xa4\xc8\xbb\xc7\x38\xf0\x0a\xc4\xa3\xaf\x81" +
		"\xdf\x31\x4a\x62\x59\xf7\xa6\xa0\xa5\x48\x22\x97\xd1\xb7\xa0\x15\x30\x17\x14\xe2\xd7\x1d\x2c\x85\xf1\xc0\x8d\x71\x91\x06\xe0\xec" +
		"\xb0\xb8\x0e\x0a\x55\x57\xc9\x0a\x20\x2b\x53\xb1\x80\x80\x92\x3c\xfa\x52\x4f\xfc\xe2\x8c\x4f\xf7\xc1\x02\x37\xaf\x83\x57\x18\x07" +
		"\xb6\x15\x90\x5b\x96\x81\xad\xa5\xc8\xf8\xb9\x23\x41\xc5\xcb\x96\x13\xa5\x62\x07\x83\x44\x59\xa6\x49\xe2\x45\x55\xbd\xa1\xd1\xc0" +
		"\x28\xec\x28\xb1\x6b\x8e\x19\xdc\x48\xca\x7d\x8e\xbd\xa0\x83\xbe\x18\x3f\xc1\xee\x93\xc1\xa7\x4f\x04\xf6\xea\x05\x5e\x7c\x32\xc2" +
		"\xe6\x30\x9f\x32\x66\x73\x96\x93\xc4\x91\xcf\x83\x7e\x42\x8c\x8f\x2f\xe3\x27\x6a\x6c\xcc\xbd\xc1\x35\xac\x73\x44\xaf\xdd\x45\xf4" +
		"\x62\x99\x3d\x55\x1c\x4b\xdc\x3b\x3e\x18\x47\xdf\xab\x2e\x07\xda\x8f\x79\x86\xff\xa0\xb9\x3a\x72\xe4\xe2\x27\x4c\x0e\x2b\x79\xb9" +
		"\x87\x57\x0a\x8d\x6e\x84\x55\x90\x98\x30\xae\xdd\xc5\xc2\x82\x05\xd8\x0f\xf4\x79\x0a\xaf\xd8\x24\x00\xed\x8f\xf0\x62\x99\x19\x65" +
		"\x5d\x20\x06\xad\x41\xaf\xb5\x20\x3a\x6d\xea\xac\xa8\xad\x5c\x1d\xcb\x4d\x71\x75\x6f\x09\x91\xf9\x3a\xc6\x31\x17\x99\x54\x10\xf8" +
		"\x74\x1d\x16\xbe\x8e\x2a\x12\x0d\xdf\x87\x57\x5a\xad\x3e\xd2\xaa\xfa\x10\x94\x82\x79\xe5\x4b\x1f\xdf\xa0\xbc\x64\xcb\xca\xa3\x3a" +
		"\xe4\xf4\x38\xe2\x28\x73\x95\x35\xf1\x40\xa8\xca\x6c\x0b\xec\x85\x78\x22\xaf\xb2\xe2\x97\xdc\x38\x2f\x66\xef\x33\x27\x26\x8d\x07" +
		"\x2a\x5d\xa3\x02\x3b\xa0\x65\x63\x6f\x22\xf8\x53\x8b\xcd\xb7\xc8\xd6\xf1\x2a\xc4\x08\x68\xb6\x87\x00\x00")

// --- job helper ---

func thumbnailJob(t *testing.T, sourceHash string, bucket int) jobs.Job {
	t.Helper()
	p, err := json.Marshal(thumbnail.Payload{SourceHash: sourceHash, Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}
	return jobs.Job{Payload: p}
}

// --- tests ---

// TestDeriveThumbnailOutcomes covers the derive_thumbnail handler in table-driven
// form: happy-path decoding for each supported format, bomb guards, transient
// peer-routing failure, and idempotency.
func TestDeriveThumbnailOutcomes(t *testing.T) {
	t.Parallel()

	const storeHash = "blake3:2222222222222222222222222222222222222222222222222222222222222222"
	const srcHash = "blake3:1111111111111111111111111111111111111111111111111111111111111111"

	bigPNGBytes := bigPNG()

	cases := []struct {
		name            string
		build           func(*testing.T) io.ReadSeekCloser
		srcSize         int64
		srcErr          error
		recStatus       string
		wantErr         bool
		wantPermanent   bool
		wantReady       int
		wantUndecodable int
	}{
		{
			name: "jpeg decoded and stored",
			build: func(t *testing.T) io.ReadSeekCloser {
				return nopSeekCloser{bytes.NewReader(makeJPEG(t))}
			},
			srcSize:   1000,
			wantReady: 1,
		},
		{
			name: "png decoded and stored",
			build: func(t *testing.T) io.ReadSeekCloser {
				return nopSeekCloser{bytes.NewReader(makePNG(t))}
			},
			srcSize:   1000,
			wantReady: 1,
		},
		{
			name: "webp decoded and stored",
			build: func(*testing.T) io.ReadSeekCloser {
				return nopSeekCloser{bytes.NewReader(minimalWebP)}
			},
			srcSize:   int64(len(minimalWebP)),
			wantReady: 1,
		},
		{
			name: "jpeg with exif orientation 6 decoded and stored",
			build: func(t *testing.T) io.ReadSeekCloser {
				return nopSeekCloser{bytes.NewReader(makeJPEGWithOrientation(t, 6))}
			},
			srcSize:   2000,
			wantReady: 1,
		},
		{
			name: "source exceeds size limit is marked undecodable permanently",
			// rc must be non-nil: the defer close is set up before the guard fires.
			build: func(*testing.T) io.ReadSeekCloser {
				return nopSeekCloser{bytes.NewReader(nil)}
			},
			srcSize:         maxThumbnailSourceBytes + 1,
			wantErr:         true,
			wantPermanent:   true,
			wantUndecodable: 1,
		},
		{
			name: "image dimension exceeds limit is marked undecodable permanently",
			build: func(*testing.T) io.ReadSeekCloser {
				return nopSeekCloser{bytes.NewReader(bigPNGBytes)}
			},
			srcSize:         int64(len(bigPNGBytes)),
			wantErr:         true,
			wantPermanent:   true,
			wantUndecodable: 1,
		},
		{
			name:    "cas.ErrNotFound is transient, not wrapped in ErrPermanent",
			srcErr:  cas.ErrNotFound,
			wantErr: true,
			// wantPermanent intentionally false
		},
		{
			name:      "already-ready status is a no-op",
			recStatus: "ready",
			build: func(*testing.T) io.ReadSeekCloser {
				// Should never be called; a non-nil reader defends against a
				// Close() panic if the handler misbehaves.
				return nopSeekCloser{bytes.NewReader(nil)}
			},
			// wantErr = false, wantReady = 0, wantUndecodable = 0
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var rc io.ReadSeekCloser
			if tc.srcErr == nil && tc.build != nil {
				rc = tc.build(t)
			}

			store := &fakeThumbnailStore{hash: storeHash}
			rec := &fakeThumbnailRecorder{status: tc.recStatus}
			src := &fakeThumbnailSource{rc: rc, size: tc.srcSize, err: tc.srcErr}

			h := DeriveThumbnailHandler(DeriveThumbnailHandlerOptions{
				Source:   src,
				Store:    store,
				Recorder: rec,
			})
			err := h(t.Context(), thumbnailJob(t, srcHash, 256))

			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantPermanent && !errors.Is(err, jobs.ErrPermanent) {
				t.Errorf("want jobs.ErrPermanent wrapping, got %v", err)
			}
			if !tc.wantPermanent && err != nil && errors.Is(err, jobs.ErrPermanent) {
				t.Errorf("got unexpected jobs.ErrPermanent: %v", err)
			}
			if got := len(rec.ready); got != tc.wantReady {
				t.Errorf("RecordThumbnailReady calls = %d, want %d", got, tc.wantReady)
			}
			if got := len(rec.undecodable); got != tc.wantUndecodable {
				t.Errorf("RecordThumbnailUndecodable calls = %d, want %d", got, tc.wantUndecodable)
			}
		})
	}
}
