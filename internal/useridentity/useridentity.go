// Package useridentity is a thin re-export of the self-sovereign user-identity
// store, which now lives in voidbind-go. Heyarr's identity core is Voidbind (the
// extracted, self-sovereign authenticator); this package keeps Heyarr's import
// path and public surface stable while the single implementation lives in one
// place. See rarebit-one/heyarr-core#366.
//
// voidbind-go v0.3.0 is a superset of what this package used to hold — including
// the §41 recovery encryption key on Identity (rarebit-one/voidbind-go#12), the
// last thing that had diverged. Two on-disk behaviours changed with the rename to
// Voidbind, both backward-compatible on read: the key-file prefix is now
// "voidbind-user-…" (the legacy "heyarr-user-…" is still accepted), and the
// directory override env var is VOIDBIND_IDENTITY_DIR (was HEYARR_IDENTITY_DIR).
// Existing key files keep working; a HEYARR_IDENTITY_DIR override does not.
package useridentity

import vb "github.com/rarebit-one/voidbind-go/useridentity"

// Types (aliases carry every field and method, including Identity.EncryptionKey).
type (
	Clock        = vb.Clock
	Identity     = vb.Identity
	Store        = vb.Store
	StoreOptions = vb.StoreOptions
	View         = vb.View
)

// Constructors.
var (
	DefaultDir = vb.DefaultDir
	NewStore   = vb.NewStore
	NewView    = vb.NewView
)

// Constants.
const (
	KeyFileName    = vb.KeyFileName
	RecordFileName = vb.RecordFileName
	Algorithm      = vb.Algorithm
	DirMode        = vb.DirMode
	EnvDir         = vb.EnvDir
	KeyFileMode    = vb.KeyFileMode
)

// Sentinel errors.
var (
	ErrKeyPermissions = vb.ErrKeyPermissions
	ErrMalformedKey   = vb.ErrMalformedKey
	ErrNoIdentity     = vb.ErrNoIdentity
	ErrIdentityExists = vb.ErrIdentityExists
)
