package deviceauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/enrolment"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/voidbind-go/rp"
)

// Scheme is the HTTP Authorization scheme a device presents, alongside the
// existing "Bearer". A device has no token to bear; it presents its admitting
// membership op and a proof of possession, so it needs its own scheme rather
// than overloading Bearer with a value the bearer verifier would only reject.
const Scheme = "Device"

// credentialSeparator joins the two halves of a device credential: the
// admitting op (a user-signed cert, or a member-signed add — ADR-0068), and the
// device's fresh possession proof. It is enrolment's constant rather than a
// second spelling of "~", so the client that assembles the credential and this
// verifier that splits it cannot drift apart.
const credentialSeparator = enrolment.CredentialSeparator

// ErrMalformedCredential is a device credential that is not "<op>~<proof>".
var ErrMalformedCredential = errors.New("deviceauth: malformed device credential")

// Authenticated is a successfully device-authenticated caller: the principal it
// acts as, and the keys that proved it.
type Authenticated struct {
	PrincipalID   string
	PrincipalName string
	UserID        string
	UserKey       string
	DeviceKey     string
	// AdmittedBy is the hash of the op that admitted the device — the credential
	// it presented, as the evaluation accepted it (voidbind-go ADR-0007).
	AdmittedBy string
}

// Verify authenticates a presented device credential, merged with the
// membership ops the device presented beside it (the Voidbind-Membership
// header, ADR-0068), and resolves the principal it acts as — or returns why it
// does not.
//
// The chain is every check ADR-0048 requires, re-read under ADR-0068, in order,
// and each refuses distinctly so a rejected device and an attack are told apart:
//
//  1. the credential's claimed user is only a hint — used to find the pinned
//     genesis key;
//  2. the user must be pinned here (ErrUnknownUser) — enrol before trust;
//  3. the possession proof must be signed by the device key the credential
//     names and bound to this credential (the possession refusals) — the
//     caller HOLDS the key, it did not merely copy the op. It is checked
//     BEFORE the evaluation below because the evaluation writes (it records
//     ops and materialises the view): a caller who cannot prove the key
//     leaves nothing behind, and a leaked op alone never creates a row;
//  4. the identity's membership is EVALUATED (voidbind-go/rp, ADR-0007) over
//     the ops this node has recorded plus the ones presented, the ops it had
//     not seen are recorded and the device view reconciled, and the
//     credential's device must be a current member (the enrolment.* and rp.*
//     refusals) — some member, or the genesis key, really did admit this
//     device and nobody has since removed it;
//  5. the device's row in the view must not carry the admin's tombstone
//     (ErrDeviceRevoked) — the node's own local word against the log, kept
//     (ADR-0067). A member the view has no row for yet is materialised here
//     (a device admitted by a phone this node has never met authenticates on
//     first contact); a row held by a different user is ErrCertMismatch.
//
// No token is issued at any step: the identity is the device key and the
// signed op set, verified offline against a pinned genesis key.
func (s *Store) Verify(ctx context.Context, credential string, presented []string, now time.Time) (Authenticated, error) {
	opToken, proof, ok := strings.Cut(credential, credentialSeparator)
	if !ok || opToken == "" || proof == "" {
		return Authenticated{}, ErrMalformedCredential
	}
	op, err := parseOp(opToken)
	if err != nil {
		return Authenticated{}, err
	}
	if err := verifyPossession(proof, op.Device, opToken, now); err != nil {
		return Authenticated{}, err
	}

	user, auth, err := s.verifyMembership(ctx, opToken, presented, now)
	if err != nil {
		return Authenticated{}, err
	}

	device, err := s.memberDevice(ctx, user, auth.DeviceKey)
	if err != nil {
		return Authenticated{}, err
	}
	if err := device.Active(now); err != nil {
		return Authenticated{}, err
	}

	return Authenticated{
		PrincipalID:   user.PrincipalID,
		PrincipalName: user.Name,
		UserID:        user.ID,
		UserKey:       user.PublicKey,
		DeviceKey:     auth.DeviceKey,
		AdmittedBy:    auth.AdmittedBy,
	}, nil
}

// parseOp reads the credential's op — its structure and its own signature,
// nothing about whether the signer had authority — so the device key it names
// can be held against the possession proof before anything is evaluated or
// written. A token that is not an op at all is ErrMalformed, the cert-era
// sentinel the log labels and the tests assert.
func parseOp(opToken string) (enrolment.Op, error) {
	op, err := enrolment.VerifyOp(opToken)
	switch {
	case errors.Is(err, enrolment.ErrOpSignature):
		return enrolment.Op{}, fmt.Errorf("%w: %w", enrolment.ErrBadSignature, err)
	case err != nil:
		return enrolment.Op{}, fmt.Errorf("%w: %w", enrolment.ErrMalformed, err)
	}
	return op, nil
}

// memberDevice is step 5 of Verify: the row the view holds for a device the
// evaluation just admitted. A member with no row is materialised on the spot —
// the log knew every op already (so RecordOps had nothing to reconcile) but
// the view had never been built for it. A row held by a different user is
// refused: device_key is unique across users, and asserting the match here
// means a future change that breaks that invariant fails closed rather than
// authenticating as the wrong user.
func (s *Store) memberDevice(ctx context.Context, user User, deviceKey string) (Device, error) {
	device, err := s.LookupDevice(ctx, deviceKey)
	if errors.Is(err, ErrUnknownDevice) {
		if err := s.Reconcile(ctx, user.PublicKey); err != nil {
			return Device{}, err
		}
		device, err = s.LookupDevice(ctx, deviceKey)
	}
	if err != nil {
		return Device{}, err
	}
	if device.UserID != user.ID {
		return Device{}, ErrCertMismatch
	}
	return device, nil
}

