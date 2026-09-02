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

// The membership op-set (voidbind-go ADR-0007, heyarr ADR-0068): a v3 op, its
// kind, and the evaluated View an identity's device set is read from. A v1/v2
// Cert is a genesis-signed add in this vocabulary, so VerifyOp reads one too.
type (
	// Op is re-exported from voidbind-go/enrolment.
	Op = vb.Op
	// OpKind is re-exported from voidbind-go/enrolment.
	OpKind = vb.OpKind
	// Cosig is re-exported from voidbind-go/enrolment (reserved, ADR-0007).
	Cosig = vb.Cosig
	// Member is re-exported from voidbind-go/enrolment.
	Member = vb.Member
	// View is re-exported from voidbind-go/enrolment.
	View = vb.View
)

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

	OpVersion  = vb.OpVersion
	HashPrefix = vb.HashPrefix
	MaxPrev    = vb.MaxPrev
	OpAdd      = vb.OpAdd
	OpRemove   = vb.OpRemove

	ReasonForeignUser  = vb.ReasonForeignUser
	ReasonBadPrev      = vb.ReasonBadPrev
	ReasonUnauthorised = vb.ReasonUnauthorised
	ReasonOutranked    = vb.ReasonOutranked
	ReasonRemoved      = vb.ReasonRemoved
	ReasonSuperseded   = vb.ReasonSuperseded
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

	ErrOpMalformed = vb.ErrOpMalformed
	ErrOpSignature = vb.ErrOpSignature
	ErrOpGenesis   = vb.ErrOpGenesis
	ErrOpNoPrev    = vb.ErrOpNoPrev
	ErrNoUser      = vb.ErrNoUser

	CertUser             = vb.CertUser
	SignCert             = vb.SignCert
	VerifyCert           = vb.VerifyCert
	SignPossession       = vb.SignPossession
	VerifyPossession     = vb.VerifyPossession
	ReasonForCert        = vb.ReasonForCert
	GenerateUserIdentity = vb.GenerateUserIdentity

	SignOp   = vb.SignOp
	VerifyOp = vb.VerifyOp
	OpHash   = vb.OpHash
	OpUser   = vb.OpUser
	Evaluate = vb.Evaluate
	Merge    = vb.Merge
)
