package render

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var (
	testSecret  = []byte("0123456789abcdef0123456789abcdef")
	otherSecret = []byte("fedcba9876543210fedcba9876543210")
	testNow     = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
)

func TestCapabilityRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		capability Capability
	}{
		{
			name: "an ordinary video",
			capability: Capability{
				BlobHash:  "blake3:" + strings.Repeat("a", 64),
				ExpiresAt: testNow.Add(time.Hour),
				MIME:      "video/mp4",
			},
		},
		{
			// The case that made the encoding load-bearing: a Samsung declares
			// audio/vnd.dolby.dd-raw, and the token's fields are dot-separated.
			name: "a media type containing dots",
			capability: Capability{
				BlobHash:  "blake3:" + strings.Repeat("b", 64),
				ExpiresAt: testNow.Add(time.Hour),
				MIME:      "audio/vnd.dolby.dd-raw",
			},
		},
		{
			name: "no media type at all",
			capability: Capability{
				BlobHash:  "blake3:" + strings.Repeat("c", 64),
				ExpiresAt: testNow.Add(time.Hour),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			token, err := tc.capability.Sign(testSecret)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			got, err := Verify(testSecret, token, testNow)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if got.BlobHash != tc.capability.BlobHash {
				t.Errorf("BlobHash = %q, want %q", got.BlobHash, tc.capability.BlobHash)
			}
			if got.MIME != tc.capability.MIME {
				t.Errorf("MIME = %q, want %q", got.MIME, tc.capability.MIME)
			}
			if !got.ExpiresAt.Equal(tc.capability.ExpiresAt) {
				t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, tc.capability.ExpiresAt)
			}
		})
	}
}

// TestCapabilityRejects is the security surface of ADR-0039 in one table. Each
// row is a way somebody could try to turn a capability for one blob into a
// capability for something else.
func TestCapabilityRejects(t *testing.T) {
	t.Parallel()

	valid := Capability{
		BlobHash:  "blake3:" + strings.Repeat("a", 64),
		ExpiresAt: testNow.Add(time.Hour),
		MIME:      "video/mp4",
	}
	token, err := valid.Sign(testSecret)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	fields := strings.Split(token, ".")

	// A capability for a DIFFERENT blob, signed correctly with the same
	// secret, then grafted onto this token's signature.
	other, err := Capability{
		BlobHash: "blake3:" + strings.Repeat("d", 64), ExpiresAt: valid.ExpiresAt, MIME: valid.MIME,
	}.Sign(testSecret)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	swappedBlob := strings.Join(append(strings.Split(other, ".")[:4], fields[4]), ".")

	tests := []struct {
		name  string
		token string
		now   time.Time
		want  error
	}{
		{
			name:  "a blob swapped under a good signature",
			token: swappedBlob,
			now:   testNow,
			want:  ErrSignature,
		},
		{
			// The most valuable forgery: outlive the expiry.
			name:  "an expiry pushed into the future",
			token: strings.Join([]string{fields[0], fields[1], "99999999999", fields[3], fields[4]}, "."),
			now:   testNow,
			want:  ErrSignature,
		},
		{
			// Relabelling the type is how you would try to make a renderer
			// accept bytes it would otherwise refuse.
			name:  "a media type rewritten",
			token: strings.Join([]string{fields[0], fields[1], fields[2], "dmlkZW8vd2VibQ", fields[4]}, "."),
			now:   testNow,
			want:  ErrSignature,
		},
		{
			// ADR-0039 scopes a capability to the peer that minted it, so this
			// is a real operational case and not only an attack.
			name:  "a token minted by another peer",
			token: mustSign(t, valid, otherSecret),
			now:   testNow,
			want:  ErrSignature,
		},
		{
			name:  "a truncated signature",
			token: token[:len(token)-4],
			now:   testNow,
			want:  ErrSignature,
		},
		{
			name:  "expired by one second",
			token: token,
			now:   valid.ExpiresAt.Add(time.Second),
			want:  ErrExpired,
		},
		{
			// The boundary: a capability is dead AT its expiry, not after it.
			name:  "expired exactly at the instant",
			token: token,
			now:   valid.ExpiresAt,
			want:  ErrExpired,
		},
		{name: "not a token at all", token: "hello", now: testNow, want: ErrMalformed},
		{name: "empty", token: "", now: testNow, want: ErrMalformed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Verify(testSecret, tc.token, tc.now); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestCapabilityIsValidUpToItsExpiry pins the other side of the boundary.
func TestCapabilityIsValidUpToItsExpiry(t *testing.T) {
	t.Parallel()

	granted := Capability{BlobHash: "blake3:" + strings.Repeat("a", 64), ExpiresAt: testNow.Add(time.Hour)}
	token := mustSign(t, granted, testSecret)
	if _, err := Verify(testSecret, token, granted.ExpiresAt.Add(-time.Second)); err != nil {
		t.Fatalf("a capability one second before expiry must verify: %v", err)
	}
}

func TestSignRefusesAnIncompleteCapability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		capability Capability
	}{
		{name: "no blob", capability: Capability{ExpiresAt: testNow}},
		{name: "no expiry", capability: Capability{BlobHash: "blake3:x"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := tc.capability.Sign(testSecret); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestNewSecretIsDistinct(t *testing.T) {
	t.Parallel()

	a, err := NewSecret()
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	b, err := NewSecret()
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	if len(a) != SecretLen {
		t.Errorf("len = %d, want %d", len(a), SecretLen)
	}
	if string(a) == string(b) {
		t.Error("two secrets are identical, which means this is not random")
	}
}

func mustSign(t *testing.T, c Capability, secret []byte) string {
	t.Helper()
	token, err := c.Sign(secret)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return token
}
