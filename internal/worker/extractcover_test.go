package worker

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/media/cover"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

type fakeCoverSource struct{ path string }

func (f fakeCoverSource) SourcePath(context.Context, string) (string, error) { return f.path, nil }

type fakeCoverBlobs struct{ put []byte }

func (f *fakeCoverBlobs) Put(_ context.Context, r io.Reader) (string, int64, error) {
	b, err := io.ReadAll(r)
	f.put = b
	return "blake3:cover", int64(len(b)), err
}

type fakeCoverRecorder struct {
	extracted []catalog.FetchedArtwork
	none      []string
}

func (f *fakeCoverRecorder) RecordExtractedCover(_ context.Context, _, _ string, art catalog.FetchedArtwork, _ time.Time) error {
	f.extracted = append(f.extracted, art)
	return nil
}

func (f *fakeCoverRecorder) RecordCoverAttempt(_ context.Context, _, _, detail string, _ time.Time) error {
	f.none = append(f.none, detail)
	return nil
}

type fakeRenderer struct {
	img cover.Image
	err error
}

func (f fakeRenderer) RenderFirstPage(context.Context, string) (cover.Image, error) {
	return f.img, f.err
}

func coverJob(t *testing.T, mime string) jobs.Job {
	t.Helper()
	p, err := json.Marshal(cover.Payload{BlobHash: "blake3:book", AssetID: "a-1", MIME: mime})
	if err != nil {
		t.Fatal(err)
	}
	return jobs.Job{Payload: p}
}

func writeEPUB(t *testing.T, withCover bool) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name, body string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("META-INF/container.xml", `<container><rootfiles><rootfile full-path="content.opf"/></rootfiles></container>`)
	manifest := `<item id="t" href="t.xhtml" media-type="application/xhtml+xml"/>`
	if withCover {
		manifest += `<item id="c" href="cover.jpg" media-type="image/jpeg" properties="cover-image"/>`
		add("cover.jpg", "jpeg-bytes")
	}
	add("content.opf", `<package><manifest>`+manifest+`</manifest></package>`)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "book.epub")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtractCoverOutcomes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		mime      string
		src       func(t *testing.T) string
		renderer  PageRenderer
		wantCover string // the bytes stored, "" when none
		wantNone  bool   // recorded as yielding no cover
		wantErr   bool   // returned for a retry
		wantMIME  string
	}{
		{
			name: "epub with a cover", mime: cover.MIMEEPUB,
			src: func(t *testing.T) string { return writeEPUB(t, true) }, wantCover: "jpeg-bytes", wantMIME: "image/jpeg",
		},
		{
			name: "epub without a cover is recorded, not retried", mime: cover.MIMEEPUB,
			src: func(t *testing.T) string { return writeEPUB(t, false) }, wantNone: true,
		},
		{
			name: "bytes that are not a zip are recorded, not retried", mime: cover.MIMEEPUB,
			src: func(t *testing.T) string {
				p := filepath.Join(t.TempDir(), "x")
				_ = os.WriteFile(p, []byte("nope"), 0o600)
				return p
			}, wantNone: true,
		},
		{
			name: "missing source file is retried", mime: cover.MIMEEPUB,
			src: func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone") }, wantErr: true,
		},
		{
			name: "pdf rendered", mime: cover.MIMEPDF, src: func(*testing.T) string { return "/x.pdf" },
			renderer:  fakeRenderer{img: cover.Image{Data: []byte("page1"), MIME: "image/jpeg"}},
			wantCover: "page1", wantMIME: "image/jpeg",
		},
		{
			name: "pdf the tool cannot render is recorded", mime: cover.MIMEPDF, src: func(*testing.T) string { return "/x.pdf" },
			renderer: fakeRenderer{err: errors.Join(cover.ErrRender, errors.New("encrypted"))}, wantNone: true,
		},
		{
			name: "pdf tool failing to start is retried", mime: cover.MIMEPDF, src: func(*testing.T) string { return "/x.pdf" },
			renderer: fakeRenderer{err: errors.New("exec: permission denied")}, wantErr: true,
		},
		{
			name: "pdf on a worker with no renderer is retried, not recorded", mime: cover.MIMEPDF,
			src: func(*testing.T) string { return "/x.pdf" }, wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			blobs := &fakeCoverBlobs{}
			rec := &fakeCoverRecorder{}
			h := ExtractCoverHandler(ExtractCoverHandlerOptions{
				Source: fakeCoverSource{path: tc.src(t)}, Blobs: blobs, Recorder: rec, Renderer: tc.renderer,
			})
			err := h(t.Context(), coverJob(t, tc.mime))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got := len(rec.none) > 0; got != tc.wantNone {
				t.Errorf("recorded none = %v (%v), want %v", got, rec.none, tc.wantNone)
			}
			if tc.wantCover == "" {
				if len(rec.extracted) != 0 {
					t.Errorf("recorded a cover %+v, want none", rec.extracted)
				}
				return
			}
			if string(blobs.put) != tc.wantCover || len(rec.extracted) != 1 || rec.extracted[0].MIME != tc.wantMIME {
				t.Errorf("stored %q, recorded %+v; want %q as %s", blobs.put, rec.extracted, tc.wantCover, tc.wantMIME)
			}
		})
	}
}
