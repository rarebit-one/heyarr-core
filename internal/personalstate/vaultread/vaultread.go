// Package vaultread is the client-side read path for a vault file (ADR-0097,
// W1c): given the space key and the MANIFEST blob's id, fetch and decrypt the
// manifest, then read a plaintext byte range by fetching only the content-blob
// frames that cover it, decrypting each with vaultframe, and trimming to the
// requested window.
//
// A space's key can rotate (ADR-0103), and nothing is re-encrypted when it does:
// a file pushed before a rotation stays sealed under the key of its epoch. A
// reader therefore holds the space's whole keyring, newest first, and the
// *WithKeys functions try each key against the manifest (which records nothing
// about the key); the key that opens the manifest is the one its frames are
// sealed under, because a push seals both under one key.
//
// Every blob is checked against its content address before it is trusted
// (Invariant 1): the manifest blob's BLAKE3 must equal the drive entry's id
// before it is opened, a whole-file read checks the content blob against the
// manifest's content id, and a range read rests on the per-frame AEAD binding
// (see readRange). A mismatch is [ErrBlobIntegrity], never "absent".
//
// This package knows nothing about HTTP. It asks a BlobFetcher for bytes by
// blob id and byte range; turning that into a ranged GET (the Range header,
// the 206/200 distinction, resuming) is the adapter's concern, not this
// package's — see the BlobFetcher doc comment.
package vaultread

import (
	"context"
	"errors"
	"fmt"

	"github.com/rarebit-one/void-which-binds-go/encryption"
	"github.com/rarebit-one/void-which-binds-go/hashing"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/vaultframe"
)

// BlobFetcher fetches ciphertext blob bytes by blob id ("blake3:<hex>").
//
// FetchRange returns the half-open byte range [start, end) of the blob — the
// same convention as vaultframe.Manifest.FrameByteRange, so a caller can pass
// its output straight through. Translating that into an HTTP byte-range
// request (an inclusive `Range: bytes=start-end-1` header, handling a 206 vs
// a 200 that ignored the range, resuming) is the adapter's job; this package
// only ever asks for [start, end) and trims nothing itself — vaultframe does
// the trimming after decrypt.
type BlobFetcher interface {
	// Fetch returns the whole blob's bytes.
	Fetch(ctx context.Context, blobID string) ([]byte, error)
	// FetchRange returns the blob's bytes in [start, end).
	FetchRange(ctx context.Context, blobID string, start, end int64) ([]byte, error)
}

// ErrBlobIntegrity is a blob whose bytes are not the object its id names: the
// manifest blob does not hash to the drive entry's id, the content blob does
// not hash to the manifest's content id, or a frame of the content blob is not
// the frame of this file the manifest describes. It is deliberately distinct
// from "absent" and from a wrong key: the node served SOMETHING, and it was not
// what was asked for (storage corruption, replication confusion, or a peer
// substituting another object). Invariant 1: a reader verifies bytes itself and
// never trusts a claimed hash — the AEAD proves only that bytes were sealed
// under the space key, not that they are the object the ref names.
var ErrBlobIntegrity = errors.New("vaultread: blob does not match its content address")

// verifyBlob checks that b hashes to the blob id ("blake3:<hex>") it was
// fetched as, returning ErrBlobIntegrity when it does not.
func verifyBlob(blobID string, b []byte) error {
	h := hashing.New()
	_, _ = h.Write(b)
	return checkDigest(blobID, h.Sum())
}

// checkDigest compares a computed digest with the blob id it must equal.
func checkDigest(blobID string, got hashing.Hash) error {
	want, err := hashing.Parse(blobID)
	if err != nil {
		return fmt.Errorf("vaultread: %q is not a blob id: %w", blobID, err)
	}
	if !got.Equal(want) {
		return fmt.Errorf("%w: %s was served as bytes hashing to %s", ErrBlobIntegrity, want, got)
	}
	return nil
}

// OpenManifestWithKeys verifies that sealed hashes to manifestBlobID — the id
// the drive entry names — and then opens it under the first of keys that opens
// it, trying them in order (newest first, as client.Manager.Keys returns them).
// It returns the manifest and the key that opened it — the key the file's
// frames are sealed under.
//
// The hash check comes first and is not optional: another manifest sealed
// under the same space key decrypts just as well, so without it a node could
// steer the reader to a different file of the same space. A mismatch is
// ErrBlobIntegrity and no key is tried.
//
// When no key opens it, the error is the first key's (the library's one opaque
// decrypt refusal). A key that decrypts the manifest but whose plaintext does
// not decode is that error at once: the key was right and the manifest is bad,
// so no other key is tried.
func OpenManifestWithKeys(keys []encryption.SpaceKey, manifestBlobID string, sealed []byte) (vaultframe.Manifest, encryption.SpaceKey, error) {
	if len(keys) == 0 {
		return vaultframe.Manifest{}, encryption.SpaceKey{}, errors.New("vaultread: no space key to open the manifest with")
	}
	if err := verifyBlob(manifestBlobID, sealed); err != nil {
		return vaultframe.Manifest{}, encryption.SpaceKey{}, err
	}
	var first error
	for _, sk := range keys {
		m, err := vaultframe.OpenManifest(sk, sealed)
		if err == nil {
			return m, sk, nil
		}
		if !errors.Is(err, encryption.ErrDecrypt) {
			return vaultframe.Manifest{}, encryption.SpaceKey{}, err
		}
		if first == nil {
			first = err
		}
	}
	return vaultframe.Manifest{}, encryption.SpaceKey{}, first
}

