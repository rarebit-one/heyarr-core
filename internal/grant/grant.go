// Package grant is a thin shim over github.com/rarebit-one/voidbind-go/grant.
//
// The capability-grant implementation (signed expiring delegation, ADR-0048)
// was extracted into voidbind-go byte-for-byte and this package re-exports it.
// The adversarial synthetic test moved to voidbind-go, to live with the
// implementation it exercises. The unit tests live there too.
package grant

import vb "github.com/rarebit-one/voidbind-go/grant"

// Capability is re-exported from voidbind-go/grant.
type Capability = vb.Capability

// Grant is re-exported from voidbind-go/grant.
type Grant = vb.Grant

// Keys is re-exported from voidbind-go/grant.
type Keys = vb.Keys

// Reason is re-exported from voidbind-go/grant.
type Reason = vb.Reason

// Request is re-exported from voidbind-go/grant.
type Request = vb.Request

// TrustStore is re-exported from voidbind-go/grant.
type TrustStore = vb.TrustStore

// Re-exported grant constants, including the Capability and Reason values.
const (
	MaxTTL     = vb.MaxTTL
	SkewMargin = vb.SkewMargin
	Version    = vb.Version

	CapabilityRead  = vb.CapabilityRead
	CapabilityWrite = vb.CapabilityWrite

	ReasonOK                = vb.ReasonOK
	ReasonMalformed         = vb.ReasonMalformed
	ReasonUnknownIssuer     = vb.ReasonUnknownIssuer
	ReasonBadSignature      = vb.ReasonBadSignature
	ReasonPrincipalMismatch = vb.ReasonPrincipalMismatch
	ReasonResourceMismatch  = vb.ReasonResourceMismatch
	ReasonCapabilityDenied  = vb.ReasonCapabilityDenied
	ReasonNotYetValid       = vb.ReasonNotYetValid
	ReasonExpired           = vb.ReasonExpired
)

// Re-exported grant sentinels and functions.
var (
	ErrMalformed         = vb.ErrMalformed
	ErrUnknownIssuer     = vb.ErrUnknownIssuer
	ErrBadSignature      = vb.ErrBadSignature
	ErrPrincipalMismatch = vb.ErrPrincipalMismatch
	ErrResourceMismatch  = vb.ErrResourceMismatch
	ErrCapabilityDenied  = vb.ErrCapabilityDenied
	ErrNotYetValid       = vb.ErrNotYetValid
	ErrExpired           = vb.ErrExpired
	ErrTTLTooLong        = vb.ErrTTLTooLong
	ErrIncomplete        = vb.ErrIncomplete
	ErrIssuerMismatch    = vb.ErrIssuerMismatch

	Verify    = vb.Verify
	ReasonFor = vb.ReasonFor
)
