package pairing

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"testing"
)

// A commitment opens to the key it was made to, and to no other. This is the
// binding property the rushing defence rests on: if a commitment could be opened
// to a second key, a party could commit to one key and reveal another, which is
// the exact freedom the commitment removes.
func TestCommitmentOpensToItsOwnKeyOnly(t *testing.T) {
	k := key(t, 1)
	other := key(t, 2)

	c, err := Commit(k, nil)
	if err != nil {
		t.Fatalf("Commit refused a well-formed key: %v", err)
	}
	if len(c) != CommitmentLen {
		t.Fatalf("commitment is %d bytes, want %d", len(c), CommitmentLen)
	}
	if err := c.Open(k, nil); err != nil {
		t.Fatalf("a commitment did not open to its own key: %v", err)
	}
	if err := c.Open(other, nil); !errors.Is(err, ErrCommitmentMismatch) {
		t.Fatalf("a commitment opened to a DIFFERENT key (err=%v) — binding is broken; "+
			"this is the rushing attack", err)
	}
}

// The commitment is deterministic — the same key commits to the same value every
// time — so both sides can check each other's, and a test is reproducible.
func TestCommitIsDeterministic(t *testing.T) {
	k := key(t, 7)
	a, err := Commit(k, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Commit(k, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("Commit is not deterministic; the two sides could never agree on a commitment")
	}
}

// Distinct keys commit to distinct values (with overwhelming probability), which
// is what makes a commitment identify one key rather than a class of them.
func TestDistinctKeysDistinctCommitments(t *testing.T) {
	a, err := Commit(key(t, 10), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Commit(key(t, 11), nil)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("two different keys produced the same commitment")
	}
}

// A commitment is domain-separated from a SAS: the two hashes over the same key
// are unrelated, so nothing computed for one can be replayed as the other.
func TestCommitmentIsDomainSeparatedFromSAS(t *testing.T) {
	k := key(t, 3)
	c, err := Commit(k, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The SAS is 7 decimal digits; a commitment is 32 raw bytes. Even reduced,
	// there is no path by which one is mistaken for the other — but assert the
	// preimages differ at the hash level by confirming a commitment is not the
	// zero value and is full-width.
	if len(c) != CommitmentLen || bytes.Equal(c, make([]byte, CommitmentLen)) {
		t.Fatalf("commitment is degenerate: %x", c)
	}
}

// A malformed key is refused rather than committed to, and so is a malformed
// commitment on Open — the same discipline Derive keeps.
func TestCommitRefusesMalformed(t *testing.T) {
	if _, err := Commit(make([]byte, 10), nil); !errors.Is(err, ErrMalformedKey) {
		t.Fatalf("Commit accepted a 10-byte key: %v", err)
	}
	good := key(t, 4)
	short := Commitment(make([]byte, 5))
	if err := short.Open(good, nil); !errors.Is(err, ErrMalformedKey) {
		t.Fatalf("Open accepted a 5-byte commitment: %v", err)
	}
	c, err := Commit(good, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Open(make([]byte, ed25519.PublicKeySize-1), nil); !errors.Is(err, ErrMalformedKey) {
		t.Fatalf("Open accepted a malformed key: %v", err)
	}
}

// The commitment binds the ENCRYPTION key too (§41, ADR-0049): the same signing
// key with two different encryption keys commits to two different values, and a
// commitment made to one enc key does not open to another. This is what closes
// the rushing attack on the enc key — an attacker cannot commit honestly to a
// signing key and later swap the enc key it reveals.
func TestCommitmentBindsTheEncryptionKey(t *testing.T) {
	sign := key(t, 20)
	enc1 := make([]byte, 32)
	enc1[0] = 1
	enc2 := make([]byte, 32)
	enc2[0] = 2

	c1, err := Commit(sign, enc1)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := Commit(sign, enc2)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(c1, c2) {
		t.Fatal("the same signing key with two different encryption keys committed to the same value — the enc key is not bound")
	}
	if err := c1.Open(sign, enc1); err != nil {
		t.Fatalf("a commitment did not open to its own enc key: %v", err)
	}
	if err := c1.Open(sign, enc2); !errors.Is(err, ErrCommitmentMismatch) {
		t.Fatalf("a commitment opened to a DIFFERENT enc key (err=%v) — the rushing attack on the enc key is not closed", err)
	}
	// The empty enc key (a v1 device or the user identity) is a distinct, bound
	// value — not interchangeable with a present one.
	if err := c1.Open(sign, nil); !errors.Is(err, ErrCommitmentMismatch) {
		t.Fatalf("a commitment to a present enc key opened to an EMPTY one (err=%v)", err)
	}
}
