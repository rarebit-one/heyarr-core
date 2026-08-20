package publication_test

import (
	"archive/zip"
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/publication"
	"github.com/rarebit-one/heyarr-core/internal/testutil/fixtures"
)

func TestFormatForExtension(t *testing.T) {
	for ext, want := range map[string]publication.Format{
		".epub": publication.FormatEPUB,
		".EPUB": publication.FormatEPUB,
		".pdf":  publication.FormatPDF,
		".cbz":  publication.FormatCBZ,
		".cbr":  publication.FormatCBR,
		// Books the identifier knows about (M1-11) that §69 does not name.
		// They are catalogued and served like anything else and simply have no
		// publication-specific handling — which is different from being
		// unrecognised, and worth pinning so nobody quietly adds them.
		".mobi": "",
		".azw3": "",
		".djvu": "",
		".mkv":  "",
		"":      "",
	} {
		if got := publication.FormatForExtension(ext); got != want {
			t.Errorf("%q = %q, want %q", ext, got, want)
		}
	}
}

// An EPUB's spine is its reading order, and the count comes from the container's
// own package document — followed from META-INF/container.xml rather than
// guessed at, because an EPUB may put the OPF anywhere.
func TestEPUBSpineCount(t *testing.T) {
	for _, chapters := range []int{1, 6, 40} {
		body, err := fixtures.EPUB("The Long Survey", "Ada Prentice", chapters)
		if err != nil {
			t.Fatal(err)
		}
		info, err := publication.Examine(bytes.NewReader(body), int64(len(body)), publication.FormatEPUB)
		if err != nil {
			t.Fatal(err)
		}
		if info.ChapterCount == nil || *info.ChapterCount != chapters {
			t.Errorf("chapters = %v, want %d", info.ChapterCount, chapters)
		}
		// A spine is not a page count and must not masquerade as one.
		if info.PageCount != nil {
			t.Errorf("an EPUB reported a page count of %d", *info.PageCount)
		}
	}
}

func TestCBZPageCount(t *testing.T) {
	for _, pages := range []int{1, 8, 64} {
		body, err := fixtures.CBZ("The Long Survey", pages)
		if err != nil {
			t.Fatal(err)
		}
		info, err := publication.Examine(bytes.NewReader(body), int64(len(body)), publication.FormatCBZ)
		if err != nil {
			t.Fatal(err)
		}
		if info.PageCount == nil || *info.PageCount != pages {
			t.Errorf("pages = %v, want %d", info.PageCount, pages)
		}
		if info.ChapterCount != nil {
			t.Errorf("a comic reported a chapter count of %d", *info.ChapterCount)
		}
	}
}

