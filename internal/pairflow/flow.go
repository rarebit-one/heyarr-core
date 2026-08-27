// Package pairflow is the device-pairing handshake (§40, ADR-0022, ADR-0038,
// #305): the transport flow that turns the internal/pairing PRIMITIVES — the SAS
// and the commitment — into an actual enrolment of a new device by an old one.
//
// It is the relay flow the pairing package's doc calls a deliberate follow-up.
// Two parties exchange, through a DUMB RELAY, the values needed to compute the
// short authentication string and, on a human match, one signs an enrolment cert
// the other stores. The relay is untrusted (ADR-0038): every value that crosses
// it is public — two commitments, two public keys, a salt, and a signed cert —
// and this package proves that by construction (see TestRelayLearnsNoKeyMaterial).
//
// # The order this file exists to hold: commit before reveal
//
// The security of a short SAS rests on neither side being able to choose its key
// with knowledge of the other's (see internal/pairing/commitment.go). This flow
// is what enforces that ordering: each side publishes its COMMITMENT first, and
// reveals its key only after the peer's commitment is in; and each side, on
// receiving the peer's revealed key, calls [pairing.Commitment.Open] to check it
// against the commitment the peer published earlier. Remove that Open call — let
// a party reveal a key it did not commit to — and the rushing attack is back:
// that is the sabotage this package's TestRushingSubstitutionIsRefused fires on.
//
// # Roles
//
// The INITIATOR is the already-enrolled ("old") device. Its SAS input is the
// USER identity public key it holds, and on a confirmed match it signs a cert
// for the responder's device key with the user private key. The RESPONDER is the
// new device; its SAS input is its freshly generated device public key, and on
// success it stores the cert it is handed. Both derive the SAME SAS iff they
// exchanged the same two keys under the same salt — which a MITM cannot arrange
// without substituting a key, which changes the SAS, which the humans catch.
//
// This package holds no keys and signs nothing itself: the initiator supplies a
// [Initiator.Sign] callback (the user-identity store's SignCert) and the
// responder an [Responder.Accept] callback (the device store's Enrol), so the
// private keys stay in the stores that own them and never enter the flow.
package pairflow

import (
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/pairing"
)

// The relay slots, one value each, written once. Their names are the wire
// contract between the two roles and the relay; they are fixed strings rather
// than an enum type because the relay treats them as opaque path segments.
const (
	SlotInitiatorCommit = "initiator_commit"
	SlotResponderCommit = "responder_commit"
	SlotInitiatorReveal = "initiator_reveal"
	SlotResponderReveal = "responder_reveal"
	SlotCert            = "cert"
	SlotAbort           = "abort"
)

// DefaultPollInterval is how often a role re-checks the relay for a slot it is
// waiting on. It is small because both parties are usually a screen-tap apart;
// the deadline that bounds the wait is the caller's context, not this.
const DefaultPollInterval = 150 * time.Millisecond

// The errors a handshake ends in, distinct because each is a different thing to
// tell the human: a mismatch is "the codes did not match, refuse", an aborted
// peer is "the other device gave up", a commitment failure is an ATTACK.
var (
	// ErrSASRefused is the confirm callback returning false: the human said the
	// two codes do not match, so the flow stops before any cert is signed. It is
	// the ordinary refusal, and it is a non-zero exit for the CLI.
	ErrSASRefused = errors.New("pairflow: pairing refused — the short codes did not match")
	// ErrPeerAborted is the other side having written the abort slot — it
	// refused, or timed out, and said so, so this side stops promptly rather
	// than waiting out its own deadline.
	ErrPeerAborted = errors.New("pairflow: the other device aborted the pairing")
	// ErrCommitmentMismatch is a peer that revealed a key it had not committed
	// to — the rushing attacker's tell, surfaced from pairing.Commitment.Open.
	// It is fatal and loud: it is never a benign race.
	ErrCommitmentMismatch = errors.New("pairflow: a device revealed a key it did not commit to (rushing attack)")
)

