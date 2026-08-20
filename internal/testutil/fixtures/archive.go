package fixtures

import (
	"archive/zip"
	"bytes"
	"fmt"
)

// Unlike the media builders, these two are the real thing: an EPUB and a CBZ
// are ZIP archives, the ZIP format is fully implemented in the standard
// library, and the EPUB container rules are short enough to satisfy properly.
// archive_test.go opens both back with archive/zip and checks the parts of the
// OCF specification that matter.

// EPUBMediaType is the media type an EPUB's first archive entry must contain.
const EPUBMediaType = "application/epub+zip"

// newDeterministicZip returns a writer whose output depends only on the entries
// written to it.
//
// Every header deliberately leaves Modified zero, and the two writer methods
// behave differently in a way worth writing down, because it was measured
// rather than assumed:
//
//   - CreateHeader honours Modified and appends a 9-byte extended-timestamp
//     extra field. That makes the archive non-reproducible, and on the
//     `mimetype` entry it would also make the EPUB invalid, since OCF §3.3
//     requires that entry to carry no extra field at all.
//   - CreateRaw ignores Modified entirely — the timestamp is not written and no
//     extra field appears.
//
// So the `mimetype` entry is safe today only because addStored uses CreateRaw.
// Switching it to CreateHeader with a timestamp would break OCF silently, which
// is exactly what TestEPUBSatisfiesTheOCFContainerRules is there to catch.
// Leaving Modified zero everywhere is what makes the whole archive
// reproducible (ADR-0017), independently of which method wrote each entry.
func newDeterministicZip(buf *bytes.Buffer) *zip.Writer { return zip.NewWriter(buf) }

func addStored(w *zip.Writer, name string, body []byte) error {
	f, err := w.CreateRaw(&zip.FileHeader{
		Name:               name,
		Method:             zip.Store,
		CompressedSize64:   uint64(len(body)),
		UncompressedSize64: uint64(len(body)),
		CRC32:              crc32Of(body),
	})
	if err != nil {
		return err
	}
	_, err = f.Write(body)
	return err
}

func addDeflated(w *zip.Writer, name string, body []byte) error {
	f, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
	if err != nil {
		return err
	}
	_, err = f.Write(body)
	return err
}

// EPUB builds a valid EPUB 3 container: the stored `mimetype` entry first, the
// OCF `META-INF/container.xml`, a package document and one XHTML chapter.
func EPUB(title, author string, chapters int) ([]byte, error) {
	var buf bytes.Buffer
	w := newDeterministicZip(&buf)

	// OCF §3.3: the first entry must be `mimetype`, stored uncompressed, with
	// no extra field. Readers locate it by byte offset, so "first" is literal.
	if err := addStored(w, "mimetype", []byte(EPUBMediaType)); err != nil {
		return nil, err
	}

	container := `<?xml version="1.0" encoding="UTF-8"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>
`
	if err := addDeflated(w, "META-INF/container.xml", []byte(container)); err != nil {
		return nil, err
	}

	var manifest, spine bytes.Buffer
	for i := 1; i <= chapters; i++ {
		id := fmt.Sprintf("ch%02d", i)
		fmt.Fprintf(&manifest, `    <item id="%s" href="%s.xhtml" media-type="application/xhtml+xml"/>%s`, id, id, "\n")
		fmt.Fprintf(&spine, `    <itemref idref="%s"/>%s`, id, "\n")
	}

	// A fixed identifier and no dcterms:modified, because a generated fixture
	// that changes every run defeats the point of generating it.
	opf := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="pub-id">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:identifier id="pub-id">urn:uuid:00000000-0000-4000-8000-000000000000</dc:identifier>
    <dc:title>%s</dc:title>
    <dc:creator>%s</dc:creator>
    <dc:language>en</dc:language>
  </metadata>
  <manifest>
%s  </manifest>
  <spine>
%s  </spine>
</package>
`, escapeXML(title), escapeXML(author), manifest.String(), spine.String())
	if err := addDeflated(w, "OEBPS/content.opf", []byte(opf)); err != nil {
		return nil, err
	}

	for i := 1; i <= chapters; i++ {
		body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<html xmlns="http://www.w3.org/1999/xhtml"><head><title>%s — %d</title></head>
<body><h1>Chapter %d</h1><p>This is a Heyarr test fixture.</p></body></html>
`, escapeXML(title), i, i)
		if err := addDeflated(w, fmt.Sprintf("OEBPS/ch%02d.xhtml", i), []byte(body)); err != nil {
			return nil, err
		}
	}

	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// CBZ builds a comic archive: a ZIP of numbered images, which is the whole of
// the format.
func CBZ(title string, pages int) ([]byte, error) {
	var buf bytes.Buffer
	w := newDeterministicZip(&buf)
	for i := 1; i <= pages; i++ {
		name := fmt.Sprintf("%s-%03d.jpg", slug(title), i)
		if err := addStored(w, name, JPEG(fmt.Sprintf("%s page %d", title, i))); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SRT builds a SubRip subtitle track.
func SRT(lines int) []byte {
	var buf bytes.Buffer
	for i := 1; i <= lines; i++ {
		start, end := (i-1)*2, i*2
		fmt.Fprintf(&buf, "%d\n00:00:%02d,000 --> 00:00:%02d,000\nHeyarr fixture line %d\n\n", i, start, end, i)
	}
	return buf.Bytes()
}