// manifest fetches the MANIFEST blob, verifies it against its id and opens it
// under the first of keys that opens it, returning that key too.
func manifest(ctx context.Context, f BlobFetcher, keys []encryption.SpaceKey, manifestBlobID string) (vaultframe.Manifest, encryption.SpaceKey, error) {
	sealed, err := f.Fetch(ctx, manifestBlobID)
	if err != nil {
		return vaultframe.Manifest{}, encryption.SpaceKey{}, fmt.Errorf("vaultread: fetching manifest %s: %w", manifestBlobID, err)
	}
	m, sk, err := OpenManifestWithKeys(keys, manifestBlobID, sealed)
	if err != nil {
		return vaultframe.Manifest{}, encryption.SpaceKey{}, fmt.Errorf("vaultread: opening manifest %s: %w", manifestBlobID, err)
	}
	return m, sk, nil
}

// ReadRange returns the vault file's plaintext[off : off+n], fetching only the
// content-blob frames that cover the range. It fetches and opens the manifest
// (one Fetch), then range-fetches and decrypts only the covering frames of the
// content blob (vaultframe.Manifest.OpenRange), trimming to exactly [off, off+n).
func ReadRange(ctx context.Context, f BlobFetcher, sk encryption.SpaceKey, manifestBlobID string, off, n int64) ([]byte, error) {
	return ReadRangeWithKeys(ctx, f, []encryption.SpaceKey{sk}, manifestBlobID, off, n)
}

// ReadRangeWithKeys is [ReadRange] over a space's keyring, newest first: the
// manifest picks the key ([OpenManifestWithKeys]) and the frames are read under
// it.
func ReadRangeWithKeys(ctx context.Context, f BlobFetcher, keys []encryption.SpaceKey, manifestBlobID string, off, n int64) ([]byte, error) {
	m, sk, err := manifest(ctx, f, keys, manifestBlobID)
	if err != nil {
		return nil, err
	}
	return readRange(ctx, f, sk, m, off, n)
}

// readRange does the range read against an already-open manifest.
//
// A range read cannot check the content blob's id — that would need the whole
// blob — so it rests on the per-frame AEAD instead (vaultframe): each frame
// seals version ‖ file_id ‖ frame_index with its data under the space key, and
// OpenFrame checks file_id and index against the manifest, whose own bytes were
// verified against the drive entry's id before it was opened. A frame from
// another space fails to decrypt; a frame from another file of the same space
// fails the file_id check; a frame moved within the file fails the index
// check; a truncated or altered frame fails the tag. Every one of those is
// reported as ErrBlobIntegrity: the key already opened this file's manifest, so
// a frame it cannot open is not this file's frame.
func readRange(ctx context.Context, f BlobFetcher, sk encryption.SpaceKey, m vaultframe.Manifest, off, n int64) ([]byte, error) {
	return openRange(sk, m, off, n, func(start, end int64) ([]byte, error) {
		return fetchContent(ctx, f, m, start, end)
	})
}

// fetchContent range-fetches [start, end) of the manifest's content blob.
func fetchContent(ctx context.Context, f BlobFetcher, m vaultframe.Manifest, start, end int64) ([]byte, error) {
	fc, err := f.FetchRange(ctx, m.Content, start, end)
	if err != nil {
		return nil, fmt.Errorf("vaultread: fetching content %s [%d,%d): %w", m.Content, start, end, err)
	}
	return fc, nil
}

// openRange decrypts [off, off+n) through fetch, classifying a frame that does
// not open, or opens as another frame, as ErrBlobIntegrity.
func openRange(sk encryption.SpaceKey, m vaultframe.Manifest, off, n int64, fetch func(start, end int64) ([]byte, error)) ([]byte, error) {
	out, err := m.OpenRange(sk, off, n, fetch)
	if err != nil {
		if ferr := frameError(m, err); ferr != nil {
			return nil, ferr
		}
		return nil, fmt.Errorf("vaultread: reading range [%d,%d) of %s: %w", off, off+n, m.Content, err)
	}
	return out, nil
}