// Relay is the dumb transport the two devices exchange through. It stores an
// opaque value per (session, slot) and hands it back; it learns nothing and
// vouches for nothing (ADR-0038). An in-memory map satisfies it for tests; an
// HTTP relay satisfies it in production (internal/pairrelay).
//
// Put is write-once per slot from this flow's point of view — it never rewrites a
// slot — so an implementation may reject a conflicting overwrite. Get reports
// found=false for a slot not yet written, which is the ordinary "keep waiting".
type Relay interface {
	Put(ctx context.Context, session, slot string, data []byte) error
	Get(ctx context.Context, session, slot string) (data []byte, found bool, err error)
}

// Result is what a completed (or refused) handshake reports to its caller.
type Result struct {
	// SAS is the short authentication string this side derived. Both sides print
	// it; the humans compare them. It is set whenever the handshake got far
	// enough to derive one, refused or not.
	SAS pairing.SAS
	// PeerKey is the authenticated key the peer contributed: the responder's
	// device key (to the initiator) or the user identity key (to the responder).
	PeerKey ed25519.PublicKey
	// PeerEncKey is the peer's X25519 encryption key, when it has one: the
	// responder's device encryption key (to the initiator), so the cert can bind
	// it (§41, ADR-0049). Empty when the peer contributed none (the user identity,
	// or a pre-Milestone-9 device).
	PeerEncKey []byte
	// Confirmed is whether the human accepted the SAS on this side.
	Confirmed bool
	// Cert is the enrolment cert: signed by the initiator, stored by the
	// responder. Empty on a refused or aborted handshake.
	Cert string
}

// pollInterval falls back to the default for a zero value, so a caller need not
// set it.
func pollInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultPollInterval
	}
	return d
}

// Initiator is the old, already-enrolled device's side of the handshake.
type Initiator struct {
	Relay        Relay
	Session      string
	PollInterval time.Duration

	// UserPub is the user identity public key this device holds — its SAS input
	// and the authority the responder authenticates. Exactly ed25519.PublicKeySize.
	UserPub ed25519.PublicKey
	// Salt is a fresh per-session salt (pairing.NewSalt). It travels in this
	// side's reveal; it is public and need not be committed (substituting it can
	// only cause a mismatch, never a false match).
	Salt []byte

	// Confirm is the human comparison. Given the derived SAS it returns true to
	// proceed (sign the cert) or false to refuse. Required.
	Confirm func(pairing.SAS) (bool, error)
	// Sign issues a user-signed enrolment cert for the responder's device keys —
	// its signing key and its X25519 encryption key (§41, ADR-0049) — using the
	// user private key the flow never sees. The encryption key is empty for a
	// pre-Milestone-9 responder. Required.
	Sign func(responderDeviceKey ed25519.PublicKey, responderEncKey []byte) (certToken string, err error)
}

