package cover_test

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/media"
	"github.com/rarebit-one/heyarr-core/internal/media/cover"
)

var pngBytes = []byte("\x89PNG\r\n\x1a\nnot-really-a-png")

// epub builds an EPUB whose OPF lives at OEBPS/content.opf, with the given OPF
// body inside <package> and the given extra members.
func epub(t *testing.T, opfBody string, members map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	write := func(name string, data []byte) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	write("mimetype", []byte("application/epub+zip"))
	write("META-INF/container.xml", []byte(`<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`))
	write("OEBPS/content.opf", []byte(`<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">`+opfBody+`</package>`))
	for name, data := range members {
		write(name, data)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFromEPUBFindsTheDeclaredCover(t *testing.T) {
	t.Parallel()
	img := map[string][]byte{"OEBPS/images/front.png": pngBytes}
	cases := []struct {
		name string
		opf  string
	}{
		{"epub3 cover-image property", `<manifest>
			<item id="x" href="text.xhtml" media-type="application/xhtml+xml"/>
			<item id="c" href="images/front.png" media-type="image/png" properties="cover-image"/>
		</manifest>`},
		{"epub2 meta name=cover", `<metadata><meta name="cover" content="img1"/></metadata><manifest>
			<item id="img1" href="images/front.png" media-type="image/png"/>
		</manifest>`},
		{"guide reference to an image", `<manifest>
			<item id="img1" href="images/front.png" media-type="image/png"/>
		</manifest><guide><reference type="cover" href="images/front.png"/></guide>`},
		{"percent-encoded href", `<manifest>
			<item id="c" href="images/%66ront.png" media-type="image/png" properties="cover-image"/>
		</manifest>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data := epub(t, tc.opf, img)
			got, err := cover.FromEPUB(bytes.NewReader(data), int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Data, pngBytes) || got.MIME != "image/png" {
				t.Fatalf("got %d bytes of %q, want the PNG", len(got.Data), got.MIME)
			}
		})
	}
}

func TestFromEPUBFallsBackToACoverNamedImage(t *testing.T) {
	t.Parallel()
	data := epub(t, `<manifest>
		<item id="a" href="images/plate1.jpg" media-type="image/jpeg"/>
		<item id="b" href="images/Cover.jpg" media-type="image/jpeg"/>
	</manifest>`, map[string][]byte{
		"OEBPS/images/plate1.jpg": []byte("plate"),
		"OEBPS/images/Cover.jpg":  []byte("cover"),
	})
	got, err := cover.FromEPUB(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Data) != "cover" {
		t.Fatalf("got %q, want the cover-named image, not the first image", got.Data)
	}
}

func TestFromEPUBReportsNoCover(t *testing.T) {
	t.Parallel()
	cases := map[string][]byte{
		"no image declared": epub(t, `<manifest><item id="x" href="t.xhtml" media-type="application/xhtml+xml"/></manifest>`, nil),
		"declared but absent": epub(t, `<manifest>
			<item id="c" href="images/gone.png" media-type="image/png" properties="cover-image"/></manifest>`, nil),
		"href escapes the container": epub(t, `<manifest>
			<item id="c" href="../../etc/passwd.png" media-type="image/png" properties="cover-image"/></manifest>`, nil),
		"svg cover is not served": epub(t, `<manifest>
			<item id="c" href="c.svg" media-type="image/svg+xml" properties="cover-image"/></manifest>`,
			map[string][]byte{"OEBPS/c.svg": []byte("<svg/>")}),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := cover.FromEPUB(bytes.NewReader(data), int64(len(data)))
			if !errors.Is(err, cover.ErrNoCover) {
				t.Fatalf("err = %v, want ErrNoCover", err)
			}
		})
	}
}

func TestFromEPUBRejectsANonContainer(t *testing.T) {
	t.Parallel()
	data := []byte("%PDF-1.4 definitely not a zip")
	if _, err := cover.FromEPUB(bytes.NewReader(data), int64(len(data))); err == nil {
		t.Fatal("want an error for bytes that are not a ZIP")
	}
}

func TestRequiredCapability(t *testing.T) {
	t.Parallel()
	if got := cover.RequiredCapability("application/pdf"); got != media.CapabilityPdftoppm {
		t.Errorf("PDF requires %q, want the toolchain's %q", got, media.CapabilityPdftoppm)
	}
	if got := cover.RequiredCapability("application/epub+zip"); got != "" {
		t.Errorf("EPUB requires %q, want nothing", got)
	}
	if !cover.Supported("Application/PDF; charset=binary") || cover.Supported("application/x-mobipocket-ebook") {
		t.Error("Supported disagrees with the two formats a cover is taken from")
	}
}

// minimalPDF is a one-page PDF with a filled rectangle — enough for pdftoppm to
// render something real. Offsets are computed so the xref table is valid.
func minimalPDF() []byte {
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 300] /Contents 4 0 R >>",
	}
	stream := "0 0 1 rg 20 20 160 260 re f"
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := []int{}
	for i, o := range objs {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	offsets = append(offsets, b.Len())
	fmt.Fprintf(&b, "4 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(stream), stream)
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 5\n0000000000 65535 f \n")
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size 5 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
	return b.Bytes()
}

func pdftoppm(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("pdftoppm")
	if err != nil {
		t.Skip("no pdftoppm on PATH (poppler-utils); the render round-trip needs the real binary")
	}
	return p
}

func TestRenderFirstPageWithTheRealTool(t *testing.T) {
	t.Parallel()
	bin := pdftoppm(t)
	src := filepath.Join(t.TempDir(), "book.pdf")
	if err := os.WriteFile(src, minimalPDF(), 0o600); err != nil {
		t.Fatal(err)
	}
	img, err := cover.PDFRenderer{Path: bin}.RenderFirstPage(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	if img.MIME != "image/jpeg" || !bytes.HasPrefix(img.Data, []byte{0xFF, 0xD8, 0xFF}) {
		t.Fatalf("got %d bytes of %q, want a JPEG", len(img.Data), img.MIME)
	}
}

func TestRenderFirstPageReportsADamagedPDFAsPermanent(t *testing.T) {
	t.Parallel()
	bin := pdftoppm(t)
	src := filepath.Join(t.TempDir(), "broken.pdf")
	if err := os.WriteFile(src, []byte("%PDF-1.4\nthis is not a pdf body"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := cover.PDFRenderer{Path: bin}.RenderFirstPage(t.Context(), src)
	if !errors.Is(err, cover.ErrRender) {
		t.Fatalf("err = %v, want ErrRender", err)
	}
}
