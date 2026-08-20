// Package publication reads what a publication container declares about itself
// (spec §69).
//
// # Heyarr stores and serves publications. It does not render them.
//
// §69 is one of the shortest sections in the spec and one of the easiest to
// violate. Heyarr manages metadata, storage, replication, access and reading
// state for EPUB, PDF, CBZ and CBR. Clients render. Anything that starts
// rasterising a page, extracting an image or laying out text is in the wrong
// repository, and there is a test asserting no such library is importable.
//
// # Reading an index is not rendering
//
// The line this package sits on: an EPUB's OPF spine and a CBZ's ZIP central
// directory are *manifests the container publishes about itself*, and reading
// one is the same kind of act as reading an MP4's `moov`. Turning page 47 into
// pixels is not.
//
// That line has a visible cost, and the cost is the honest part:
//
//   - EPUB and CBZ are ZIP containers, so archive/zip reads their indexes with
//     no dependency at all.
//   - PDF's page count lives in its page tree, which needs a PDF parser. CBR is
//     RAR, which needs a RAR decoder. Both would be a dependency taken solely
//     to produce a number, and both sit uncomfortably close to the line §69
//     draws.
//
// So PDF and CBR are catalogued, stored, served and readable, and report **no
// page count**. A container that does not declare one reports absent rather
// than a guess (§69's "Heyarr manages metadata" does not say "Heyarr invents
// it"). Should that become a real complaint, the answer is a client that
// counts, or an external specialist per §83 — not a PDF parser in here.
package publication

import (
	"archive/zip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Format is a publication container Heyarr recognises (§69).
type Format string

const (
	// FormatEPUB is a ZIP containing an OPF package document.
	FormatEPUB Format = "epub"
	// FormatPDF is stored and served; its page count is not read. See above.
	FormatPDF Format = "pdf"
	// FormatCBZ is a ZIP of page images.
	FormatCBZ Format = "cbz"
	// FormatCBR is a RAR of page images. Stored and served; not indexed.
	FormatCBR Format = "cbr"
)

// Formats is every recognised format, in a stable order.
func Formats() []Format { return []Format{FormatEPUB, FormatPDF, FormatCBZ, FormatCBR} }

// FormatForExtension maps a lowercased extension, including the dot, to a
// format. The empty Format means "not a publication".
//
// §69 names exactly four containers and this recognises exactly those four.
// The identifier knows about .mobi, .azw and .djvu as *books* (M1-11), which is
// a different question: those are catalogued and served like any other file,
// and simply have no publication-specific handling.
func FormatForExtension(ext string) Format {
	switch strings.ToLower(ext) {
	case ".epub":
		return FormatEPUB
	case ".pdf":
		return FormatPDF
	case ".cbz":
		return FormatCBZ
	case ".cbr":
		return FormatCBR
	default:
		return ""
	}
}

// Indexed reports whether this format's own manifest is readable without
// taking a dependency Heyarr will not take.
func (f Format) Indexed() bool { return f == FormatEPUB || f == FormatCBZ }

// Info is what a container declares about itself.
//
// The counts are pointers because absent and zero are different answers: a
// malformed CBZ with no images is a zero-page comic, and a PDF is a publication
// whose page count Heyarr does not read. A client rendering "0 pages" for the
// second has been told something false.
type Info struct {
	Format Format
	// PageCount is the number of page images in a comic archive.
	PageCount *int
	// ChapterCount is the number of spine items in an EPUB — its reading
	// order, which is the closest thing an EPUB has to a page count and is not
	// the same thing.
	ChapterCount *int
}

// ErrNotIndexed means the format is recognised and deliberately not indexed.
var ErrNotIndexed = errors.New("publication: this format's index is not read")

// maxZipEntries bounds how many entries are counted.
//
// A zip is a decompression bomb primitive: an attacker-supplied archive can
// declare millions of entries in a few kilobytes of central directory, and
// counting them all is an allocation the caller did not agree to. Heyarr
// ingests files the operator points it at, so this is a guard rather than a
// defence — but "we only ingest trusted files" is exactly the assumption that
// stops being true when a library is shared.
const maxZipEntries = 100_000

// pageExts are the image types a comic archive's pages are stored as.
var pageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true,
	".webp": true, ".bmp": true, ".avif": true,
}