// Run drives the initiator through the handshake and, on a confirmed match,
// posts a cert for the responder. It is safe to run concurrently with the
// matching Responder against the same relay session.
func (in Initiator) Run(ctx context.Context) (Result, error) {
	if len(in.UserPub) != ed25519.PublicKeySize {
		return Result{}, fmt.Errorf("pairflow: initiator user key is %d bytes, want %d", len(in.UserPub), ed25519.PublicKeySize)
	}
	if len(in.Salt) < pairing.MinSaltLen {
		return Result{}, fmt.Errorf("pairflow: salt is %d bytes, want at least %d", len(in.Salt), pairing.MinSaltLen)
	}
	if in.Confirm == nil || in.Sign == nil {
		return Result{}, errors.New("pairflow: initiator needs both a Confirm and a Sign callback")
	}
	interval := pollInterval(in.PollInterval)

	// 1. Commit to the user key, before revealing anything. The user identity has
	//    no encryption key, so its committed enc key is empty (framed, still bound).
	commit, err := pairing.Commit(in.UserPub, nil)
	if err != nil {
		return Result{}, err
	}
	if err := in.Relay.Put(ctx, in.Session, SlotInitiatorCommit, commit); err != nil {
		return Result{}, fmt.Errorf("pairflow: posting the initiator commitment: %w", err)
	}

	// 2. Wait for the responder's commitment. Only then reveal — commit before
	//    reveal, on this side.
	if _, err := waitSlot(ctx, in.Relay, in.Session, SlotResponderCommit, interval); err != nil {
		return Result{}, err
	}
	respCommit, _, err := in.Relay.Get(ctx, in.Session, SlotResponderCommit)
	if err != nil {
		return Result{}, fmt.Errorf("pairflow: reading the responder commitment: %w", err)
	}

	// 3. Reveal the user key and the salt (the user identity carries no enc key).
	if err := in.Relay.Put(ctx, in.Session, SlotInitiatorReveal, revealBytes(in.UserPub, nil, in.Salt)); err != nil {
		return Result{}, fmt.Errorf("pairflow: posting the initiator reveal: %w", err)
	}

	// 4. Wait for the responder's revealed device key.
	respReveal, err := waitSlot(ctx, in.Relay, in.Session, SlotResponderReveal, interval)
	if err != nil {
		return Result{}, err
	}
	respPub, respEnc, _, err := parseReveal(respReveal, false)
	if err != nil {
		abort(ctx, in.Relay, in.Session, "responder reveal malformed")
		return Result{}, err
	}

	// 5. THE CHECK. The revealed keys must open the commitment the responder
	//    published in step 2 — BOTH the signing and the encryption key. Without
	//    this, the responder could commit to one pair and reveal another (the
	//    rushing attack), including substituting only its encryption key.
	if err := pairing.Commitment(respCommit).Open(respPub, respEnc); err != nil {
		abort(ctx, in.Relay, in.Session, "commitment mismatch")
		return Result{}, fmt.Errorf("%w: %w", ErrCommitmentMismatch, err)
	}

	// 6. Derive the SAS and ask the human. The v2 SAS binds the responder's
	//    ENCRYPTION key alongside its signing key (§41, ADR-0049), so a relay that
	//    substitutes the enc key yields a different string the humans catch. The
	//    user identity contributes no enc key.
	sas, err := pairing.Derive(pairing.Keys{Sign: in.UserPub}, pairing.Keys{Sign: respPub, Enc: respEnc}, in.Salt)
	if err != nil {
		abort(ctx, in.Relay, in.Session, "derive failed")
		return Result{}, err
	}
	res := Result{SAS: sas, PeerKey: respPub, PeerEncKey: respEnc}
	ok, err := in.Confirm(sas)
	if err != nil {
		abort(ctx, in.Relay, in.Session, "confirm error")
		return res, err
	}
	if !ok {
		abort(ctx, in.Relay, in.Session, "sas mismatch")
		return res, ErrSASRefused
	}
	res.Confirmed = true

	// 7. Sign the cert for the responder's device keys (signing + encryption) and
	//    post it, so the cert binds the enc key the SAS just authenticated (§41).
	cert, err := in.Sign(respPub, respEnc)
	if err != nil {
		abort(ctx, in.Relay, in.Session, "sign failed")
		return res, fmt.Errorf("pairflow: signing the enrolment cert: %w", err)
	}
	if err := in.Relay.Put(ctx, in.Session, SlotCert, []byte(cert)); err != nil {
		return res, fmt.Errorf("pairflow: posting the cert: %w", err)
	}
	res.Cert = cert
	return res, nil
}

// Responder is the new device's side of the handshake.
type Responder struct {
	Relay        Relay
	Session      string
	PollInterval time.Duration

	// DevicePub is this device's freshly generated public key — its SAS input
	// and the key the cert will bind. Exactly ed25519.PublicKeySize.
	DevicePub ed25519.PublicKey
	// DeviceEnc is this device's X25519 encryption public key (§41, ADR-0049),
	// committed and revealed alongside DevicePub so the v2 SAS binds it and the
	// cert can wrap-target it. Empty for a pre-Milestone-9 device, which pairs
	// with a v1-shaped SAS the v2 primitive derives identically.
	DeviceEnc []byte

	// Confirm is the human comparison, as on the initiator. Required.
	Confirm func(pairing.SAS) (bool, error)
	// Accept stores the enrolment cert the initiator signed (the device store's
	// Enrol). It is handed the raw cert token. Required.
	Accept func(certToken string) error
}

