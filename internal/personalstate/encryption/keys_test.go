package encryption

import (
	"errors"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/recovery"
)

// TestFormatParseRoundTrip: a generated key renders "x25519:<hex>" and parses
// back to the same bytes. The identity of an encryption key is its bytes, so the
// round trip must be lossless.
func TestFormatParseRoundTrip(t *testing.T) {
	for i := 0; i < 100; i++ {
		priv, err := GenerateKey()
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		enc := FormatPublicKey(priv.PublicKey().Bytes())
		if !strings.HasPrefix(enc, Algorithm+":") {
			t.Fatalf("rendering %q lacks the %q prefix", enc, Algorithm)
		}
		back, err := ParsePublicKey(enc)
		if err != nil {
			t.Fatalf("ParsePublicKey(%q): %v", enc, err)
		}
		if !back.Equal(priv.PublicKey()) {
			t.Fatalf("round trip changed the key for %q", enc)
		}
	}
}

// TestParseRejectsMalformed: everything that is not a well-formed encryption key
// is refused with ErrMalformedPublicKey, never parsed into a usable value. The
// ed25519 case is the load-bearing one — a signing key must not be accepted where
// an encryption key is meant, and vice versa.
func TestParseRejectsMalformed(t *testing.T) {
	priv, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	valid := FormatPublicKey(priv.PublicKey().Bytes())

	// A signing key, rendered its own "ed25519:<hex>" way, is NOT an encryption
	// key — refused on the algorithm prefix before the bytes are ever read.
	edRendered := identity.FormatPublicKey(make([]byte, 32))

	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"no prefix", strings.TrimPrefix(valid, Algorithm+":")},
		{"wrong algorithm (ed25519)", edRendered},
		{"uppercase hex", Algorithm + ":" + strings.ToUpper(strings.TrimPrefix(valid, Algorithm+":"))},
		{"not hex", Algorithm + ":zz"},
		{"too short", Algorithm + ":00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParsePublicKey(tt.in); !errors.Is(err, ErrMalformedPublicKey) {
				t.Fatalf("ParsePublicKey(%q) = %v, want ErrMalformedPublicKey", tt.in, err)
			}
		})
	}
}

// TestEmptyRendersEmpty: a zero key renders "" rather than a bare "x25519:", so
// it cannot masquerade as a real key in a record or a cert.
func TestEmptyRendersEmpty(t *testing.T) {
	if got := FormatPublicKey(nil); got != "" {
		t.Fatalf("FormatPublicKey(nil) = %q, want empty", got)
	}
	if got := FormatPublicKey([]byte{}); got != "" {
		t.Fatalf("FormatPublicKey(empty) = %q, want empty", got)
	}
}

// TestNewPrivateKeyFromRecoverySeed: the seed recovery derives builds a usable
// key through this package's own constructor, and it agrees with a counterpart —
// the end-to-end path a recovery unwrap will take (ADR-0049, ADR-0022).
func TestNewPrivateKeyFromRecoverySeed(t *testing.T) {
	secret, err := recovery.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	seed := recovery.DeriveUserEncryptionSeed(secret)

	priv, err := NewPrivateKey(seed)
	if err != nil {
		t.Fatalf("NewPrivateKey(recovery seed): %v", err)
	}

	// Deterministic and usable: same secret, same key, and it does ECDH.
	priv2, err := NewPrivateKey(recovery.DeriveUserEncryptionSeed(secret))
	if err != nil {
		t.Fatalf("NewPrivateKey (second): %v", err)
	}
	if !priv.PublicKey().Equal(priv2.PublicKey()) {
		t.Fatal("the recovery encryption key is not deterministic")
	}

	other, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	a, err := priv.ECDH(other.PublicKey())
	if err != nil {
		t.Fatalf("ECDH: %v", err)
	}
	b, err := other.ECDH(priv.PublicKey())
	if err != nil {
		t.Fatalf("ECDH: %v", err)
	}
	if string(a) != string(b) {
		t.Fatal("the recovery key did not agree with a counterpart")
	}
}

// TestNewPrivateKeyRejectsWrongLength: a seed that is not 32 bytes is refused,
// not silently truncated or padded into a different key.
func TestNewPrivateKeyRejectsWrongLength(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		if _, err := NewPrivateKey(make([]byte, n)); !errors.Is(err, ErrMalformedPublicKey) {
			t.Fatalf("NewPrivateKey(%d bytes) = %v, want ErrMalformedPublicKey", n, err)
		}
	}
}
