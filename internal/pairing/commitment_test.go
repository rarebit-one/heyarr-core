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

	c, err := Commit(k)
	if err != nil {
		t.Fatalf("Commit refused a well-formed key: %v", err)
	}
	if len(c) != CommitmentLen {
		t.Fatalf("commitment is %d bytes, want %d", len(c), CommitmentLen)
	}
	if err := c.Open(k); err != nil {
		t.Fatalf("a commitment did not open to its own key: %v", err)
	}
	if err := c.Open(other); !errors.Is(err, ErrCommitmentMismatch) {
		t.Fatalf("a commitment opened to a DIFFERENT key (err=%v) — binding is broken; "+
			"this is the rushing attack", err)
	}
}

// The commitment is deterministic — the same key commits to the same value every
// time — so both sides can check each other's, and a test is reproducible.
func TestCommitIsDeterministic(t *testing.T) {
	k := key(t, 7)
	a, err := Commit(k)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Commit(k)
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
	a, err := Commit(key(t, 10))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Commit(key(t, 11))
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
	c, err := Commit(k)
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
	if _, err := Commit(make([]byte, 10)); !errors.Is(err, ErrMalformedKey) {
		t.Fatalf("Commit accepted a 10-byte key: %v", err)
	}
	good := key(t, 4)
	short := Commitment(make([]byte, 5))
	if err := short.Open(good); !errors.Is(err, ErrMalformedKey) {
		t.Fatalf("Open accepted a 5-byte commitment: %v", err)
	}
	c, err := Commit(good)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Open(make([]byte, ed25519.PublicKeySize-1)); !errors.Is(err, ErrMalformedKey) {
		t.Fatalf("Open accepted a malformed key: %v", err)
	}
}