// Run drives the responder through the handshake and, on success, accepts the
// cert the initiator posts.
func (rp Responder) Run(ctx context.Context) (Result, error) {
	if len(rp.DevicePub) != ed25519.PublicKeySize {
		return Result{}, fmt.Errorf("pairflow: responder device key is %d bytes, want %d", len(rp.DevicePub), ed25519.PublicKeySize)
	}
	if rp.Confirm == nil || rp.Accept == nil {
		return Result{}, errors.New("pairflow: responder needs both a Confirm and an Accept callback")
	}
	interval := pollInterval(rp.PollInterval)

	// 1. Wait for the initiator's commitment before revealing anything —
	//    commit before reveal, on this side.
	if _, err := waitSlot(ctx, rp.Relay, rp.Session, SlotInitiatorCommit, interval); err != nil {
		return Result{}, err
	}
	initCommit, _, err := rp.Relay.Get(ctx, rp.Session, SlotInitiatorCommit)
	if err != nil {
		return Result{}, fmt.Errorf("pairflow: reading the initiator commitment: %w", err)
	}

	// 2. Commit to the device keys — signing AND encryption (§41, ADR-0049).
	commit, err := pairing.Commit(rp.DevicePub, rp.DeviceEnc)
	if err != nil {
		return Result{}, err
	}
	if err := rp.Relay.Put(ctx, rp.Session, SlotResponderCommit, commit); err != nil {
		return Result{}, fmt.Errorf("pairflow: posting the responder commitment: %w", err)
	}

	// 3. Reveal the device keys (after committing, and after seeing the
	//    initiator's commitment). The responder carries no salt.
	if err := rp.Relay.Put(ctx, rp.Session, SlotResponderReveal, revealBytes(rp.DevicePub, rp.DeviceEnc, nil)); err != nil {
		return Result{}, fmt.Errorf("pairflow: posting the responder reveal: %w", err)
	}

	// 4. Wait for the initiator's revealed user key and salt.
	initReveal, err := waitSlot(ctx, rp.Relay, rp.Session, SlotInitiatorReveal, interval)
	if err != nil {
		return Result{}, err
	}
	userPub, _, salt, err := parseReveal(initReveal, true)
	if err != nil {
		abort(ctx, rp.Relay, rp.Session, "initiator reveal malformed")
		return Result{}, err
	}

	// 5. THE CHECK: the initiator's revealed key must open its commitment. The
	//    user identity has no encryption key, so an empty enc is what it committed.
	if err := pairing.Commitment(initCommit).Open(userPub, nil); err != nil {
		abort(ctx, rp.Relay, rp.Session, "commitment mismatch")
		return Result{}, fmt.Errorf("%w: %w", ErrCommitmentMismatch, err)
	}

	// 6. Derive the SAS and ask the human. The v2 SAS binds THIS device's
	//    encryption key (§41, ADR-0049); the user identity contributes none.
	sas, err := pairing.Derive(pairing.Keys{Sign: userPub}, pairing.Keys{Sign: rp.DevicePub, Enc: rp.DeviceEnc}, salt)
	if err != nil {
		abort(ctx, rp.Relay, rp.Session, "derive failed")
		return Result{}, err
	}
	res := Result{SAS: sas, PeerKey: userPub}
	ok, err := rp.Confirm(sas)
	if err != nil {
		abort(ctx, rp.Relay, rp.Session, "confirm error")
		return res, err
	}
	if !ok {
		abort(ctx, rp.Relay, rp.Session, "sas mismatch")
		return res, ErrSASRefused
	}
	res.Confirmed = true

	// 7. Wait for the cert — or for the initiator to abort (it may have refused
	//    the SAS on its side).
	cert, err := waitCertOrAbort(ctx, rp.Relay, rp.Session, interval)
	if err != nil {
		return res, err
	}
	if err := rp.Accept(cert); err != nil {
		return res, fmt.Errorf("pairflow: storing the enrolment cert: %w", err)
	}
	res.Cert = cert
	return res, nil
}

// revealBytes frames a reveal as three length-prefixed fields: the signing key,
// the encryption key, and the salt. The encryption key is empty for the user
// identity (which has none) and the salt is empty for the responder (which
// carries none); framing makes the three unambiguous whichever are empty. A
// fixed-offset split — as this used before the encryption key was added — could
// not tell an absent enc key from the first byte of the salt, so it had to become
// self-describing the moment the reveal carried two keys (§41, ADR-0049).
func revealBytes(pub ed25519.PublicKey, enc, salt []byte) []byte {
	var out []byte
	out = appendField(out, pub)
	out = appendField(out, enc)
	out = appendField(out, salt)
	return out
}

