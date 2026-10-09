// Package cover lifts a book's cover out of the book file itself (ADR-0105):
// the image an EPUB declares as its cover, or a PDF's first page rendered by
// poppler's pdftoppm.
//
// # Why this is not rendering in the §69 sense
//
// §69 says Heyarr stores and serves publications and clients render them, and
// internal/domain/publication holds that line. This package does not render a
// publication for reading. It produces one piece of METADATA — the Work's cover,
// the same role='artwork' asset a shipped cover.jpg or an Open Library fetch
// becomes — and it does so in the two ways §83 allows:
//
//   - An EPUB's cover is a file the container already holds and names in its own
//     OPF manifest. Copying those bytes out is reading an index and a member, the
//     same act publication.Examine performs; nothing is decoded.
//   - A PDF's cover has to be rasterised, and that is handed to an external
//     specialist (pdftoppm), exactly as video encoding is handed to FFmpeg. No
//     PDF or image library is linked into Heyarr. A node without pdftoppm
//     advertises no capability, and its PDF cover jobs wait, visibly (ADR-0023).
package cover

import "strings"

// JobType is the extract_cover job: lift one book file's embedded cover into a
// role='artwork' asset on its Edition.
const JobType = "extract_cover"

// MIME types this package can take a cover from.
const (
	MIMEEPUB = "application/epub+zip"
	MIMEPDF  = "application/pdf"
)

// Payload names the book file whose cover is wanted: the primary asset (whose
// Edition the cover attaches to) and its blob (the bytes to read).
type Payload struct {
	BlobHash string `json:"blob_hash"`
	AssetID  string `json:"asset_id"`
	MIME     string `json:"mime"`
}

// DedupeKey keys a live extraction on the blob: two Editions holding identical
// bytes have identical covers, and one live job per blob is enough.
func DedupeKey(blobHash string) string { return "extract-cover:" + blobHash }

// Supported reports whether a cover can be taken from a file of this MIME.
func Supported(mime string) bool {
	switch normalise(mime) {
	case MIMEEPUB, MIMEPDF:
		return true
	}
	return false
}

// RequiredCapability is the capability a worker must advertise to run the job
// for this MIME. An EPUB needs nothing but archive/zip; a PDF needs pdftoppm.
// The job — not the handler registration — carries the requirement, so a node
// without pdftoppm still extracts every EPUB cover while its PDF jobs wait.
func RequiredCapability(mime string) string {
	if normalise(mime) == MIMEPDF {
		return CapabilityPdftoppm
	}
	return ""
}

// CapabilityPdftoppm mirrors media.CapabilityPdftoppm. It is restated rather than
// imported so the controller, which enqueues, need not import the toolchain
// resolver; TestRequiredCapability keeps the two in step.
const CapabilityPdftoppm = "pdftoppm"

func normalise(mime string) string {
	m, _, _ := strings.Cut(mime, ";")
	return strings.ToLower(strings.TrimSpace(m))
}
