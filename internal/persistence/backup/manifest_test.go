package backup

import (
	"crypto/ed25519"
	"errors"
	"testing"
	"time"
)

func sampleCore() Core {
	return Core{
		SourcePeerID:  "peer-a",
		Generation:    42,
		SchemaVersion: 32,
		TakenAt:       time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
		Digest:        "blake3:" + "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
		SizeBytes:     4096,
		Omissions:     []string{OmitProviderCredentials},
	}
}

// TestSigningBytesAreStable pins the exact bytes a signature covers, so a Go
// upgrade or a field reorder cannot silently invalidate every backup ever
// signed.
func TestSigningBytesAreStable(t *testing.T) {
	got, err := sampleCore().signingBytes()
	if err != nil {
		t.Fatalf("signingBytes: %v", err)
	}
	const want = `{"source_peer_id":"peer-a","generation":42,"schema_version":32,` +
		`"taken_at":"2026-08-25T12:00:00Z","digest":"blake3:00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",` +
		`"size_bytes":4096,"omissions":["provider-credentials"]}`
	if string(got) != want {
		t.Errorf("signing bytes changed:\n got: %s\nwant: %s", got, want)
	}
}

// TestVerifyDistinguishesUnsignedFromInvalid proves the two facts are different
// errors: a caller that required a signature must not have "there was none" pass
// as "it verified", nor read as "it was forged".
func TestVerifyDistinguishesUnsignedFromInvalid(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	core := sampleCore()

	unsigned := Manifest{Core: core}
	if err := unsigned.Verify(pub); !errors.Is(err, ErrUnsigned) {
		t.Errorf("unsigned manifest: got %v, want ErrUnsigned", err)
	}

	signed, err := core.sign(priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if err := signed.Verify(pub); err != nil {
		t.Errorf("a validly signed manifest did not verify: %v", err)
	}

	// A different key rejects it as invalid, not unsigned.
	otherPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	if err := signed.Verify(otherPub); !errors.Is(err, ErrSignatureInvalid) {
		t.Errorf("wrong key: got %v, want ErrSignatureInvalid", err)
	}

	// A mutated field after signing fails: the signature covers Core.
	tampered := signed
	tampered.Generation = 43
	if err := tampered.Verify(pub); !errors.Is(err, ErrSignatureInvalid) {
		t.Errorf("mutated manifest: got %v, want ErrSignatureInvalid", err)
	}
}

// TestValidateRejectsZeroGeneration pins that generation zero is not a spellable
// backup — absent is the absence of one (catalog.Meta's rule).
func TestValidateRejectsZeroGeneration(t *testing.T) {
	m := Manifest{Core: sampleCore()}
	m.Generation = 0
	if err := m.Validate(); err == nil {
		t.Error("a manifest at generation zero validated")
	}
}