// Examine reads a container's own manifest.
//
// It never decodes an image, lays out text, or opens anything that is not an
// index. A container it cannot read is not an error the caller should act on:
// Heyarr stores bytes it cannot interpret, and that is the entire premise
// (ADR-0020's linked assets, §14's immutability, this).
func Examine(r io.ReaderAt, size int64, format Format) (Info, error) {
	info := Info{Format: format}
	if !format.Indexed() {
		return info, ErrNotIndexed
	}

	zr, err := zip.NewReader(r, size)
	if err != nil {
		return info, fmt.Errorf("publication: reading the %s container: %w", format, err)
	}
	if len(zr.File) > maxZipEntries {
		return info, fmt.Errorf("publication: the archive declares %d entries, more than %d",
			len(zr.File), maxZipEntries)
	}

	switch format {
	case FormatCBZ:
		n := countPages(zr)
		info.PageCount = &n
	case FormatEPUB:
		n, err := countSpineItems(zr)
		if err != nil {
			return info, err
		}
		info.ChapterCount = &n
	case FormatPDF, FormatCBR:
		// Unreachable: Indexed() already excluded these.
	}
	return info, nil
}

// countPages counts image entries, ignoring directories and metadata files
// like ComicInfo.xml. It reads the central directory only — no entry is opened
// and nothing is decompressed.
func countPages(zr *zip.Reader) int {
	n := 0
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := f.Name
		if i := strings.LastIndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		}
		// macOS resource forks travel inside comic archives constantly and are
		// not pages. Counting them makes every affected comic report roughly
		// twice its real length.
		if strings.HasPrefix(name, "._") || strings.HasPrefix(f.Name, "__MACOSX/") {
			continue
		}
		if i := strings.LastIndexByte(name, '.'); i >= 0 && pageExts[strings.ToLower(name[i:])] {
			n++
		}
	}
	return n
}

// container is META-INF/container.xml, which points at the OPF package
// document. An EPUB may put the OPF anywhere, so this is followed rather than
// guessed at.
type container struct {
	Rootfiles []struct {
		FullPath string `xml:"full-path,attr"`
	} `xml:"rootfiles>rootfile"`
}

// packageDoc is the fragment of the OPF this reads: the spine, which is the
// document's reading order. Nothing else in the OPF is touched — the metadata
// in there is Milestone 3's problem, and reading it now would be identification
// smuggled into the wrong milestone.
type packageDoc struct {
	Spine struct {
		Items []struct {
			IDRef string `xml:"idref,attr"`
		} `xml:"itemref"`
	} `xml:"spine"`
}

// maxXMLBytes bounds an index document. A container.xml is a few hundred bytes
// and an OPF is a few kilobytes; anything past this is not one.
const maxXMLBytes = 8 << 20

func countSpineItems(zr *zip.Reader) (int, error) {
	var c container
	if err := readXML(zr, "META-INF/container.xml", &c); err != nil {
		return 0, fmt.Errorf("publication: reading the EPUB container index: %w", err)
	}
	if len(c.Rootfiles) == 0 {
		return 0, errors.New("publication: the EPUB names no package document")
	}

	var pkg packageDoc
	if err := readXML(zr, c.Rootfiles[0].FullPath, &pkg); err != nil {
		return 0, fmt.Errorf("publication: reading the EPUB package document: %w", err)
	}
	return len(pkg.Spine.Items), nil
}

// readXML decodes one bounded entry.
func readXML(zr *zip.Reader, name string, into any) error {
	f, err := zr.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	// LimitReader rather than trusting the declared uncompressed size, which an
	// archive can lie about — that lie is the whole zip-bomb technique.
	body, err := io.ReadAll(io.LimitReader(f, maxXMLBytes))
	if err != nil {
		return err
	}
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	// No entity expansion and no external references: an XML index from a file
	// on someone's NAS is untrusted input, and billion-laughs is the oldest
	// trick there is.
	dec.Strict = false
	dec.Entity = map[string]string{}
	return dec.Decode(into)
}
