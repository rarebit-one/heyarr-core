// Package cruciform is the ADR-0098 "cruciform-offload" device-key custody
// backend: the desktop holds NO device encryption key at all. On unwrap it wakes
// the paired phone (one.rarebit.cruciform) over the voidbind notify plane
// (ADR-0005) and exchanges the wrapped space key over the pairing relay
// (ADR-0002); the phone hardware-gates (StrongBox/TEE + biometric), performs the
// X25519 agreement in its enclave, and returns the space key. The phone is the
// root of trust; the desktop is a terminal, consulted once per space-key unwrap
// (per space / per session / per rotation), NOT per file.
//
// # Shape: phone returns the space key, not a raw agreement (ADR-0098 §4)
//
// This backend implements personalstate/client.Unwrapper via a wake + relay
// round-trip — NOT via encryption.UnwrapWithAgreement. The agreement AND the
// assembly both run on the phone, which returns a fully-formed space key sealed
// to a per-unwrap ephemeral key. Contrast the yubikey/tpm backends, which inject
// a LOCAL encryption.AgreementFunc and assemble the key in this process.
//
// The alternative — offloading only the ECDH as a remote AgreementFunc — was
// rejected: it would turn the phone into a general X25519 decryption oracle
// (compute ECDH(phone_priv, X) for any X and return the raw shared secret), so
// anyone who could reach the phone past the gate could decrypt anything ever
// wrapped to it. Returning an already-assembled space key for a blob the phone
// unwraps against its own key is a far narrower capability, and gains no
// confidentiality loss because the space key lands in this process either way
// (that is ADR-0098 §4 by design — what offload protects is the long-term device
// key, which never leaves the phone).
//
// # Transport is a seam
//
// The wake+relay wiring is behind the [Transport] interface. The concrete
// notify+relay transport (and the phone half in voidbind-kmp) is deferred — this
// package is the desktop-side backend and its authenticated protocol, unit-tested
// against a fake transport; a live round-trip is gated on a reachable paired
// phone, exactly as the yubikey backend gated its on-card round-trip.
package cruciform

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
)

// DefaultTimeout bounds a single offload round-trip. It is generous: the phone
// wakes over push and a human approves a biometric prompt, so seconds-to-tens-of-
// seconds is normal, not an error.
const DefaultTimeout = 90 * time.Second

// Transport wakes the paired phone and carries one opaque request to it and one
// opaque response back, over the voidbind notify + pairing-relay planes. It holds
// no key and never inspects the bytes: everything it forwards is public or
// already-sealed (the request carries no secret; the space key comes back sealed
// to a key only this unwrap holds). A live implementation creates a relay
// session, posts the request, wakes the phone with an opaque pointer to that
// session, and polls the relay for the response until ctx is done.
type Transport interface {
	RoundTrip(ctx context.Context, request []byte) (response []byte, err error)
}

// Unwrapper is the cruciform-offload backend. It implements
// personalstate/client.Unwrapper by a wake + relay round-trip to the paired
// phone. It holds the desktop's TRANSPORT signing key (which authenticates a
// request as coming from this paired terminal — the one-time pairing that pins it
// on the phone is a separate ceremony) and the phone's pinned DEVICE public key
// (which authenticates the response). Neither is the device ENCRYPTION key: this
// backend never holds one — that is the whole point.
type Unwrapper struct {
	transport    Transport
	transportKey ed25519.PrivateKey // signs requests; proves "the paired desktop asked"
	phonePub     ed25519.PublicKey  // verifies responses; proves "the phone answered"
	timeout      time.Duration
}

// Option configures an [Unwrapper].
type Option func(*Unwrapper)

// WithTimeout overrides [DefaultTimeout] for each round-trip.
func WithTimeout(d time.Duration) Option {
	return func(u *Unwrapper) {
		if d > 0 {
			u.timeout = d
		}
	}
}

// New returns a cruciform-offload Unwrapper. transportKey is this desktop's
// pairing key (ed25519 private); phonePub is the paired phone's pinned device key
// (ed25519 public). It fails if the transport is nil or either key is the wrong
// size.
func New(t Transport, transportKey ed25519.PrivateKey, phonePub ed25519.PublicKey, opts ...Option) (*Unwrapper, error) {
	if t == nil {
		return nil, errors.New("cruciform: a Transport is required")
	}
	if len(transportKey) != ed25519.PrivateKeySize {
		return nil, ErrWrongTransportKeyLen
	}
	if len(phonePub) != ed25519.PublicKeySize {
		return nil, ErrWrongPhoneKeyLen
	}
	u := &Unwrapper{transport: t, transportKey: transportKey, phonePub: phonePub, timeout: DefaultTimeout}
	for _, o := range opts {
		o(u)
	}
	return u, nil
}

// Unwrap recovers a space key wrapped to the paired phone by offloading the
// agreement to it. It mints a per-unwrap ephemeral X25519 key, asks the phone
// (over the transport) to unwrap and seal the space key to that ephemeral key,
// verifies the phone's signature and the nonce, and opens the sealed answer with
// the ephemeral key it alone holds. The ephemeral private key is discarded when
// this returns, so each unwrap is forward-secret against a later relay compromise.
func (u *Unwrapper) Unwrap(wrapped []byte) (encryption.SpaceKey, error) {
	eph, err := encryption.GenerateKey()
	if err != nil {
		return encryption.SpaceKey{}, fmt.Errorf("cruciform: minting the ephemeral key: %w", err)
	}
	ephPub := eph.PublicKey().Bytes()

	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return encryption.SpaceKey{}, fmt.Errorf("cruciform: nonce: %w", err)
	}

	reqBytes, err := signRequest(u.transportKey, wrapped, ephPub, nonce)
	if err != nil {
		return encryption.SpaceKey{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), u.timeout)
	defer cancel()

	respBytes, err := u.transport.RoundTrip(ctx, reqBytes)
	if err != nil {
		return encryption.SpaceKey{}, fmt.Errorf("cruciform: offload round-trip: %w", err)
	}

	resp, err := verifyResponse(u.phonePub, nonce, respBytes)
	if err != nil {
		return encryption.SpaceKey{}, err
	}

	// The phone sealed the space key to our ephemeral public key with the same
	// wrap primitive the controller uses, so opening it is an ordinary Unwrap
	// with the ephemeral private key we alone hold.
	key, err := encryption.Unwrap(resp.Sealed, eph)
	if err != nil {
		return encryption.SpaceKey{}, fmt.Errorf("cruciform: opening the phone's sealed reply: %w", err)
	}
	return key, nil
}
