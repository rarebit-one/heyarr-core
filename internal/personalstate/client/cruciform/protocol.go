package cruciform

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// The offload exchange is two opaque relay messages, mutually authenticated.
//
//	desktop --request-->  phone   (signed by the desktop TRANSPORT key)
//	desktop <--response-- phone   (signed by the phone DEVICE key; space key
//	                               sealed to the desktop's per-unwrap eph key)
//
// Both messages ride the pairing relay (ADR-0002), which is opaque and untrusted:
// it forwards bytes it cannot open and could try to substitute them. The
// signatures are what defeat substitution — the request proves "your paired
// desktop asked" (the confused-deputy gate a wake-with-no-QR reopens), and the
// response proves "the phone answered", so a malicious relay cannot swap in a
// key the desktop would then trust. A per-exchange nonce binds the response to
// its request. The wire carries NO secret in the clear: the request's fields are
// public or already-opaque (the wrapped space key), and the space key comes back
// sealed to an ephemeral key only this unwrap holds.

// wireVersion prefixes every record so a future field change is a parse failure,
// not a misread (mirrors pairflow.InviteVersion).
const wireVersion byte = 1

// Domain tags separate the request and response signature inputs so a signature
// over one can never be replayed as the other (voidbind's cosig tag convention,
// e.g. "voidbind-cosig-v1\x00").
const (
	requestDomain  = "heyarr-cruciform-unwrap-req-v1\x00"
	responseDomain = "heyarr-cruciform-unwrap-resp-v1\x00"
)

// nonceLen is the per-exchange anti-replay nonce length.
const nonceLen = 16

// maxField bounds a single length-prefixed field. The real payloads are tiny (a
// wrapped space key is ~100 bytes) and the whole record must fit the relay's
// 64 KiB slot; this cap keeps a malformed length from allocating unboundedly.
const maxField = 4096

// Protocol sentinels.
var (
	ErrMalformed            = errors.New("cruciform: malformed offload message")
	ErrBadRequestSignature  = errors.New("cruciform: request signature does not verify against the paired desktop key")
	ErrBadResponseSignature = errors.New("cruciform: response signature does not verify against the phone device key")
	ErrNonceMismatch        = errors.New("cruciform: response nonce does not match the request")
	ErrWrongTransportKeyLen = errors.New("cruciform: transport signing key is not an ed25519 private key")
	ErrWrongPhoneKeyLen     = errors.New("cruciform: phone device key is not an ed25519 public key")
)

// request is the desktop's opaque ask, posted to the relay. Wrapped is the
// phone's wrapped copy of the space key (opaque ciphertext); EphPub is the
// desktop's per-unwrap X25519 public key the phone seals the answer to; Nonce
// binds the response; Sig is an ed25519 signature by the desktop transport key
// over everything else. There is deliberately no space id — the Unwrapper seam
// (Unwrap(wrapped)) is not given one, and the phone does not need it to run the
// agreement against its own key.
type request struct {
	Wrapped []byte
	EphPub  []byte
	Nonce   []byte
	Sig     []byte
}

// response is the phone's answer. Nonce echoes the request; Sealed is the space
// key sealed to the request's EphPub (encryption.Seal); Sig is an ed25519
// signature by the phone device key over Nonce+Sealed.
type response struct {
	Nonce  []byte
	Sealed []byte
	Sig    []byte
}

// requestSigningInput is the canonical byte string the request signature covers:
// the domain tag, the version, and the length-prefixed public fields — never the
// signature itself. Verify recomputes it from the parsed fields, so the encoding
// is authenticated, not just the payloads.
func requestSigningInput(wrapped, ephPub, nonce []byte) []byte {
	var b bytes.Buffer
	b.WriteString(requestDomain)
	b.WriteByte(wireVersion)
	putField(&b, wrapped)
	putField(&b, ephPub)
	putField(&b, nonce)
	return b.Bytes()
}

