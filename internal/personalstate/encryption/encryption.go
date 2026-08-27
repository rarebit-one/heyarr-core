// Package encryption is a thin shim over github.com/rarebit-one/voidbind-go/encryption.
//
// The X25519 space-key wrapping (ephemeral-static ECDH seal) and XChaCha20
// content encryption were extracted into voidbind-go byte-for-byte, including
// the heyarr/space-key-wrap/v1 label, so keys wrapped before the extraction
// unwrap unchanged. This shim re-exports the library; the adversarial synthetic
// test moved to voidbind-go, to live with the implementation it exercises.
package encryption

import vb "github.com/rarebit-one/voidbind-go/encryption"

// SpaceKey is re-exported from voidbind-go/encryption.
type SpaceKey = vb.SpaceKey

// Re-exported encryption constants.
const (
	Algorithm    = vb.Algorithm
	SeedSize     = vb.SeedSize
	SpaceKeySize = vb.SpaceKeySize
)

// Re-exported encryption sentinels and functions.
var (
	ErrDecrypt            = vb.ErrDecrypt
	ErrMalformedPublicKey = vb.ErrMalformedPublicKey
	ErrUnwrap             = vb.ErrUnwrap
	ErrWrongLength        = vb.ErrWrongLength

	DecryptChange   = vb.DecryptChange
	EncryptChange   = vb.EncryptChange
	FormatPublicKey = vb.FormatPublicKey
	GenerateKey     = vb.GenerateKey
	NewPrivateKey   = vb.NewPrivateKey
	ParsePublicKey  = vb.ParsePublicKey
	Seal            = vb.Seal
	NewSpaceKey     = vb.NewSpaceKey
	Unwrap          = vb.Unwrap
)
