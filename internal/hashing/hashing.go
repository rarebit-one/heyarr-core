// Package hashing is a thin shim over github.com/rarebit-one/voidbind-go/hashing.
//
// The implementation was extracted into voidbind-go (byte-for-byte); this
// package now re-exports it so heyarr-core has one copy of the content-hashing
// primitives, not two. Every identifier here is a type alias or a forward to the
// library, so callers that import this path — and the blake3 Hash values they
// pass around — are unchanged. The tests live with the implementation, in
// voidbind-go.
package hashing

import vb "github.com/rarebit-one/voidbind-go/hashing"

// Hash is re-exported from voidbind-go/hashing.
type Hash = vb.Hash

// Hasher is re-exported from voidbind-go/hashing.
type Hasher = vb.Hasher

// VerifyingReader is re-exported from voidbind-go/hashing.
type VerifyingReader = vb.VerifyingReader

// ErrMismatch is re-exported from voidbind-go/hashing.
type ErrMismatch = vb.ErrMismatch

// Re-exported hashing constants.
const (
	DigestLen = vb.DigestLen
	HexLen    = vb.HexLen
	Algorithm = vb.Algorithm
)

// Re-exported hashing sentinels and functions.
var (
	ErrInvalidHash = vb.ErrInvalidHash

	Verify             = vb.Verify
	HashFile           = vb.HashFile
	HashReader         = vb.HashReader
	MustParse          = vb.MustParse
	Parse              = vb.Parse
	New                = vb.New
	NewVerifyingReader = vb.NewVerifyingReader
)
