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

// OpenManifestWithKeys opens a sealed manifest under the first of keys that
// opens it, trying them in order (newest first, as client.Manager.Keys returns
// them), and returns the manifest and the key that opened it — the key the
// file's frames are sealed under. When no key opens it, the error is the first
// key's (the library's one opaque decrypt refusal). A key that decrypts the
// manifest but whose plaintext does not decode is that error at once: the key
// was right and the manifest is bad, so no other key is tried.
func OpenManifestWithKeys(keys []encryption.SpaceKey, sealed []byte) (vaultframe.Manifest, encryption.SpaceKey, error) {
	if len(keys) == 0 {
		return vaultframe.Manifest{}, encryption.SpaceKey{}, errors.New("vaultread: no space key to open the manifest with")
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

// manifest fetches the MANIFEST blob and opens it under the first of keys that
// opens it, returning that key too.
func manifest(ctx context.Context, f BlobFetcher, keys []encryption.SpaceKey, manifestBlobID string) (vaultframe.Manifest, encryption.SpaceKey, error) {
	sealed, err := f.Fetch(ctx, manifestBlobID)
	if err != nil {
		return vaultframe.Manifest{}, encryption.SpaceKey{}, fmt.Errorf("vaultread: fetching manifest %s: %w", manifestBlobID, err)
	}
	m, sk, err := OpenManifestWithKeys(keys, sealed)
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

// readRange does the range read against an already-open manifest, so ReadAll
// does not fetch the manifest blob twice.
func readRange(ctx context.Context, f BlobFetcher, sk encryption.SpaceKey, m vaultframe.Manifest, off, n int64) ([]byte, error) {
	out, err := m.OpenRange(sk, off, n, func(start, end int64) ([]byte, error) {
		fc, ferr := f.FetchRange(ctx, m.Content, start, end)
		if ferr != nil {
			return nil, fmt.Errorf("vaultread: fetching content %s [%d,%d): %w", m.Content, start, end, ferr)
		}
		return fc, nil
	})
	if err != nil {
		return nil, fmt.Errorf("vaultread: reading range [%d,%d) of %s: %w", off, off+n, m.Content, err)
	}
	return out, nil
}

// ReadAll returns the vault file's whole plaintext: the manifest's full
// [0, PlaintextSize) — every content frame is a "covering" frame of the whole
// file, so this fetches each frame exactly once.
func ReadAll(ctx context.Context, f BlobFetcher, sk encryption.SpaceKey, manifestBlobID string) ([]byte, error) {
	return ReadAllWithKeys(ctx, f, []encryption.SpaceKey{sk}, manifestBlobID)
}

// ReadAllWithKeys is [ReadAll] over a space's keyring, newest first.
func ReadAllWithKeys(ctx context.Context, f BlobFetcher, keys []encryption.SpaceKey, manifestBlobID string) ([]byte, error) {
	m, sk, err := manifest(ctx, f, keys, manifestBlobID)
	if err != nil {
		return nil, err
	}
	return readRange(ctx, f, sk, m, 0, m.PlaintextSize)
}
