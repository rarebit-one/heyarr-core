// Package pairing is a thin shim over github.com/rarebit-one/voidbind-go/pairing.
//
// The SAS + commit-reveal implementation was extracted into voidbind-go, and
// (after voidbind-go#3) is byte-identical to heyarr's v2 dual-key commitment
// (ADR-0049 §41): Commit(pub, enc) / Open(pub, enc) bind both the signing and
// the encryption key, under domain heyarr/pairing/commit/v2. This shim
// re-exports it. The adversarial synthetic test moved to voidbind-go, and the
// unit tests live with the implementation there.
package pairing

import vb "github.com/rarebit-one/voidbind-go/pairing"

// Commitment is re-exported from voidbind-go/pairing.
type Commitment = vb.Commitment

// Keys is re-exported from voidbind-go/pairing.
type Keys = vb.Keys

// SAS is re-exported from voidbind-go/pairing.
type SAS = vb.SAS

// Re-exported pairing constants.
const (
	SaltLen       = vb.SaltLen
	MinSaltLen    = vb.MinSaltLen
	CommitmentLen = vb.CommitmentLen
	Digits        = vb.Digits
	EncKeySize    = vb.EncKeySize
	Version       = vb.Version
)

// Re-exported pairing sentinels and functions.
var (
	ErrMalformedKey       = vb.ErrMalformedKey
	ErrShortSalt          = vb.ErrShortSalt
	ErrCommitmentMismatch = vb.ErrCommitmentMismatch

	NewSalt = vb.NewSalt
	Commit  = vb.Commit
	Derive  = vb.Derive
)
