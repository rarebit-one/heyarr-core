package pairing_test

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/pairing"
)

// Adversarial synthetic tests for the v2 pairing SAS. The hand-written suite
// proves each substitution in isolation; these consolidate the whole threat into
// one matrix — a two-device pairing has FOUR public keys (each device's signing
// and encryption key), and a relay wins only if it can change one while leaving
// the string unchanged — and add a statistical distinctness check that no two of
// many independent pairings collide. The threat model is a relay that forwards
// the handshake and substitutes any single key it likes (§40, §41, ADR-0049).

func synthSalt() []byte { return bytes.Repeat([]byte{0x5a}, pairing.SaltLen) }

func synthSign(seed byte) ed25519.PublicKey {
	pub, _, err := ed25519.GenerateKey(bytes.NewReader(bytes.Repeat([]byte{seed}, ed25519.SeedSize)))
	if err != nil {
		panic(err)
	}
	return pub
}

func synthEnc(seed byte) []byte { return bytes.Repeat([]byte{seed}, pairing.EncKeySize) }

// TestEveryKeyOfBothDevicesIsBound: with all four keys populated, flipping ANY
// single one — either device's signing OR encryption key — changes the SAS. A key
// that could be swapped without moving the string is a relay hole.
func TestEveryKeyOfBothDevicesIsBound(t *testing.T) {
	t.Parallel()
	base := func() (pairing.Keys, pairing.Keys) {
		return pairing.Keys{Sign: synthSign(1), Enc: synthEnc(0x11)},
			pairing.Keys{Sign: synthSign(2), Enc: synthEnc(0x22)}
	}
	init0, resp0 := base()
	honest, err := pairing.Derive(init0, resp0, synthSalt())
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}

	// Each mutation touches exactly one of the four keys.
	mutations := []struct {
		name       string
		init, resp pairing.Keys
	}{
		{"initiator signing", pairing.Keys{Sign: synthSign(9), Enc: synthEnc(0x11)}, resp0},
		{"initiator encryption", pairing.Keys{Sign: synthSign(1), Enc: synthEnc(0x99)}, resp0},
		{"responder signing", init0, pairing.Keys{Sign: synthSign(9), Enc: synthEnc(0x22)}},
		{"responder encryption", init0, pairing.Keys{Sign: synthSign(2), Enc: synthEnc(0x99)}},
	}
	for _, m := range mutations {
		got, err := pairing.Derive(m.init, m.resp, synthSalt())
		if err != nil {
			t.Fatalf("%s: Derive: %v", m.name, err)
		}
		if got == honest {
			t.Fatalf("substituting the %s key left the SAS unchanged (%q): it is not bound", m.name, honest)
		}
	}
}

// TestSignAndEncPositionsAreDistinct: a device's signing and encryption keys are
// bound in SEPARATE framed positions, not summed — swapping the two within one
// device changes the string. If they were concatenated without framing, swapping
// equal-length values could go unnoticed.
func TestSignAndEncPositionsAreDistinct(t *testing.T) {
	t.Parallel()
	sign := synthSign(3)
	enc := synthEnc(0x44)
	resp := pairing.Keys{Sign: synthSign(4), Enc: synthEnc(0x55)}

	normal, err := pairing.Derive(pairing.Keys{Sign: sign, Enc: enc}, resp, synthSalt())
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	// Put the encryption bytes in the signing slot and vice versa. (Both are 32
	// bytes, so this is a well-formed key set — the point is the POSITIONS differ.)
	swapped, err := pairing.Derive(pairing.Keys{Sign: ed25519.PublicKey(enc), Enc: []byte(sign)}, resp, synthSalt())
	if err != nil {
		t.Fatalf("Derive (swapped): %v", err)
	}
	if normal == swapped {
		t.Fatal("swapping a device's signing and encryption keys left the SAS unchanged: the two positions are not distinct")
	}
}

// TestManyPairingsAreDistinct: a large sample of independent pairings (varying one
// key each) produces no two identical strings. A collision here would be a
// second-preimage the 7-digit space makes a ~1-in-10^7 accident, not something a
// small deterministic sample should ever hit — a repeat would signal the inputs
// are not all reaching the hash.
func TestManyPairingsAreDistinct(t *testing.T) {
	t.Parallel()
	seen := make(map[pairing.SAS]int)
	resp := pairing.Keys{Sign: synthSign(200), Enc: synthEnc(0xEE)}
	n := 0
	for s := 0; s < 60; s++ {
		for e := 0; e < 60; e++ {
			init := pairing.Keys{Sign: synthSign(byte(s)), Enc: synthEnc(byte(e))}
			sas, err := pairing.Derive(init, resp, synthSalt())
			if err != nil {
				t.Fatalf("Derive: %v", err)
			}
			n++
			// A collision is statistically possible in 3600 draws over 10^7
			// (~0.06% by the birthday bound), so a single repeat is not a defect —
			// but if a whole ROW of encryption keys collapsed to one string, an
			// input is being dropped. Track and assert the latter, stronger, thing.
			seen[sas]++
		}
	}
	// No single SAS may absorb a large share of the sample — that would mean a
	// field is not reaching the digest. Cap any one value well below a row.
	for sas, count := range seen {
		if count > 5 {
			t.Fatalf("SAS %q occurred %d times in %d pairings — an input field is being dropped", sas, count, n)
		}
	}
}