// signRequest builds a signed, marshaled request ready to hand to the relay.
func signRequest(key ed25519.PrivateKey, wrapped, ephPub, nonce []byte) ([]byte, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, ErrWrongTransportKeyLen
	}
	sig := ed25519.Sign(key, requestSigningInput(wrapped, ephPub, nonce))
	return marshalRequest(request{Wrapped: wrapped, EphPub: ephPub, Nonce: nonce, Sig: sig}), nil
}

// responseSigningInput is the canonical byte string the response signature covers.
func responseSigningInput(nonce, sealed []byte) []byte {
	var b bytes.Buffer
	b.WriteString(responseDomain)
	b.WriteByte(wireVersion)
	putField(&b, nonce)
	putField(&b, sealed)
	return b.Bytes()
}

// verifyResponse parses the phone's answer, checks the phone device signature,
// and checks the nonce echoes the one the desktop sent. The desktop calls it
// before unsealing — so a relay that swapped the sealed bytes for its own is
// caught here, not by a later failed frame decrypt.
func verifyResponse(phonePub ed25519.PublicKey, wantNonce, raw []byte) (response, error) {
	if len(phonePub) != ed25519.PublicKeySize {
		return response{}, ErrWrongPhoneKeyLen
	}
	resp, err := unmarshalResponse(raw)
	if err != nil {
		return response{}, err
	}
	if len(resp.Sig) != ed25519.SignatureSize ||
		!ed25519.Verify(phonePub, responseSigningInput(resp.Nonce, resp.Sealed), resp.Sig) {
		return response{}, ErrBadResponseSignature
	}
	if !bytes.Equal(resp.Nonce, wantNonce) {
		return response{}, ErrNonceMismatch
	}
	return resp, nil
}

func marshalRequest(r request) []byte {
	var b bytes.Buffer
	b.WriteByte(wireVersion)
	putField(&b, r.Wrapped)
	putField(&b, r.EphPub)
	putField(&b, r.Nonce)
	putField(&b, r.Sig)
	return b.Bytes()
}

func unmarshalResponse(raw []byte) (response, error) {
	r := bytes.NewReader(raw)
	if err := expectVersion(r); err != nil {
		return response{}, err
	}
	var resp response
	var err error
	if resp.Nonce, err = getField(r); err != nil {
		return response{}, err
	}
	if resp.Sealed, err = getField(r); err != nil {
		return response{}, err
	}
	if resp.Sig, err = getField(r); err != nil {
		return response{}, err
	}
	if r.Len() != 0 {
		return response{}, fmt.Errorf("%w: trailing bytes", ErrMalformed)
	}
	return resp, nil
}

func expectVersion(r *bytes.Reader) error {
	v, err := r.ReadByte()
	if err != nil {
		return fmt.Errorf("%w: missing version", ErrMalformed)
	}
	if v != wireVersion {
		return fmt.Errorf("%w: version %d, want %d", ErrMalformed, v, wireVersion)
	}
	return nil
}

func putField(b *bytes.Buffer, f []byte) {
	var l [4]byte
	// #nosec G115 -- f is a codec field (a key, nonce, signature, or a wrapped
	// space key), all far below 2^32; getField refuses anything over maxField on
	// the way back in, so the length always round-trips through uint32.
	binary.BigEndian.PutUint32(l[:], uint32(len(f)))
	b.Write(l[:])
	b.Write(f)
}

func getField(r *bytes.Reader) ([]byte, error) {
	var l [4]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return nil, fmt.Errorf("%w: short length", ErrMalformed)
	}
	n := binary.BigEndian.Uint32(l[:])
	if n > maxField {
		return nil, fmt.Errorf("%w: field of %d bytes exceeds %d", ErrMalformed, n, maxField)
	}
	f := make([]byte, n)
	if _, err := io.ReadFull(r, f); err != nil {
		return nil, fmt.Errorf("%w: short field", ErrMalformed)
	}
	return f, nil
}
