package enrolment

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPossessionRoundTrip(t *testing.T) {
	t.Parallel()
	_, devicePriv, _ := ed25519.GenerateKey(nil)
	devicePub := devicePriv.Public().(ed25519.PublicKey)
	cert := "a-cert-token"

	proof, err := SignPossession(devicePriv, cert, testNow, 0)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	if err := VerifyPossession(proof, devicePub, cert, testNow); err != nil {
		t.Fatalf("a valid proof was refused: %v", err)
	}
}

// A proof signed by a different key does not prove possession of this device —
// the whole point.
func TestPossessionFromAnotherKeyFails(t *testing.T) {
	t.Parallel()
	_, devicePriv, _ := ed25519.GenerateKey(nil)
	_, impostor, _ := ed25519.GenerateKey(nil)
	devicePub := devicePriv.Public().(ed25519.PublicKey)
	cert := "a-cert-token"

	proof, err := SignPossession(impostor, cert, testNow, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPossession(proof, devicePub, cert, testNow); !errors.Is(err, ErrPossessionSignature) {
		t.Fatalf("want ErrPossessionSignature, got %v", err)
	}
}

// A proof made for one cert cannot be lifted onto another presentation.
func TestPossessionBoundToItsCert(t *testing.T) {
	t.Parallel()
	_, devicePriv, _ := ed25519.GenerateKey(nil)
	devicePub := devicePriv.Public().(ed25519.PublicKey)

	proof, err := SignPossession(devicePriv, "cert-one", testNow, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPossession(proof, devicePub, "cert-two", testNow); !errors.Is(err, ErrPossessionCert) {
		t.Fatalf("want ErrPossessionCert for a mismatched cert, got %v", err)
	}
	// And it still verifies against its own cert.
	if err := VerifyPossession(proof, devicePub, "cert-one", testNow); err != nil {
		t.Fatalf("the proof should verify against its own cert, got %v", err)
	}
}

// Expiry is refused by the verifier's own clock, and skew only shortens the
// window (fails toward refusal).
func TestPossessionExpiryAndSkew(t *testing.T) {
	t.Parallel()
	_, devicePriv, _ := ed25519.GenerateKey(nil)
	devicePub := devicePriv.Public().(ed25519.PublicKey)
	cert := "a-cert-token"
	proof, err := SignPossession(devicePriv, cert, testNow, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Expiry is strict: at true expiry, refused.
	if err := VerifyPossession(proof, devicePub, cert, testNow.Add(time.Hour)); !errors.Is(err, ErrPossessionExpired) {
		t.Fatalf("at true expiry want expired, got %v", err)
	}
	// One second before expiry: honoured (no margin shortens the window).
	if err := VerifyPossession(proof, devicePub, cert, testNow.Add(time.Hour-time.Second)); err != nil {
		t.Fatalf("just before expiry want honoured, got %v", err)
	}
	// A default-TTL proof is honoured well inside its window — the bug this
	// guards against was a skew margin larger than the TTL, which made the
	// honoured window empty.
	shortProof, err := SignPossession(devicePriv, cert, testNow, 0) // PossessionTTL
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPossession(shortProof, devicePub, cert, testNow.Add(time.Minute)); err != nil {
		t.Fatalf("a default-TTL proof must be honoured within its window, got %v", err)
	}
	// A device clock a little ahead of the server is tolerated on the not-yet
	// side (within PossessionSkew).
	if err := VerifyPossession(proof, devicePub, cert, testNow.Add(-PossessionSkew/2)); err != nil {
		t.Fatalf("a small forward skew should be tolerated, got %v", err)
	}
	// But a proof issued well in the future is refused not_yet.
	if err := VerifyPossession(proof, devicePub, cert, testNow.Add(-2*PossessionSkew)); !errors.Is(err, ErrPossessionNotYet) {
		t.Fatalf("beyond the tolerance want not_yet, got %v", err)
	}
}

func TestPossessionMalformed(t *testing.T) {
	t.Parallel()
	_, devicePriv, _ := ed25519.GenerateKey(nil)
	devicePub := devicePriv.Public().(ed25519.PublicKey)
	for _, tok := range []string{"", "no-dot", "bad!.bad!", "."} {
		if err := VerifyPossession(tok, devicePub, "c", testNow); !errors.Is(err, ErrPossessionMalformed) && !errors.Is(err, ErrPossessionSignature) {
			t.Fatalf("token %q: want malformed/signature, got %v", tok, err)
		}
	}
}

// Tampering any byte of the signed body breaks the proof.
func TestPossessionTamperIsRefused(t *testing.T) {
	t.Parallel()
	_, devicePriv, _ := ed25519.GenerateKey(nil)
	devicePub := devicePriv.Public().(ed25519.PublicKey)
	cert := "a-cert-token"
	proof, err := SignPossession(devicePriv, cert, testNow, 0)
	if err != nil {
		t.Fatal(err)
	}
	body, sig, _ := strings.Cut(proof, ".")
	raw, err := decode(body)
	if err != nil {
		t.Fatal(err)
	}
	for i := range raw {
		mutated := append([]byte(nil), raw...)
		mutated[i] ^= 0x01
		if err := VerifyPossession(encode(mutated)+"."+sig, devicePub, cert, testNow); err == nil {
			t.Fatalf("byte %d: a tampered proof was honoured", i)
		}
	}
}

func TestSignPossessionRefusesEmptyInputs(t *testing.T) {
	t.Parallel()
	_, devicePriv, _ := ed25519.GenerateKey(nil)
	if _, err := SignPossession(nil, "c", testNow, 0); err == nil {
		t.Fatal("a nil device key should be refused")
	}
	if _, err := SignPossession(devicePriv, "", testNow, 0); !errors.Is(err, ErrPossessionMalformed) {
		t.Fatalf("an empty cert should be refused, got %v", err)
	}
}
