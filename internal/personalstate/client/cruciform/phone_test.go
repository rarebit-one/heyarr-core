package cruciform

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"net/url"

	"github.com/rarebit-one/heyarr-core/internal/pairing"
)

// The PHONE half of the offload wire contract, kept in a test file so it does
// not ship in the desktop binary. The desktop only ever signs requests,
// verifies responses and encodes invites; the phone, in its own implementation,
// verifies requests, signs responses and decodes invites. The tests play the
// phone with these, so both directions of the contract have one definition
// here and are exercised against each other.

// verifyRequest parses a request off the relay and checks its signature against
// the pinned desktop transport public key. The phone calls this before gating.
func verifyRequest(pub ed25519.PublicKey, raw []byte) (request, error) {
	if len(pub) != ed25519.PublicKeySize {
		return request{}, ErrWrongTransportKeyLen
	}
	req, err := unmarshalRequest(raw)
	if err != nil {
		return request{}, err
	}
	if len(req.Sig) != ed25519.SignatureSize ||
		!ed25519.Verify(pub, requestSigningInput(req.Wrapped, req.EphPub, req.Nonce), req.Sig) {
		return request{}, ErrBadRequestSignature
	}
	return req, nil
}

// signResponse builds a signed, marshaled response. The phone calls it after it
// has sealed the space key to the request's EphPub.
func signResponse(key ed25519.PrivateKey, nonce, sealed []byte) ([]byte, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, ErrWrongPhoneKeyLen
	}
	sig := ed25519.Sign(key, responseSigningInput(nonce, sealed))
	return marshalResponse(response{Nonce: nonce, Sealed: sealed, Sig: sig}), nil
}

func unmarshalRequest(raw []byte) (request, error) {
	r := bytes.NewReader(raw)
	if err := expectVersion(r); err != nil {
		return request{}, err
	}
	var req request
	var err error
	if req.Wrapped, err = getField(r); err != nil {
		return request{}, err
	}
	if req.EphPub, err = getField(r); err != nil {
		return request{}, err
	}
	if req.Nonce, err = getField(r); err != nil {
		return request{}, err
	}
	if req.Sig, err = getField(r); err != nil {
		return request{}, err
	}
	if r.Len() != 0 {
		return request{}, fmt.Errorf("%w: trailing bytes", ErrMalformed)
	}
	return req, nil
}

func marshalResponse(r response) []byte {
	var b bytes.Buffer
	b.WriteByte(wireVersion)
	putField(&b, r.Nonce)
	putField(&b, r.Sealed)
	putField(&b, r.Sig)
	return b.Bytes()
}

// Invite is a decoded offload pairing invite — the phone half's parse target.
type Invite struct {
	RelayBase string
	Session   string
	Salt      []byte
}

// DecodeInvite parses an invite URI back into its parts, refusing a wrong scheme,
// opaque, version, or a salt below the freshness floor. It is here so the wire
// contract has one definition the phone half mirrors and tests exercise.
func DecodeInvite(s string) (Invite, error) {
	u, err := url.Parse(s)
	if err != nil {
		return Invite{}, fmt.Errorf("%w: %w", ErrMalformedInvite, err)
	}
	if u.Scheme != InviteScheme || u.Opaque != inviteOpaque {
		return Invite{}, fmt.Errorf("%w: not a %q:%s invite", ErrMalformedInvite, InviteScheme, inviteOpaque)
	}
	q := u.Query()
	if v := q.Get("v"); v != offloadInviteVer {
		return Invite{}, fmt.Errorf("%w: version %q, want %q", ErrMalformedInvite, v, offloadInviteVer)
	}
	inv := Invite{RelayBase: q.Get("relay"), Session: q.Get("session")}
	inv.Salt, err = hex.DecodeString(q.Get("salt"))
	if err != nil {
		return Invite{}, fmt.Errorf("%w: salt is not hex: %w", ErrMalformedInvite, err)
	}
	if inv.RelayBase == "" || inv.Session == "" {
		return Invite{}, fmt.Errorf("%w: missing relay or session", ErrMalformedInvite)
	}
	if len(inv.Salt) < pairing.MinSaltLen {
		return Invite{}, fmt.Errorf("%w: salt is %d bytes, want at least %d", ErrMalformedInvite, len(inv.Salt), pairing.MinSaltLen)
	}
	return inv, nil
}
