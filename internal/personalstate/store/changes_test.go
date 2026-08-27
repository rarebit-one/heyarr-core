package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
)

// change builds a valid EncryptedChange for a space, chained on the given parents.
func change(t *testing.T, spaceID string, parents []string, ct []byte) protocol.EncryptedChange {
	t.Helper()
	c, err := protocol.NewChange(spaceID, parents, ct)
	if err != nil {
		t.Fatalf("NewChange: %v", err)
	}
	return c
}

func TestPutAndFetchChanges(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	sp, _ := s.CreateSpace(ctx, spaces.KindPersonal)

	a := change(t, sp.ID, nil, []byte("c-a"))
	b := change(t, sp.ID, []string{a.ChangeID}, []byte("c-b"))
	for _, c := range []protocol.EncryptedChange{a, b} {
		if err := s.PutChange(ctx, c); err != nil {
			t.Fatalf("PutChange: %v", err)
		}
	}

	got, err := s.ChangesFor(ctx, sp.ID)
	if err != nil {
		t.Fatalf("ChangesFor: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d changes, want 2", len(got))
	}
	// The heads frontier is the tip.
	heads, err := s.HeadsFor(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(heads) != 1 || heads[0] != b.ChangeID {
		t.Fatalf("heads = %v, want [%s]", heads, b.ChangeID)
	}
}

// TestPutChangeVerifiesID: a change whose stated id does not match its bytes — a
// forgery a malicious peer might push — is refused, never stored. The store never
// trusts a claimed id (Invariant 1).
func TestPutChangeVerifiesID(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	sp, _ := s.CreateSpace(ctx, spaces.KindFamily)

	c := change(t, sp.ID, nil, []byte("honest"))
	c.Ciphertext = []byte("tampered") // bytes no longer match the id
	if err := s.PutChange(ctx, c); !errors.Is(err, protocol.ErrIDMismatch) {
		t.Fatalf("PutChange(forged id) = %v, want ErrIDMismatch", err)
	}
	if got, _ := s.ChangesFor(ctx, sp.ID); len(got) != 0 {
		t.Fatal("a forged change was stored")
	}
}

// TestPutChangeIsIdempotent: accepting the same change twice leaves one row — a
// re-sending relay cannot duplicate it (the id is the primary key).
func TestPutChangeIsIdempotent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	sp, _ := s.CreateSpace(ctx, spaces.KindResearch)
	c := change(t, sp.ID, nil, []byte("once"))

	for i := 0; i < 3; i++ {
		if err := s.PutChange(ctx, c); err != nil {
			t.Fatalf("PutChange %d: %v", i, err)
		}
	}
	if got, _ := s.ChangesFor(ctx, sp.ID); len(got) != 1 {
		t.Fatalf("a re-sent change duplicated: %d rows", len(got))
	}
}

func TestPutChangeRequiresSpace(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	c := change(t, "missing-space", nil, []byte("x"))
	if err := s.PutChange(ctx, c); !errors.Is(err, store.ErrUnknownSpace) {
		t.Fatalf("PutChange(unknown space) = %v, want ErrUnknownSpace", err)
	}
}

// TestStoredChangeIsCiphertext: a change stored and read back is the opaque
// ciphertext, byte-for-byte, and it is not the plaintext — the peer holds
// ciphertext it cannot read (§38).
func TestStoredChangeIsCiphertext(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	sp, _ := s.CreateSpace(ctx, spaces.KindPersonal)

	plaintext := []byte("a private annotation the peer must not see")
	// Stand-in ciphertext that simply must not equal the plaintext and must
	// round-trip opaquely (the real ciphertext comes from encryption.EncryptChange;
	// the store treats it as bytes either way).
	ct := append([]byte("enc:"), bytes.Repeat([]byte{0x5a}, 40)...)
	c := change(t, sp.ID, nil, ct)
	if err := s.PutChange(ctx, c); err != nil {
		t.Fatal(err)
	}
	got, err := s.ChangesFor(ctx, sp.ID)
	if err != nil || len(got) != 1 {
		t.Fatalf("ChangesFor: %v (%d)", err, len(got))
	}
	if !bytes.Equal(got[0].Ciphertext, ct) {
		t.Fatal("stored ciphertext did not round-trip opaquely")
	}
	if bytes.Contains(got[0].Ciphertext, plaintext) {
		t.Fatal("stored change contains the plaintext")
	}
	// And the read-back change still validates against its id.
	if err := got[0].Validate(); err != nil {
		t.Fatalf("a stored change no longer validates: %v", err)
	}
}
