// Package enrolment is a thin shim over github.com/rarebit-one/voidbind-go/enrolment.
//
// The cert + possession-proof implementation was extracted into voidbind-go
// (byte-for-byte, including the heyarr/* wire labels), and this package now
// re-exports it. It is a dedup, not a migration: the cert format, the Version,
// and every label are identical, so certs and possession proofs made before the
// extraction verify unchanged. Tests live with the implementation, in voidbind-go.
package enrolment

import vb "github.com/rarebit-one/voidbind-go/enrolment"

// Cert is re-exported from voidbind-go/enrolment.
type Cert = vb.Cert

// Reason is re-exported from voidbind-go/enrolment.
type Reason = vb.Reason

// UserIdentity is re-exported from voidbind-go/enrolment.
type UserIdentity = vb.UserIdentity

// Re-exported enrolment constants, including the Reason values.
const (
	CertLifetime        = vb.CertLifetime
	CredentialSeparator = vb.CredentialSeparator
	PossessionSkew      = vb.PossessionSkew
	PossessionTTL       = vb.PossessionTTL
	SkewMargin          = vb.SkewMargin
	Version             = vb.Version

	ReasonOK           = vb.ReasonOK
	ReasonMalformed    = vb.ReasonMalformed
	ReasonUnknownUser  = vb.ReasonUnknownUser
	ReasonBadSignature = vb.ReasonBadSignature
	ReasonNotYetValid  = vb.ReasonNotYetValid
	ReasonExpired      = vb.ReasonExpired
)

// Re-exported enrolment sentinels and functions.
var (
	ErrMalformed    = vb.ErrMalformed
	ErrUnknownUser  = vb.ErrUnknownUser
	ErrBadSignature = vb.ErrBadSignature
	ErrNotYetValid  = vb.ErrNotYetValid
	ErrExpired      = vb.ErrExpired
	ErrIncomplete   = vb.ErrIncomplete

	ErrPossessionMalformed = vb.ErrPossessionMalformed
	ErrPossessionSignature = vb.ErrPossessionSignature
	ErrPossessionExpired   = vb.ErrPossessionExpired
	ErrPossessionNotYet    = vb.ErrPossessionNotYet
	ErrPossessionCert      = vb.ErrPossessionCert

	CertUser             = vb.CertUser
	SignCert             = vb.SignCert
	VerifyCert           = vb.VerifyCert
	SignPossession       = vb.SignPossession
	VerifyPossession     = vb.VerifyPossession
	ReasonForCert        = vb.ReasonForCert
	GenerateUserIdentity = vb.GenerateUserIdentity
)
