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

// Clock is re-exported from voidbind-go/useridentity.
type Clock = vb.Clock

// Identity is re-exported from voidbind-go/useridentity (it carries the §41
// recovery EncryptionKey, since aliases keep every field).
type Identity = vb.Identity

// Store is re-exported from voidbind-go/useridentity.
type Store = vb.Store

// StoreOptions is re-exported from voidbind-go/useridentity.
type StoreOptions = vb.StoreOptions

// View is re-exported from voidbind-go/useridentity.
type View = vb.View

// DefaultDir is the default user-identity directory. See voidbind-go/useridentity.
func DefaultDir() (string, error) { return vb.DefaultDir() }

// NewStore opens a user-identity store. See voidbind-go/useridentity.
func NewStore(opts StoreOptions) (*Store, error) { return vb.NewStore(opts) }

// NewView renders an Identity for display. See voidbind-go/useridentity.
func NewView(i Identity) View { return vb.NewView(i) }

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