// Resource forks travel inside comic archives constantly. Counting them makes
// every affected comic report roughly twice its real length, which looks like a
// plausible number and is wrong.
func TestCBZIgnoresResourceForksAndMetadata(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, name := range []string{
		"page-001.jpg", "page-002.jpg", "page-003.jpg",
		"__MACOSX/._page-001.jpg", "._page-002.jpg",
		"ComicInfo.xml", "cover/", "notes.txt",
	} {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		// A name ending in "/" is a directory entry and cannot be written to.
		// It is in the list because a real comic archive has them and the
		// counter has to skip them.
		if strings.HasSuffix(name, "/") {
			continue
		}
		if _, err := f.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := publication.Examine(bytes.NewReader(buf.Bytes()), int64(buf.Len()), publication.FormatCBZ)
	if err != nil {
		t.Fatal(err)
	}
	if info.PageCount == nil || *info.PageCount != 3 {
		t.Errorf("pages = %v, want 3", info.PageCount)
	}
}

// PDF and CBR are stored, served and readable, and report no count. The refusal
// is the deliverable: reading a PDF's page tree needs a PDF parser and a CBR's
// index needs a RAR decoder, and both would be dependencies taken solely to
// produce a number, uncomfortably close to the line §69 draws.
func TestPDFAndCBRAreNotIndexed(t *testing.T) {
	for _, f := range []publication.Format{publication.FormatPDF, publication.FormatCBR} {
		if f.Indexed() {
			t.Errorf("%s claims to be indexed", f)
		}
		info, err := publication.Examine(strings.NewReader("%PDF-1.7"), 8, f)
		if !errors.Is(err, publication.ErrNotIndexed) {
			t.Errorf("%s returned %v, want ErrNotIndexed", f, err)
		}
		if info.Format != f {
			t.Errorf("the format was lost: %q", info.Format)
		}
		// Absent, not zero. "0 pages" would be a false statement about a
		// document Heyarr simply has not counted.
		if info.PageCount != nil || info.ChapterCount != nil {
			t.Errorf("%s reported counts: %+v", f, info)
		}
	}
}

// Heyarr stores bytes it cannot interpret — that is the premise. A malformed
// archive must produce absent metadata, never a failure that would make ingest
// reject the file.
func TestAMalformedContainerYieldsAbsentMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
		f    publication.Format
	}{
		{"not a zip at all", []byte("this is not an archive"), publication.FormatCBZ},
		{"truncated zip", func() []byte {
			b, _ := fixtures.CBZ("x", 4)
			return b[:len(b)/2]
		}(), publication.FormatCBZ},
		{"an epub with no container.xml", func() []byte {
			var buf bytes.Buffer
			w := zip.NewWriter(&buf)
			f, _ := w.Create("random.txt")
			_, _ = f.Write([]byte("x"))
			_ = w.Close()
			return buf.Bytes()
		}(), publication.FormatEPUB},
		{"an epub whose OPF is missing", func() []byte {
			var buf bytes.Buffer
			w := zip.NewWriter(&buf)
			f, _ := w.Create("META-INF/container.xml")
			_, _ = f.Write([]byte(`<container><rootfiles>` +
				`<rootfile full-path="nowhere/content.opf"/></rootfiles></container>`))
			_ = w.Close()
			return buf.Bytes()
		}(), publication.FormatEPUB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info, err := publication.Examine(bytes.NewReader(tc.body), int64(len(tc.body)), tc.f)
			if err == nil {
				t.Fatal("a malformed container was read successfully")
			}
			if info.Format != tc.f {
				t.Errorf("the format was lost on failure: %q", info.Format)
			}
			if info.PageCount != nil || info.ChapterCount != nil {
				t.Errorf("a malformed container produced counts: %+v", info)
			}
		})
	}
}

// A comic with no images is a zero-page comic, and that is a real answer —
// distinct from a PDF, whose count Heyarr does not read. A client rendering
// "0 pages" for the second has been told something false.
func TestAnEmptyArchiveIsZeroPagesNotAbsent(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := publication.Examine(bytes.NewReader(buf.Bytes()), int64(buf.Len()), publication.FormatCBZ)
	if err != nil {
		t.Fatal(err)
	}
	if info.PageCount == nil {
		t.Fatal("an empty comic reported an absent page count")
	}
	if *info.PageCount != 0 {
		t.Errorf("pages = %d, want 0", *info.PageCount)
	}
}

// An XML index from a file on someone's NAS is untrusted input.
func TestEntityExpansionIsNotPerformed(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, _ := w.Create("META-INF/container.xml")
	_, _ = f.Write([]byte(`<?xml version="1.0"?>
<!DOCTYPE container [
  <!ENTITY a "aaaaaaaaaa">
  <!ENTITY b "&a;&a;&a;&a;&a;&a;&a;&a;&a;&a;">
  <!ENTITY c "&b;&b;&b;&b;&b;&b;&b;&b;&b;&b;">
]>
<container><rootfiles><rootfile full-path="&c;"/></rootfiles></container>`))
	_ = w.Close()

	// The assertion that matters is that this RETURNS, promptly, rather than
	// expanding. Whether it errors or reads a literal is the decoder's
	// business; not allocating gigabytes is ours.
	info, err := publication.Examine(bytes.NewReader(buf.Bytes()), int64(buf.Len()), publication.FormatEPUB)
	if err == nil {
		t.Log("the entity was not expanded and the document parsed; no expansion occurred")
	}
	if info.ChapterCount != nil {
		t.Errorf("a hostile container produced a chapter count of %d", *info.ChapterCount)
	}
}
