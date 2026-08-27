// Package identity is a thin shim over github.com/rarebit-one/voidbind-go/identity.
//
// The Ed25519 identity core — public-key format/parse, the Identity value and
// its Signer, the key-file storage, and Ensure with its Peers/Marker interfaces
// — was extracted into voidbind-go byte-for-byte. This package re-exports it.
//
// The three glue spots the migration keeps in heyarr live ELSEWHERE and are
// unchanged by this shim: heyarr's DB implements the Peers and Marker interfaces
// (re-exported below as aliases, so those implementations still satisfy them)
// and passes them to Ensure; deviceauth and pairrelay keep their DB/HTTP halves.
// Only the identity primitives are deduplicated here. Tests live in voidbind-go.
package identity

import vb "github.com/rarebit-one/voidbind-go/identity"

// Identity is re-exported from voidbind-go/identity.
type Identity = vb.Identity

// Options is re-exported from voidbind-go/identity.
type Options = vb.Options

// Peers is re-exported from voidbind-go/identity. It is the interface heyarr's
// peer database satisfies; as an alias it is the SAME interface voidbind-go's
// Ensure expects, so heyarr's implementation passes to Ensure unchanged.
type Peers = vb.Peers

// Marker is re-exported from voidbind-go/identity. Like Peers, it is the SAME
// interface Ensure expects, satisfied by heyarr's CAS root marker unchanged.
type Marker = vb.Marker

// Re-exported identity constants.
const (
	Algorithm   = vb.Algorithm
	KeyFileMode = vb.KeyFileMode
	KeyFileName = vb.KeyFileName
)

// Re-exported identity sentinels and functions.
var (
	ErrIdentityConflict   = vb.ErrIdentityConflict
	ErrKeyExists          = vb.ErrKeyExists
	ErrMalformedPublicKey = vb.ErrMalformedPublicKey
	ErrKeyMismatch        = vb.ErrKeyMismatch
	ErrKeyMissing         = vb.ErrKeyMissing
	ErrKeyPermissions     = vb.ErrKeyPermissions

	FormatPublicKey = vb.FormatPublicKey
	ParsePublicKey  = vb.ParsePublicKey
	Ensure          = vb.Ensure
	Signer          = vb.Signer
	Install         = vb.Install
	InstallFromFile = vb.InstallFromFile
	KeyPath         = vb.KeyPath
	ReadSeed        = vb.ReadSeed
)
