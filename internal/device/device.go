package device

// This package's implementation now lives in voidbind-go; see doc.go for what the
// device store is. Heyarr's identity core is Voidbind (the extracted
// self-sovereign authenticator); this file re-exports voidbind-go/device so
// Heyarr's import path and public surface stay stable. See
// rarebit-one/heyarr-core#366.
//
// Two on-disk behaviours changed with the rename, both backward-compatible on
// read: the key-file prefixes are now "voidbind-device-…" (legacy "heyarr-device-…"
// still accepted) and the directory override is VOIDBIND_DEVICE_DIR (was
// HEYARR_DEVICE_DIR).

import vb "github.com/rarebit-one/voidbind-go/device"

// Clock is re-exported from voidbind-go/device.
type Clock = vb.Clock

// Device is re-exported from voidbind-go/device.
type Device = vb.Device

// Store is re-exported from voidbind-go/device.
type Store = vb.Store

// StoreOptions is re-exported from voidbind-go/device.
type StoreOptions = vb.StoreOptions

// View is re-exported from voidbind-go/device.
type View = vb.View

// DefaultDir is the default device directory. See voidbind-go/device.
func DefaultDir() (string, error) { return vb.DefaultDir() }

// NewStore opens a device store. See voidbind-go/device.
func NewStore(opts StoreOptions) (*Store, error) { return vb.NewStore(opts) }

// NewView renders a Device for display. See voidbind-go/device.
func NewView(d Device) View { return vb.NewView(d) }

// NewViews renders a list of Devices for display. See voidbind-go/device.
func NewViews(devices []Device) []View { return vb.NewViews(devices) }

// Constants.
const (
	KeyFileName           = vb.KeyFileName
	RecordFileName        = vb.RecordFileName
	EncryptionKeyFileName = vb.EncryptionKeyFileName
	CertFileName          = vb.CertFileName
	EnrolmentNotEnrolled  = vb.EnrolmentNotEnrolled
	EnrolmentEnrolled     = vb.EnrolmentEnrolled
	Algorithm             = vb.Algorithm
	DirMode               = vb.DirMode
	KeyFileMode           = vb.KeyFileMode
	EnvDir                = vb.EnvDir

	EnrolledNotAuthorising = vb.EnrolledNotAuthorising
	NotYetAuthorising      = vb.NotYetAuthorising
)

// Sentinel errors.
var (
	ErrKeyPermissions   = vb.ErrKeyPermissions
	ErrMalformedKey     = vb.ErrMalformedKey
	ErrUnknownDevice    = vb.ErrUnknownDevice
	ErrNoDevice         = vb.ErrNoDevice
	ErrDeviceExists     = vb.ErrDeviceExists
	ErrCertNotForDevice = vb.ErrCertNotForDevice
	ErrNotEnrolled      = vb.ErrNotEnrolled
)
