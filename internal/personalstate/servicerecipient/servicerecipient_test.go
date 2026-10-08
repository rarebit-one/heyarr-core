package servicerecipient_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/servicerecipient"
)

// The fingerprint is a wire format a person compares across two machines: pin
// its bytes, so the executor's host and the registering device never disagree.
func TestFingerprintVector(t *testing.T) {
	t.Parallel()
	got, err := servicerecipient.Fingerprint("x25519:" + strings.Repeat("44", 32))
	if err != nil {
		t.Fatal(err)
	}
	const want = "GIES LOSM A63H 4ICY" // independently: python3 hashlib + base64.b32encode
	if got != want {
		t.Errorf("Fingerprint = %q, want %q", got, want)
	}
	if !servicerecipient.SameFingerprint("gies-losm a63h4icy", got) {
		t.Error("a fingerprint typed with other spacing and case did not compare equal")
	}
	if servicerecipient.SameFingerprint("", got) || servicerecipient.SameFingerprint("GIES LOSM A63H 4ICZ", got) {
		t.Error("an empty or different fingerprint compared equal")
	}
}

func TestParseRefusesOtherSpellings(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"",
		strings.Repeat("44", 32),
		"x25519:" + strings.Repeat("4A", 32),
		"x25519:" + strings.Repeat("44", 31),
		"x25519:" + strings.Repeat("00", 32),
		"ed25519:" + strings.Repeat("44", 32),
		"x25519:" + strings.Repeat("zz", 32),
	} {
		if _, err := servicerecipient.Parse(in); !errors.Is(err, servicerecipient.ErrMalformed) {
			t.Errorf("Parse(%q) = %v, want ErrMalformed", in, err)
		}
	}
}
