package fixtures

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func openZip(t *testing.T, data []byte) *zip.Reader {
	t.Helper()
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("opening archive: %v", err)
	}
	return r
}

func readEntry(t *testing.T, r *zip.Reader, name string) []byte {
	t.Helper()
	f, err := r.Open(name)
	if err != nil {
		t.Fatalf("opening %s: %v", name, err)
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return body
}

// OCF §3.3. All three properties matter and all three are easy to lose: readers
// locate the media type by byte offset, so it must be first, stored, and carry
// no extra field.
func TestEPUBSatisfiesTheOCFContainerRules(t *testing.T) {
	data, err := EPUB("The Long Survey", "Ada Prentice", 3)
	if err != nil {
		t.Fatal(err)
	}
	r := openZip(t, data)

	if len(r.File) == 0 {
		t.Fatal("the archive is empty")
	}
	first := r.File[0]
	if first.Name != "mimetype" {
		t.Fatalf("first entry = %q, want mimetype", first.Name)
	}
	if first.Method != zip.Store {
		t.Errorf("mimetype is compressed (method %d), want stored", first.Method)
	}
	if len(first.Extra) != 0 {
		t.Errorf("mimetype carries a %d-byte extra field, which OCF forbids", len(first.Extra))
	}
	if got := string(readEntry(t, r, "mimetype")); got != EPUBMediaType {
		t.Errorf("mimetype = %q, want %q", got, EPUBMediaType)
	}
}

func TestEPUBContainerAndPackageAreWellFormedXML(t *testing.T) {
	data, err := EPUB("Title & Trouble", "O'Brien, Ada", 4)
	if err != nil {
		t.Fatal(err)
	}
	r := openZip(t, data)

	for _, name := range []string{"META-INF/container.xml", "OEBPS/content.opf"} {
		body := readEntry(t, r, name)
		// Decoding the whole document is what catches an unescaped ampersand in
		// a title, which is exactly the bug a fixture generator introduces the
		// first time someone puts punctuation in a name.
		dec := xml.NewDecoder(bytes.NewReader(body))
		for {
			_, err := dec.Token()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("%s is not well-formed XML: %v\n%s", name, err, body)
			}
		}
	}

	opf := string(readEntry(t, r, "OEBPS/content.opf"))
	if !strings.Contains(opf, "Title &amp; Trouble") {
		t.Errorf("the title was not XML-escaped in the package document:\n%s", opf)
	}
	for i := 1; i <= 4; i++ {
		if !strings.Contains(opf, `idref="ch0`) {
			t.Errorf("the spine has no chapter references:\n%s", opf)
			break
		}
	}
	// Every manifest item must actually exist, or the EPUB is broken in a way
	// no XML check would catch.
	for i := 1; i <= 4; i++ {
		name := "OEBPS/ch0" + string(rune('0'+i)) + ".xhtml"
		if _, err := r.Open(name); err != nil {
			t.Errorf("the manifest lists %s but the archive does not contain it", name)
		}
	}
}

// A second implementation is worth more than a second assertion from the same
// one: archive/zip wrote these bytes, so archive/zip agreeing with itself is
// weak evidence.
func TestEPUBAndCBZOpenWithAnIndependentUnzip(t *testing.T) {
	unzip, err := exec.LookPath("unzip")
	if err != nil {
		t.Skip("no unzip on this machine; archive/zip is the only check here")
	}

	epub, err := EPUB("The Long Survey", "Ada Prentice", 3)
	if err != nil {
		t.Fatal(err)
	}
	cbz, err := CBZ("The Long Survey", 4)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	for name, body := range map[string][]byte{"book.epub": epub, "comic.cbz": cbz} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(unzip, "-t", path).CombinedOutput() // #nosec G204 -- resolved binary, test-controlled path
		if err != nil {
			t.Errorf("unzip -t %s failed: %v\n%s", name, err, out)
		}
		if !bytes.Contains(out, []byte("No errors detected")) {
			t.Errorf("unzip -t %s did not report a clean archive:\n%s", name, out)
		}
	}
}

func TestCBZIsPagesInOrder(t *testing.T) {
	data, err := CBZ("The Long Survey", 5)
	if err != nil {
		t.Fatal(err)
	}
	r := openZip(t, data)

	if len(r.File) != 5 {
		t.Fatalf("pages = %d, want 5", len(r.File))
	}
	var last string
	for i, f := range r.File {
		if f.Name <= last {
			t.Errorf("page %d (%q) does not sort after %q — a reader shows pages in name order",
				i, f.Name, last)
		}
		last = f.Name

		body := readEntry(t, r, f.Name)
		if !bytes.Equal(body[0:2], []byte{0xFF, 0xD8}) {
			t.Errorf("page %q is not a JPEG: % x", f.Name, body[0:2])
		}
	}
}

func TestArchivesAreReproducible(t *testing.T) {
	// A generated fixture whose bytes move between runs would make the expected
	// blob hashes useless, and the demo asserts on those hashes (ADR-0017).
	for range 3 {
		a, err := EPUB("The Long Survey", "Ada Prentice", 3)
		if err != nil {
			t.Fatal(err)
		}
		b, err := EPUB("The Long Survey", "Ada Prentice", 3)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatal("two EPUBs built from the same inputs differ")
		}

		c, err := CBZ("The Long Survey", 4)
		if err != nil {
			t.Fatal(err)
		}
		d, err := CBZ("The Long Survey", 4)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(c, d) {
			t.Fatal("two CBZs built from the same inputs differ")
		}
	}
}

func TestSRTIsWellFormed(t *testing.T) {
	body := string(SRT(3))
	blocks := strings.Split(strings.TrimRight(body, "\n"), "\n\n")
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3\n%s", len(blocks), body)
	}
	for i, b := range blocks {
		lines := strings.Split(b, "\n")
		if len(lines) < 3 {
			t.Fatalf("block %d has %d lines, want at least 3", i, len(lines))
		}
		if !strings.Contains(lines[1], "-->") {
			t.Errorf("block %d has no timing line: %q", i, lines[1])
		}
	}
}