// appendField writes a uvarint length prefix and then the bytes.
func appendField(dst, b []byte) []byte {
	var hdr [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(hdr[:], uint64(len(b)))
	dst = append(dst, hdr[:n]...)
	return append(dst, b...)
}

// readField reads one length-prefixed field and returns it (copied) and the rest.
func readField(b []byte) (field, rest []byte, err error) {
	n, adv := binary.Uvarint(b)
	if adv <= 0 {
		return nil, nil, errors.New("truncated field length prefix")
	}
	b = b[adv:]
	if uint64(len(b)) < n {
		return nil, nil, fmt.Errorf("field claims %d bytes, %d remain", n, len(b))
	}
	return append([]byte(nil), b[:n]...), b[n:], nil
}

// parseReveal splits a framed reveal into the signing key, the encryption key
// (empty for the user identity), and, when withSalt, the salt. It refuses a
// signing key of the wrong length, a malformed frame, or (with a salt) one below
// the freshness floor — a truncated reveal is not something to derive on. The
// encryption key's length is not checked here: an empty one is legitimate (a
// pre-Milestone-9 device or the user identity), and pairing.Derive enforces the
// exact width when it binds a non-empty one.
func parseReveal(b []byte, withSalt bool) (pub ed25519.PublicKey, enc, salt []byte, err error) {
	signB, rest, err := readField(b)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("pairflow: a reveal's signing key is malformed: %w", err)
	}
	if len(signB) != ed25519.PublicKeySize {
		return nil, nil, nil, fmt.Errorf("pairflow: a revealed signing key is %d bytes, want %d", len(signB), ed25519.PublicKeySize)
	}
	enc, rest, err = readField(rest)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("pairflow: a reveal's encryption key is malformed: %w", err)
	}
	saltB, _, err := readField(rest)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("pairflow: a reveal's salt is malformed: %w", err)
	}
	if withSalt {
		if len(saltB) < pairing.MinSaltLen {
			return nil, nil, nil, fmt.Errorf("pairflow: a revealed salt is %d bytes, want at least %d", len(saltB), pairing.MinSaltLen)
		}
		salt = saltB
	}
	return ed25519.PublicKey(signB), enc, salt, nil
}

// waitSlot polls the relay for a slot until it appears, the peer aborts, or the
// context is done. It returns the slot's bytes. Every wait is abort-aware so a
// refusal on one side stops the other promptly rather than at its deadline.
func waitSlot(ctx context.Context, relay Relay, session, slot string, interval time.Duration) ([]byte, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if data, found, err := relay.Get(ctx, session, slot); err != nil {
			return nil, fmt.Errorf("pairflow: polling %s: %w", slot, err)
		} else if found {
			return data, nil
		}
		if aborted, err := checkAbort(ctx, relay, session); err != nil {
			return nil, err
		} else if aborted {
			return nil, ErrPeerAborted
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("pairflow: waiting for %s: %w", slot, ctx.Err())
		case <-ticker.C:
		}
	}
}

// waitCertOrAbort waits for the cert slot, returning ErrPeerAborted if the
// initiator writes the abort slot first.
func waitCertOrAbort(ctx context.Context, relay Relay, session string, interval time.Duration) (string, error) {
	data, err := waitSlot(ctx, relay, session, SlotCert, interval)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// checkAbort reports whether the abort slot has been written.
func checkAbort(ctx context.Context, relay Relay, session string) (bool, error) {
	_, found, err := relay.Get(ctx, session, SlotAbort)
	if err != nil {
		return false, fmt.Errorf("pairflow: checking for an abort: %w", err)
	}
	return found, nil
}

// abort records a refusal on the relay so the peer stops waiting. A best-effort
// write: if it fails, the peer falls back to its context deadline, which is why
// the error is deliberately not returned.
func abort(ctx context.Context, relay Relay, session, reason string) {
	_ = relay.Put(ctx, session, SlotAbort, []byte(reason))
}