// verifyMembership is steps 1, 2 and 4 of Verify: the credential's claimed user is a
// hint to find the pinned genesis key, the user must be pinned here, and the
// identity's membership — this node's op log merged with what the device
// presented — must, evaluated, admit the credential's device. It is shared
// with self-enrolment and the admin enrolment (ADR-0067) so the op a device
// enrols with is judged by exactly the rule it will later authenticate under
// — one verifier, not two that can drift.
//
// Steps 1 and 3 are voidbind-go/rp's verifier — the shared trust core All
// Thing and heyarr hold in common (ADR-0048, ADR-0068). It is backed by the
// single key resolved from the store, pinned under the claimed user, and by
// the store itself as the op log (rp.Membership): rp records the ops it
// accepted that this node had not seen, and RecordOps reconciles the device
// view in the same transaction. Step 2's ErrUnknownUser stays LookupUser's —
// the pin is present by construction here, so rp never reaches its own
// unknown-user gate.
func (s *Store) verifyMembership(ctx context.Context, opToken string, presented []string, now time.Time) (User, rp.Authenticated, error) {
	claimedUser, err := enrolment.OpUser(opToken)
	if err != nil {
		return User{}, rp.Authenticated{}, err
	}
	user, err := s.LookupUser(ctx, claimedUser)
	if err != nil {
		return User{}, rp.Authenticated{}, err
	}
	pinned, err := identity.ParsePublicKey(user.PublicKey)
	if err != nil {
		return User{}, rp.Authenticated{}, err
	}
	auth, err := rp.Verifier{
		Trust:      rp.MemTrust{claimedUser: pinned},
		Membership: s.Membership(ctx),
	}.Verify(opToken, presented, now)
	if err != nil {
		return User{}, rp.Authenticated{}, err
	}
	return user, auth, nil
}

// verifyPossession is step 3 of Verify: the proof must be signed by the
// credential's device key and bound to this credential — the caller HOLDS the
// key, it did not merely copy the op. Shared with self-enrolment for the same
// reason verifyMembership is.
func verifyPossession(proof, deviceKey, opToken string, now time.Time) error {
	devicePub, err := identity.ParsePublicKey(deviceKey)
	if err != nil {
		return err
	}
	if err := enrolment.VerifyPossession(proof, devicePub, opToken, now); err != nil {
		return err
	}
	// The window the proof gave ITSELF is checked only after the signature is,
	// so a caller learns nothing from an unsigned proof, and so the TTL check
	// can trust the numbers it reads.
	return withinMaxPossessionTTL(proof)
}

// MaxPossessionTTL is the longest life this node honours on a possession proof,
// whatever the device signed (#420).
//
// The proof's window IS its replay window: it is stateless by design, so a proof
// captured off a compromised channel is reusable until it expires, and until now
// nothing rejected a proof a device chose to make valid for an hour. The
// reference signer uses enrolment.PossessionTTL (two minutes); a real client has
// a reason to want longer — heyarr-mobile re-signs on a sealed key, which costs
// a biometric prompt each time — so the answer is a CEILING a client may batch
// beneath rather than a fixed value that would force a prompt per request.
//
// One hour is that ceiling for now. ADR-0070 chose ten minutes and said to
// revisit if a client appeared for which that was too short; heyarr-mobile was
// that client on day one (2026-09-03): its sealed key demands a fresh biometric
// per signature, so it mints hour-long proofs and every read was refused with
// device_possession_ttl_too_long. The right fix is on the client — a sealed
// key whose user-auth window outlives the proof, so ten-minute proofs cost
// nothing (heyarr-core #444) — after which this drops back to ten minutes. It
// is checked on `exp - iat` — the window the device CLAIMED — rather than on
// the time remaining, because the remaining time of a long proof looks
// perfectly ordinary a few minutes before it expires.
const MaxPossessionTTL = time.Hour

// ErrPossessionTTLTooLong is a proof whose own window exceeds MaxPossessionTTL.
// It is distinct from ErrPossessionExpired because it means the opposite thing:
// the proof is not stale, it was minted to live too long.
var ErrPossessionTTLTooLong = errors.New("deviceauth: possession proof lives longer than this node accepts")

// possessionWindow is the {iat, exp} a proof carries. The proof body is
// voidbind-go's, and its payload type is unexported there, so the two fields
// this node has a policy about are re-read here rather than the package being
// forked for a getter. Only these two are read: everything else about the proof
// — the version, the binding, the signature — is VerifyPossession's business and
// has already been judged by the time this runs.
type possessionWindow struct {
	IssuedAt int64 `json:"iat"`
	Expires  int64 `json:"exp"`
}

// withinMaxPossessionTTL refuses a proof that granted itself more than
// MaxPossessionTTL.
func withinMaxPossessionTTL(proof string) error {
	bodyEnc, _, ok := strings.Cut(proof, ".")
	if !ok {
		return enrolment.ErrPossessionMalformed
	}
	body, err := base64.RawURLEncoding.DecodeString(bodyEnc)
	if err != nil {
		return enrolment.ErrPossessionMalformed
	}
	var w possessionWindow
	if err := json.Unmarshal(body, &w); err != nil {
		return enrolment.ErrPossessionMalformed
	}
	// A window that ends before it starts is not a short proof, it is a
	// malformed one — VerifyPossession has no opinion about the ordering, so it
	// is refused here rather than passing as "well within the cap".
	if w.Expires < w.IssuedAt {
		return enrolment.ErrPossessionMalformed
	}
	if time.Duration(w.Expires-w.IssuedAt)*time.Second > MaxPossessionTTL {
		return ErrPossessionTTLTooLong
	}
	return nil
}
