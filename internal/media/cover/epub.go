package cover

import (
	"archive/zip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
)

// ErrNoCover means the file declares no usable cover. It is a permanent answer
// about these bytes, not a failure a retry could fix.
var ErrNoCover = errors.New("cover: the file declares no cover image")

// maxXMLBytes bounds container.xml and the OPF. An OPF for a large book is tens
// of kilobytes; anything near this is not one.
const maxXMLBytes = 4 << 20

// maxImageBytes bounds a cover image. A print-resolution cover is a few
// megabytes; this refuses a "cover" that is really an archive bomb.
const maxImageBytes = 32 << 20

// Image is a cover read out of a book file.
type Image struct {
	Data []byte
	MIME string
}

type epubContainer struct {
	Rootfiles []struct {
		FullPath string `xml:"full-path,attr"`
	} `xml:"rootfiles>rootfile"`
}

type opfItem struct {
	ID         string `xml:"id,attr"`
	Href       string `xml:"href,attr"`
	MediaType  string `xml:"media-type,attr"`
	Properties string `xml:"properties,attr"`
}

type opfPackage struct {
	Metas []struct {
		Name    string `xml:"name,attr"`
		Content string `xml:"content,attr"`
	} `xml:"metadata>meta"`
	Items      []opfItem `xml:"manifest>item"`
	References []struct {
		Type string `xml:"type,attr"`
		Href string `xml:"href,attr"`
	} `xml:"guide>reference"`
}

// FromEPUB reads the cover image an EPUB declares about itself. It returns
// ErrNoCover (wrapped) when the container is readable but names no image cover,
// and another error when the container itself cannot be read — both permanent
// for these bytes.
//
// The declaration is looked for in the order publishers actually use:
//
//  1. EPUB 3: the manifest item with properties="cover-image".
//  2. EPUB 2: <meta name="cover" content="ITEM-ID">.
//  3. The guide's type="cover" reference, when it points straight at an image.
//  4. A manifest image whose id or file name is "cover…" — common in files
//     written by tools that skipped both declarations.
func FromEPUB(r io.ReaderAt, size int64) (Image, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return Image{}, fmt.Errorf("cover: reading the EPUB container: %w", err)
	}

	var c epubContainer
	if err := readXML(zr, "META-INF/container.xml", &c); err != nil {
		return Image{}, err
	}
	if len(c.Rootfiles) == 0 || c.Rootfiles[0].FullPath == "" {
		return Image{}, errors.New("cover: the EPUB names no package document")
	}
	opfPath := c.Rootfiles[0].FullPath

	var pkg opfPackage
	if err := readXML(zr, opfPath, &pkg); err != nil {
		return Image{}, err
	}

	item, ok := pickCoverItem(pkg)
	if !ok {
		return Image{}, ErrNoCover
	}
	name, err := resolveHref(opfPath, item.Href)
	if err != nil {
		return Image{}, fmt.Errorf("%w: %w", ErrNoCover, err)
	}
	data, err := readMember(zr, name, maxImageBytes)
	if err != nil {
		return Image{}, fmt.Errorf("%w: %w", ErrNoCover, err)
	}
	return Image{Data: data, MIME: strings.ToLower(item.MediaType)}, nil
}

func pickCoverItem(pkg opfPackage) (opfItem, bool) {
	byID := make(map[string]opfItem, len(pkg.Items))
	for _, it := range pkg.Items {
		byID[it.ID] = it
	}

	for _, it := range pkg.Items {
		if isImage(it) && hasProperty(it.Properties, "cover-image") {
			return it, true
		}
	}
	for _, m := range pkg.Metas {
		if strings.EqualFold(m.Name, "cover") {
			if it, ok := byID[m.Content]; ok && isImage(it) {
				return it, true
			}
		}
	}
	for _, ref := range pkg.References {
		if !strings.EqualFold(ref.Type, "cover") {
			continue
		}
		href, _, _ := strings.Cut(ref.Href, "#")
		for _, it := range pkg.Items {
			if it.Href == href && isImage(it) {
				return it, true
			}
		}
	}
	for _, it := range pkg.Items {
		base := strings.ToLower(path.Base(it.Href))
		if isImage(it) && (strings.Contains(strings.ToLower(it.ID), "cover") || strings.HasPrefix(base, "cover")) {
			return it, true
		}
	}
	return opfItem{}, false
}

// isImage accepts the raster types a client can show as a cover. SVG is left
// out: it can reference other members and scripts, and no cover consumer
// renders it.
func isImage(it opfItem) bool {
	switch strings.ToLower(it.MediaType) {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	}
	return false
}

func hasProperty(props, want string) bool {
	for _, p := range strings.Fields(props) {
		if p == want {
			return true
		}
	}
	return false
}

// resolveHref turns a manifest href, which is a URL relative to the OPF, into a
// ZIP member name. A reference that climbs out of the container is refused.
func resolveHref(opfPath, href string) (string, error) {
	u, err := url.Parse(href)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "", fmt.Errorf("the cover href %q is not a container path", href)
	}
	name := path.Clean(path.Join(path.Dir(opfPath), u.Path))
	if name == "." || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("the cover href %q leaves the container", href)
	}
	return name, nil
}

func readXML(zr *zip.Reader, name string, into any) error {
	data, err := readMember(zr, name, maxXMLBytes)
	if err != nil {
		return err
	}
	if err := xml.Unmarshal(data, into); err != nil {
		return fmt.Errorf("cover: parsing %s: %w", name, err)
	}
	return nil
}

// readMember reads one ZIP member, matching its name case-insensitively when an
// exact match is missing (EPUB writers disagree about case far more often than
// they ship two members differing only in it).
func readMember(zr *zip.Reader, name string, limit int64) ([]byte, error) {
	var f *zip.File
	for _, zf := range zr.File {
		if zf.Name == name {
			f = zf
			break
		}
	}
	if f == nil {
		for _, zf := range zr.File {
			if strings.EqualFold(zf.Name, name) {
				f = zf
				break
			}
		}
	}
	if f == nil {
		return nil, fmt.Errorf("cover: the container has no %s", name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("cover: opening %s: %w", name, err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("cover: reading %s: %w", name, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("cover: %s is larger than %d bytes", name, limit)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("cover: %s is empty", name)
	}
	return data, nil
}
