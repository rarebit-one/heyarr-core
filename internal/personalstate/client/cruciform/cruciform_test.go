package cruciform

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
)

// The backend implements the client.Unwrapper seam (ADR-0098) — the whole point.
var _ client.Unwrapper = (*Unwrapper)(nil)

// transportFunc adapts a closure to the Transport seam.
type transportFunc func(ctx context.Context, req []byte) ([]byte, error)

func (f transportFunc) RoundTrip(ctx context.Context, req []byte) ([]byte, error) {
	return f(ctx, req)
}

// phone is a reference cruciform: it holds the device signing key and the device
// X25519 encryption key, and answers an offload request exactly as the real app
// must — authenticate the desktop, run the agreement against its own key, seal
// the space key to the desktop's ephemeral key, and sign the reply.
type phone struct {
	sign ed25519.PrivateKey
	enc  *ecdh.PrivateKey
}

func newPhone(t *testing.T) phone {
	t.Helper()
	_, sign, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("phone signing key: %v", err)
	}
	enc, err := encryption.GenerateKey()
	if err != nil {
		t.Fatalf("phone encryption key: %v", err)
	}
	return phone{sign: sign, enc: enc}
}

// honestRoundTrip is the phone answering correctly, given the desktop's pinned
// transport public key (which it verifies the request against).
func (p phone) honestRoundTrip(desktopPub ed25519.PublicKey) transportFunc {
	return func(_ context.Context, reqBytes []byte) ([]byte, error) {
		req, err := verifyRequest(desktopPub, reqBytes)
		if err != nil {
			return nil, err
		}
		sk, err := encryption.Unwrap(req.Wrapped, p.enc)
		if err != nil {
			return nil, err
		}
		ephPub, err := ecdh.X25519().NewPublicKey(req.EphPub)
		if err != nil {
			return nil, err
		}
		sealed, err := encryption.Seal(sk, ephPub)
		if err != nil {
			return nil, err
		}
		return signResponse(p.sign, req.Nonce, sealed)
	}
}

// desktopKeys mints a transport (pairing) keypair for the desktop terminal.
func desktopKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("desktop transport key: %v", err)
	}
	return pub, priv
}

// wrapForPhone stands in for the controller: it mints a space key, wraps it to
// the phone's encryption key, and returns the wrapped blob plus a canary
// ciphertext sealed under the space key, so a caller can prove it recovered the
// SAME key by decrypting the canary.
func wrapForPhone(t *testing.T, p phone, canary []byte) (wrapped, canaryCT []byte) {
	t.Helper()
	sk, err := encryption.NewSpaceKey()
	if err != nil {
		t.Fatalf("space key: %v", err)
	}
	wrapped, err = encryption.Seal(sk, p.enc.PublicKey())
	if err != nil {
		t.Fatalf("wrap for phone: %v", err)
	}
	canaryCT, err = encryption.EncryptChange(sk, canary)
	if err != nil {
		t.Fatalf("canary: %v", err)
	}
	return wrapped, canaryCT
}

func TestNewValidates(t *testing.T) {
	t.Parallel()
	_, priv := desktopKeys(t)
	phonePub, _, _ := ed25519.GenerateKey(rand.Reader)
	fn := transportFunc(func(context.Context, []byte) ([]byte, error) { return nil, nil })

	if _, err := New(nil, priv, phonePub); err == nil {
		t.Error("nil transport: want error")
	}
	if _, err := New(fn, ed25519.PrivateKey("short"), phonePub); !errors.Is(err, ErrWrongTransportKeyLen) {
		t.Errorf("short transport key: got %v, want ErrWrongTransportKeyLen", err)
	}
	if _, err := New(fn, priv, ed25519.PublicKey("short")); !errors.Is(err, ErrWrongPhoneKeyLen) {
		t.Errorf("short phone key: got %v, want ErrWrongPhoneKeyLen", err)
	}
	if _, err := New(fn, priv, phonePub); err != nil {
		t.Errorf("valid New: %v", err)
	}
}

