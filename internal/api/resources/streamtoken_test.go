package resources

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/auth"
)

// The token is the plan: what it names is what the route does, and only the
// credential it was minted for can present it.
func TestStreamTokenRoundTripsAndBinds(t *testing.T) {
	key := streamKey([]byte("secret"))
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	tok := streamToken{
		BlobHash: "blake3:abc", Subject: "p1/token:t1",
		CopyVideo: true, CopyAudio: false, MaxHeight: 1080,
		ExpiresAt: now.Add(streamTokenTTL),
	}
	signed, err := tok.sign(key)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(signed, "/+=") {
		t.Errorf("token %q is not URL-safe", signed)
	}

	got, err := verifyStreamToken(key, signed, "p1/token:t1", now.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got != tok {
		t.Errorf("verified = %+v, want %+v", got, tok)
	}

	cases := []struct {
		name    string
		token   string
		subject string
		at      time.Time
		key     []byte
		want    error
	}{
		{"another credential", signed, "p1/token:t2", now, key, errStreamTokenSubject},
		{"a device where a token planned", signed, "p1/device:k", now, key, errStreamTokenSubject},
		{"expired", signed, "p1/token:t1", now.Add(streamTokenTTL), key, errStreamTokenExpired},
		{"another node's key", signed, "p1/token:t1", now, streamKey([]byte("other")), errStreamTokenSignature},
		{"the render key, not the stream key", signed, "p1/token:t1", now, []byte("secret"), errStreamTokenSignature},
		{"a flipped flag", flip(signed, "."+"1"+".", "."+"3"+"."), "p1/token:t1", now, key, errStreamTokenSignature},
		{"a longer life", flip(signed, "1780", "1790"), "p1/token:t1", now, key, errStreamTokenSignature},
		{"no signature", strings.SplitN(signed, ".", 2)[0], "p1/token:t1", now, key, errStreamTokenMalformed},
		{"garbage", "not.a.token", "p1/token:t1", now, key, errStreamTokenMalformed},
		{"empty", "", "p1/token:t1", now, key, errStreamTokenMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := verifyStreamToken(tc.key, tc.token, tc.subject, tc.at)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// flip substitutes inside the signed payload so the signature no longer
// matches. The first argument that does not occur leaves the token intact,
// which the test would then report as "verified" — so it asserts occurrence.
func flip(token, from, to string) string {
	if !strings.Contains(token, from) {
		return token + "x" // still tampered, still must fail
	}
	return strings.Replace(token, from, to, 1)
}

// The subject is stable per credential and distinct across them — including
// a device credential, which has no token id.
func TestStreamSubjectNamesTheCredential(t *testing.T) {
	bearer := auth.Identity{Principal: auth.Principal{ID: "p1"}, Token: auth.Token{ID: "t1", Name: "phone"}}
	bearer2 := auth.Identity{Principal: auth.Principal{ID: "p1"}, Token: auth.Token{ID: "t2", Name: "phone"}}
	device := auth.Identity{Principal: auth.Principal{ID: "p1"}, Token: auth.Token{Name: "device:k1"}}
	device2 := auth.Identity{Principal: auth.Principal{ID: "p1"}, Token: auth.Token{Name: "device:k2"}}
	anon := auth.Identity{Anonymous: true, Token: auth.Token{Name: "anonymous"}}

	again := auth.Identity{Principal: auth.Principal{ID: "p1"}, Token: auth.Token{ID: "t1", Name: "renamed"}}
	if streamSubject(bearer) != streamSubject(again) {
		t.Error("the same credential named twice differs")
	}
	seen := map[string]string{}
	for name, id := range map[string]auth.Identity{"bearer": bearer, "bearer2": bearer2, "device": device, "device2": device2, "anon": anon} {
		s := streamSubject(id)
		if s == "" {
			t.Errorf("%s: empty subject", name)
		}
		if other, dup := seen[s]; dup {
			t.Errorf("%s and %s share the subject %q", name, other, s)
		}
		seen[s] = name
	}
}

// A node with no render secret still signs: a per-process secret, so the
// key is never empty and sign cannot be talked into an unsigned token.
func TestStreamSecretIsNeverEmpty(t *testing.T) {
	s, err := newStreamSecret()
	if err != nil || len(s) != 32 {
		t.Fatalf("secret = %x, %v", s, err)
	}
	if _, err := (streamToken{}).sign(nil); err == nil {
		t.Error("signing with no key succeeded")
	}
	if _, err := (streamToken{BlobHash: "b", ExpiresAt: time.Now()}).sign(streamKey(s)); err == nil {
		t.Error("signing with no subject succeeded — an unbound token is exactly what must not exist")
	}
}
