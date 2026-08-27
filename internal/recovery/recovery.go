// Package recovery is a thin shim over github.com/rarebit-one/voidbind-go/recovery.
//
// The recovery-secret implementation was extracted into voidbind-go byte-for-byte,
// including the identity-DEFINING HKDF labels (heyarr/recovery/v1/...): those
// derive the user key, so they are kept verbatim — changing them would orphan
// every enrolled identity. This shim re-exports the library. Tests live there.
package recovery

import vb "github.com/rarebit-one/voidbind-go/recovery"

// Secret is re-exported from voidbind-go/recovery.
type Secret = vb.Secret

// Re-exported recovery constants (the HKDF labels are identity-defining — kept verbatim).
const (
	SecretEntropyBytes  = vb.SecretEntropyBytes
	UserEncryptionLabel = vb.UserEncryptionLabel
	UserIdentityLabel   = vb.UserIdentityLabel
)

// Re-exported recovery sentinels and functions.
var (
	ErrMalformedSecret = vb.ErrMalformedSecret
	ErrCorruptSecret   = vb.ErrCorruptSecret
	ErrBech32          = vb.ErrBech32

	DeriveUserEncryptionSeed = vb.DeriveUserEncryptionSeed
	DeriveUserSeed           = vb.DeriveUserSeed
	FormatUserID             = vb.FormatUserID
	GenerateSecret           = vb.GenerateSecret
	ParseSecret              = vb.ParseSecret
)