// frameError is err as ErrBlobIntegrity when it is a frame that did not open
// under the key that opened its manifest, or opened as another frame; nil when
// it is any other failure.
func frameError(m vaultframe.Manifest, err error) error {
	if errors.Is(err, vaultframe.ErrFrame) || errors.Is(err, encryption.ErrDecrypt) {
		return fmt.Errorf("%w: a frame of %s is not this file's: %w", ErrBlobIntegrity, m.Content, err)
	}
	return nil
}

// initialReadBuffer is readAll's first allocation: the plaintext buffer grows
// from here as frames decrypt, never from the manifest's claim alone.
const initialReadBuffer = 8 << 20

// grow returns buf with room for n more bytes, at most limit in all. A larger
// buffer is allocated, the plaintext copied in, and the old one ZEROED, so
// growing never leaves a plaintext copy behind (which append's own growth
// would).
func grow(buf []byte, n int, limit int64) []byte {
	if len(buf)+n <= cap(buf) {
		return buf
	}
	size := min(max(int64(cap(buf))*2, int64(len(buf)+n)), limit)
	next := make([]byte, len(buf), size)
	copy(next, buf)
	clear(buf[:cap(buf)])
	return next
}

// readAll reads the whole file against an already-open manifest, streaming:
// it fetches the frames in order, feeds each one's ciphertext to a running
// BLAKE3 and decrypts it straight into the plaintext buffer, then drops it, so
// peak memory is about twice the plaintext read so far (while the buffer
// grows) plus one frame. The frames together are the
// whole content blob, so after the last one the digest must equal the
// manifest's content id. Until it does, the plaintext is not released: on any
// failure — a frame that does not open, or a final digest mismatch — the
// buffer is zeroed and no plaintext is returned.
func readAll(ctx context.Context, f BlobFetcher, sk encryption.SpaceKey, m vaultframe.Manifest) (_ []byte, err error) {
	if m.FrameCount < 0 || m.PlaintextSize < 0 {
		return nil, fmt.Errorf("vaultread: manifest records %d frames of %d bytes", m.FrameCount, m.PlaintextSize)
	}
	// The manifest's size is a claim until the frames bear it out, so it never
	// sizes an allocation outright: the buffer starts small and grows as frames
	// actually decrypt (grow), and a manifest claiming petabytes costs nothing
	// until that much ciphertext has been fetched.
	out := make([]byte, 0, min(m.PlaintextSize, initialReadBuffer))
	defer func() {
		if err != nil {
			clear(out[:cap(out)])
			out = nil
		}
	}()
	h := hashing.New()
	for i := 0; i < m.FrameCount; i++ {
		start, end := m.FrameByteRange(i)
		fc, err := fetchContent(ctx, f, m, start, end)
		if err != nil {
			return nil, err
		}
		_, _ = h.Write(fc)
		data, err := m.OpenFrame(sk, i, fc)
		if err != nil {
			if ferr := frameError(m, err); ferr != nil {
				return nil, ferr
			}
			return nil, fmt.Errorf("vaultread: opening frame %d of %s: %w", i, m.Content, err)
		}
		// Never outgrow the buffer: append would copy the plaintext so far into
		// a new one and leave this copy unzeroed.
		if int64(len(out))+int64(len(data)) > m.PlaintextSize {
			clear(data)
			return nil, fmt.Errorf("vaultread: content %s decrypts past the %d bytes its manifest records", m.Content, m.PlaintextSize)
		}
		out = grow(out, len(data), m.PlaintextSize)
		out = append(out, data...)
		clear(data)
	}
	if err := checkDigest(m.Content, h.Sum()); err != nil {
		return nil, fmt.Errorf("vaultread: content of the manifest: %w", err)
	}
	if int64(len(out)) != m.PlaintextSize {
		return nil, fmt.Errorf("vaultread: content %s decrypted to %d bytes, manifest records %d", m.Content, len(out), m.PlaintextSize)
	}
	return out, nil
}

// ReadAll returns the vault file's whole plaintext: the manifest's full
// [0, PlaintextSize). It fetches each frame exactly once, decrypting as it
// goes, and returns the plaintext only once the concatenated frames — the
// whole content blob — have hashed to the manifest's content id.
func ReadAll(ctx context.Context, f BlobFetcher, sk encryption.SpaceKey, manifestBlobID string) ([]byte, error) {
	return ReadAllWithKeys(ctx, f, []encryption.SpaceKey{sk}, manifestBlobID)
}

// ReadAllWithKeys is [ReadAll] over a space's keyring, newest first.
func ReadAllWithKeys(ctx context.Context, f BlobFetcher, keys []encryption.SpaceKey, manifestBlobID string) ([]byte, error) {
	m, sk, err := manifest(ctx, f, keys, manifestBlobID)
	if err != nil {
		return nil, err
	}
	return readAll(ctx, f, sk, m)
}
