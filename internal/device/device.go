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

// HintOption is re-exported from voidbind-go/device. It configures the prose a
// device renders about itself — see [WithCommandName].
type HintOption = vb.HintOption

// WithCommandName is re-exported from voidbind-go/device. heyarr passes
// WithCommandName("heyarr") so the `authorises` caveat names `heyarr identity
// enrol`, not the voidbind CLI the operator does not have (#369).
var WithCommandName = vb.WithCommandName

// CommandName is the binary heyarr presents as (matches root.go's `Use`). It is
// here, in heyarr's own device shim, so every place that renders a device reads
// the same name from one spot.
const CommandName = "heyarr"

// CommandHint is the option every heyarr device rendering passes so voidbind-go's
// `authorises` caveat names `heyarr …`, not `voidbind …` (#369). One instance,
// shared by every render site across the CLI and the Personal MCP.
var CommandHint = WithCommandName(CommandName)

// NotYetAuthorisingFor renders the un-enrolled caveat for a command name, for
// the one caller that has no Device to render: the Personal MCP's LIST response,
// whose top-level `authorises` must still speak for an empty list. voidbind-go
// exposes the parameterised string only through a Device, so we ask a zero
// (un-enrolled) device — its empty cert makes AuthorisationNote return exactly
// this caveat. Keeping the trick here means no render site repeats it.
func NotYetAuthorisingFor(opts ...HintOption) string {
	return vb.Device{}.AuthorisationNote(opts...)
}

// DefaultDir is the default device directory. See voidbind-go/device.
func DefaultDir() (string, error) { return vb.DefaultDir() }

// NewStore opens a device store. See voidbind-go/device.
func NewStore(opts StoreOptions) (*Store, error) { return vb.NewStore(opts) }

// NewView renders a Device for display. See voidbind-go/device.
func NewView(d Device, opts ...HintOption) View { return vb.NewView(d, opts...) }

// NewViews renders a list of Devices for display. See voidbind-go/device.
func NewViews(devices []Device, opts ...HintOption) []View { return vb.NewViews(devices, opts...) }

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
)

// NotYetAuthorising is re-exported from voidbind-go/device. It is a var, not a
// const, because upstream renders it from the default command name at init
// (voidbind-go#13) — the default (voidbind) string, for callers that want the
// unparameterised caveat. heyarr's own renderings use [CommandHint] instead.
var NotYetAuthorising = vb.NotYetAuthorising

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
