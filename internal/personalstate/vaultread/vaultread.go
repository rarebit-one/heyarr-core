// Package vaultread is the client-side read path for a vault file (ADR-0097,
// W1c): given the space key and the MANIFEST blob's id, fetch and decrypt the
// manifest, then read a plaintext byte range by fetching only the content-blob
// frames that cover it, decrypting each with vaultframe, and trimming to the
// requested window.
//
// This package knows nothing about HTTP. It asks a BlobFetcher for bytes by
// blob id and byte range; turning that into a ranged GET (the Range header,
// the 206/200 distinction, resuming) is the adapter's concern, not this
// package's — see the BlobFetcher doc comment.
package vaultread

import (
	"context"
	"fmt"

	"github.com/rarebit-one/voidbind-go/encryption"

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

// manifest fetches the MANIFEST blob and opens it under sk.
func manifest(ctx context.Context, f BlobFetcher, sk encryption.SpaceKey, manifestBlobID string) (vaultframe.Manifest, error) {
	sealed, err := f.Fetch(ctx, manifestBlobID)
	if err != nil {
		return vaultframe.Manifest{}, fmt.Errorf("vaultread: fetching manifest %s: %w", manifestBlobID, err)
	}
	m, err := vaultframe.OpenManifest(sk, sealed)
	if err != nil {
		return vaultframe.Manifest{}, fmt.Errorf("vaultread: opening manifest %s: %w", manifestBlobID, err)
	}
	return m, nil
}

// ReadRange returns the vault file's plaintext[off : off+n], fetching only the
// content-blob frames that cover the range. It fetches and opens the manifest
// (one Fetch), then range-fetches and decrypts only the covering frames of the
// content blob (vaultframe.Manifest.OpenRange), trimming to exactly [off, off+n).
func ReadRange(ctx context.Context, f BlobFetcher, sk encryption.SpaceKey, manifestBlobID string, off, n int64) ([]byte, error) {
	m, err := manifest(ctx, f, sk, manifestBlobID)
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
	m, err := manifest(ctx, f, sk, manifestBlobID)
	if err != nil {
		return nil, err
	}
	return readRange(ctx, f, sk, m, 0, m.PlaintextSize)
}