func TestOffloadRoundTripRecoversTheKey(t *testing.T) {
	t.Parallel()
	p := newPhone(t)
	desktopPub, desktopPriv := desktopKeys(t)
	canary := randBytes(t, 48)
	wrapped, canaryCT := wrapForPhone(t, p, canary)

	u, err := New(p.honestRoundTrip(desktopPub), desktopPriv, p.sign.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	key, err := u.Unwrap(wrapped)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	// The recovered key is the controller's key iff it opens the canary.
	got, err := encryption.DecryptChange(key, canaryCT)
	if err != nil {
		t.Fatalf("decrypt canary with recovered key: %v", err)
	}
	if !bytes.Equal(got, canary) {
		t.Fatalf("recovered key decrypts to %x, want the canary %x", got, canary)
	}
}

// TestWireCarriesNoSpaceKey is the offload analogue of notify's TestPingIsOpaque:
// the space key never crosses the relay in the clear. Neither the request nor the
// response wire bytes contain the canary plaintext, and the sealed reply opens
// only with the desktop's ephemeral key — a relay that kept the bytes learns
// nothing.
func TestWireCarriesNoSpaceKey(t *testing.T) {
	t.Parallel()
	p := newPhone(t)
	desktopPub, desktopPriv := desktopKeys(t)
	canary := randBytes(t, 48)
	wrapped, _ := wrapForPhone(t, p, canary)

	var reqSeen, respSeen []byte
	recording := transportFunc(func(ctx context.Context, req []byte) ([]byte, error) {
		reqSeen = append([]byte(nil), req...)
		resp, err := p.honestRoundTrip(desktopPub)(ctx, req)
		respSeen = append([]byte(nil), resp...)
		return resp, err
	})
	u, err := New(recording, desktopPriv, p.sign.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Unwrap(wrapped); err != nil {
		t.Fatalf("Unwrap: %v", err)
	}

	if bytes.Contains(reqSeen, canary) {
		t.Error("request wire bytes contain the canary plaintext")
	}
	if bytes.Contains(respSeen, canary) {
		t.Error("response wire bytes contain the canary plaintext")
	}
	// The reply is sealed to the unwrap's ephemeral key; a different key cannot
	// open it (so the relay, holding only the bytes, cannot).
	resp, err := unmarshalResponse(respSeen)
	if err != nil {
		t.Fatal(err)
	}
	other, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encryption.Unwrap(resp.Sealed, other); err == nil {
		t.Error("a foreign key opened the sealed reply")
	}
}

// TestResponseSubstitutionRefused: a malicious relay replaces the phone's reply
// with a space key of its own choosing, sealed to the (public) ephemeral key it
// read off the wire. Without the phone's signature the desktop refuses it, rather
// than trusting an attacker-chosen key.
func TestResponseSubstitutionRefused(t *testing.T) {
	t.Parallel()
	p := newPhone(t)
	desktopPub, desktopPriv := desktopKeys(t)
	wrapped, _ := wrapForPhone(t, p, randBytes(t, 16))

	substitute := transportFunc(func(_ context.Context, reqBytes []byte) ([]byte, error) {
		req, err := verifyRequest(desktopPub, reqBytes)
		if err != nil {
			return nil, err
		}
		evilKey, _ := encryption.NewSpaceKey()
		ephPub, _ := ecdh.X25519().NewPublicKey(req.EphPub)
		sealed, _ := encryption.Seal(evilKey, ephPub)
		// Sign with an attacker key, not the phone's.
		_, akpriv, _ := ed25519.GenerateKey(rand.Reader)
		return signResponse(akpriv, req.Nonce, sealed)
	})
	u, err := New(substitute, desktopPriv, p.sign.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Unwrap(wrapped); !errors.Is(err, ErrBadResponseSignature) {
		t.Fatalf("substituted reply: got %v, want ErrBadResponseSignature", err)
	}
}

// TestNonceReplayRefused: the phone signs a well-formed reply but for a stale
// nonce (a replayed earlier response). The desktop refuses it.
func TestNonceReplayRefused(t *testing.T) {
	t.Parallel()
	p := newPhone(t)
	desktopPub, desktopPriv := desktopKeys(t)
	wrapped, _ := wrapForPhone(t, p, randBytes(t, 16))

	staleNonce := randBytes(t, nonceLen)
	replay := transportFunc(func(_ context.Context, reqBytes []byte) ([]byte, error) {
		req, err := verifyRequest(desktopPub, reqBytes)
		if err != nil {
			return nil, err
		}
		sk, _ := encryption.Unwrap(req.Wrapped, p.enc)
		ephPub, _ := ecdh.X25519().NewPublicKey(req.EphPub)
		sealed, _ := encryption.Seal(sk, ephPub)
		return signResponse(p.sign, staleNonce, sealed) // wrong nonce
	})
	u, err := New(replay, desktopPriv, p.sign.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Unwrap(wrapped); !errors.Is(err, ErrNonceMismatch) {
		t.Fatalf("replayed nonce: got %v, want ErrNonceMismatch", err)
	}
}

// TestRequestAuthenticatesTheDesktop: the phone-side check refuses a request that
// is not signed by the pinned desktop transport key — the confused-deputy gate a
// wake-with-no-QR reopens. An attacker who can reach the relay cannot make the
// phone act as an unwrap oracle.
func TestRequestAuthenticatesTheDesktop(t *testing.T) {
	t.Parallel()
	desktopPub, desktopPriv := desktopKeys(t)
	_, attacker := desktopKeys(t) // a different transport key

	wrapped := randBytes(t, 64)
	ephPub := randBytes(t, 32)
	nonce := randBytes(t, nonceLen)

	// A request signed by a foreign transport key is refused — an attacker who
	// reaches the relay cannot make the phone act as an unwrap oracle.
	forged, err := signRequest(attacker, wrapped, ephPub, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifyRequest(desktopPub, forged); !errors.Is(err, ErrBadRequestSignature) {
		t.Fatalf("forged request: got %v, want ErrBadRequestSignature", err)
	}

	// A field tampered after a good signature is caught: the signature covers the
	// canonical fields, so swapping in a different ephemeral key breaks it.
	good, err := signRequest(desktopPriv, wrapped, ephPub, nonce)
	if err != nil {
		t.Fatal(err)
	}
	req, err := unmarshalRequest(good)
	if err != nil {
		t.Fatal(err)
	}
	req.EphPub[0] ^= 0xFF
	if _, err := verifyRequest(desktopPub, marshalRequest(req)); !errors.Is(err, ErrBadRequestSignature) {
		t.Fatalf("tampered request: got %v, want ErrBadRequestSignature", err)
	}
}

func TestCodecRoundTrips(t *testing.T) {
	t.Parallel()
	_, priv := desktopKeys(t)
	wrapped, ephPub, nonce := randBytes(t, 100), randBytes(t, 32), randBytes(t, nonceLen)

	reqBytes, err := signRequest(priv, wrapped, ephPub, nonce)
	if err != nil {
		t.Fatal(err)
	}
	req, err := unmarshalRequest(reqBytes)
	if err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if !bytes.Equal(req.Wrapped, wrapped) || !bytes.Equal(req.EphPub, ephPub) || !bytes.Equal(req.Nonce, nonce) {
		t.Fatal("request round-trip changed a field")
	}

	_, sign, _ := ed25519.GenerateKey(rand.Reader)
	respBytes, err := signResponse(sign, nonce, wrapped)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := unmarshalResponse(respBytes)
	if err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !bytes.Equal(resp.Nonce, nonce) || !bytes.Equal(resp.Sealed, wrapped) {
		t.Fatal("response round-trip changed a field")
	}

	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"empty", nil},
		{"bad version", append([]byte{0x99}, reqBytes[1:]...)},
		{"truncated", reqBytes[:5]},
		{"trailing", append(append([]byte(nil), reqBytes...), 0x00)},
	} {
		if _, err := unmarshalRequest(tc.raw); err == nil {
			t.Errorf("%s: want a parse error", tc.name)
		}
	}
	// An oversized declared length is refused, not allocated.
	big := []byte{wireVersion, 0xFF, 0xFF, 0xFF, 0xFF}
	if _, err := unmarshalRequest(big); !errors.Is(err, ErrMalformed) {
		t.Errorf("oversized field: got %v, want ErrMalformed", err)
	}
}

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return b
}
