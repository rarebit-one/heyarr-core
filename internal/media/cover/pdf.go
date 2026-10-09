package cover

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// renderTimeout bounds one page-1 render. A first page renders in well under a
// second; a PDF that takes minutes is malformed or hostile, and holding a job
// slot for it helps nobody.
const renderTimeout = 2 * time.Minute

// coverLongEdge is the size the rendered page is scaled to fit, in pixels. It
// matches what a cover endpoint serves for a fetched cover (Open Library's -L),
// so a rendered page and a fetched cover look alike in a grid.
const coverLongEdge = 1200

// PDFRenderer rasterises a PDF's first page with pdftoppm.
type PDFRenderer struct {
	// Path is the resolved pdftoppm binary (media.Toolchain.Pdftoppm.Path).
	Path string
}

// ErrRender means pdftoppm could not render the file — a damaged or encrypted
// PDF. Permanent for these bytes.
var ErrRender = errors.New("cover: the PDF's first page could not be rendered")

// RenderFirstPage renders page 1 of the PDF at src as a JPEG and returns its
// bytes. ErrRender (wrapped) is a property of the file; any other error is the
// environment's.
func (p PDFRenderer) RenderFirstPage(ctx context.Context, src string) (Image, error) {
	if p.Path == "" {
		return Image{}, errors.New("cover: no pdftoppm configured")
	}
	dir, err := os.MkdirTemp("", "heyarr-cover-*")
	if err != nil {
		return Image{}, fmt.Errorf("cover: making a scratch directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	ctx, cancel := context.WithTimeout(ctx, renderTimeout)
	defer cancel()

	prefix := filepath.Join(dir, "page")
	var stderr bytes.Buffer
	// #nosec G204 -- the binary is the operator's resolved pdftoppm and src is a
	// CAS path this node owns; no argument comes from the file's contents.
	cmd := exec.CommandContext(ctx, p.Path,
		"-f", "1", "-l", "1", "-singlefile",
		"-jpeg", "-jpegopt", "quality=85",
		"-scale-to", fmt.Sprint(coverLongEdge),
		src, prefix)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Image{}, fmt.Errorf("%w: timed out after %s", ErrRender, renderTimeout)
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return Image{}, fmt.Errorf("%w: %s", ErrRender, firstLine(stderr.String()))
		}
		return Image{}, fmt.Errorf("cover: running pdftoppm: %w", err)
	}

	data, err := os.ReadFile(prefix + ".jpg") //nolint:gosec // a path inside our own scratch directory
	if err != nil {
		return Image{}, fmt.Errorf("%w: no output image: %w", ErrRender, err)
	}
	if len(data) == 0 {
		return Image{}, fmt.Errorf("%w: empty output image", ErrRender)
	}
	return Image{Data: data, MIME: "image/jpeg"}, nil
}

func firstLine(s string) string {
	line, _, _ := bytes.Cut([]byte(s), []byte("\n"))
	return string(line)
}
