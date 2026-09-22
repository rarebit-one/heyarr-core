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

// AgreementFunc is re-exported from voidbind-go/encryption (ADR-0098): the
// X25519 ECDH step of an unwrap, factored out so the recipient private key can
// live where this process cannot export it — a YubiKey doing the agreement
// on-card, a TPM-gated key, or an offloaded phone. A pluggable Unwrapper backend
// supplies one; UnwrapWithAgreement drives it.
type AgreementFunc = vb.AgreementFunc

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

	// UnwrapWithAgreement is Unwrap with the ECDH injected (ADR-0098): the seam a
	// hardware-gated or offloaded Unwrapper reaches through, so the recipient
	// private key need not be an in-process *ecdh.PrivateKey. Unwrap delegates to
	// it and is byte-identical.
	UnwrapWithAgreement = vb.UnwrapWithAgreement
)
