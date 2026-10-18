package crdt

import (
	"fmt"

	"github.com/rarebit-one/void-which-binds-go/hashing"
)

// DriveMeta is the optional, client-made metadata a drive PUT may carry (ADR-0113):
// what a navigator needs to show a folder — type, capture time, dimensions, a
// thumbnail — without opening every manifest. The names are [PhotoManifest]'s, so
// an album item and a drive entry speak one vocabulary. Every field is optional and
// omitted when zero, so a drive without metadata encodes and snapshots exactly as
// before. It belongs to the WRITE that carries it: a later write's metadata does
// not merge with an earlier one's, and adding metadata to an existing file is a PUT
// of the same manifest id built on the current head (nothing is re-uploaded).
//
// Like everything in a change it is plaintext only on a device; the peer stores
// ciphertext (Invariant 6).
type DriveMeta struct {
	// Mime is the content type, sniffed or declared by the writer.
	Mime string `json:"mime,omitempty"`
	// PlaintextHash is the BLAKE3 of the plaintext bytes: the in-space dedupe key.
	PlaintextHash string `json:"plaintext_hash,omitempty"`
	// TakenAt is the capture time (unix seconds), a display and sort hint.
	TakenAt int64 `json:"taken_at,omitempty"`
	// Width and Height are the pixel dimensions of an image or video.
	Width  uint32 `json:"width,omitempty"`
	Height uint32 `json:"height,omitempty"`
	// DurationMS is a media duration in milliseconds.
	DurationMS int64 `json:"duration_ms,omitempty"`
	// Thumbnail is the blob id of a device-made sealed thumbnail, small enough to
	// read in one whole-blob fetch (the server never thumbnails vault bytes).
	Thumbnail string `json:"thumbnail,omitempty"`
	// Sidecar is the blob id of a sealed EXIF sidecar.
	Sidecar string `json:"sidecar,omitempty"`
}

// IsZero reports whether no metadata is set.
func (m DriveMeta) IsZero() bool { return m == DriveMeta{} }

// blobRefs are the blob ids the metadata references, for retention.
func (m DriveMeta) blobRefs() []string {
	var out []string
	for _, b := range []string{m.Thumbnail, m.Sidecar} {
		if b != "" {
			out = append(out, b)
		}
	}
	return out
}

// Validate rejects metadata whose ids are not canonical "blake3:<64 hex>" ids, as
// [PhotoManifest.Validate] does, and negative numbers. A pure function of the
// bytes, so every replica accepts or rejects the same change.
func (m DriveMeta) Validate() error {
	for _, f := range [...]struct{ name, id string }{
		{"plaintext_hash", m.PlaintextHash}, {"thumbnail", m.Thumbnail}, {"sidecar", m.Sidecar},
	} {
		if f.id == "" {
			continue
		}
		if _, err := hashing.Parse(f.id); err != nil {
			return fmt.Errorf("%w: %s %q is not a canonical blake3 id: %w", ErrInvalidChange, f.name, f.id, err)
		}
	}
	if m.TakenAt < 0 || m.DurationMS < 0 {
		return fmt.Errorf("%w: negative taken_at or duration_ms", ErrInvalidChange)
	}
	return nil
}
