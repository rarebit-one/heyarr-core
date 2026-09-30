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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestChangesSinceReturnsOnlyTheTail: the incremental pull is what makes a
// steady-state sync free. Pulling from a cursor returns what arrived after it,
// and pulling from the cursor it hands back returns nothing (§44).
func TestChangesSinceReturnsOnlyTheTail(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	sp, _ := s.CreateSpace(ctx, spaces.KindPersonal)

	for _, body := range [][]byte{[]byte("a"), []byte("b"), []byte("c")} {
		if err := s.PutChange(ctx, change(t, sp.ID, nil, body)); err != nil {
			t.Fatalf("PutChange(%s): %v", body, err)
		}
	}

	all, cursor, err := s.ChangesSince(ctx, sp.ID, 0)
	if err != nil {
		t.Fatalf("ChangesSince(0): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ChangesSince(0) returned %d changes, want 3", len(all))
	}
	if cursor == 0 {
		t.Fatal("ChangesSince(0) handed back a zero cursor after returning rows")
	}

	// The whole point: caught up means nothing on the wire.
	tail, next, err := s.ChangesSince(ctx, sp.ID, cursor)
	if err != nil {
		t.Fatalf("ChangesSince(cursor): %v", err)
	}
	if len(tail) != 0 {
		t.Fatalf("a caught-up pull returned %d changes, want 0", len(tail))
	}
	if next != cursor {
		t.Fatalf("an empty pull moved the cursor: %d -> %d", cursor, next)
	}

	// A new change arrives: only it comes back.
	fresh := change(t, sp.ID, nil, []byte("d"))
	if err := s.PutChange(ctx, fresh); err != nil {
		t.Fatalf("PutChange(d): %v", err)
	}
	got, after, err := s.ChangesSince(ctx, sp.ID, cursor)
	if err != nil {
		t.Fatalf("ChangesSince(cursor) after a new change: %v", err)
	}
	if len(got) != 1 || got[0].ChangeID != fresh.ChangeID {
		t.Fatalf("incremental pull = %d changes, want exactly the new one", len(got))
	}
	if after <= cursor {
		t.Fatalf("cursor did not advance: %d -> %d", cursor, after)
	}
}

// TestChangesSinceIsNotRewoundByARedelivery: a relay re-sending a change a
// device has already passed must not drag it back before that device's cursor,
// or the device would re-download the log forever.
func TestChangesSinceIsNotRewoundByARedelivery(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	sp, _ := s.CreateSpace(ctx, spaces.KindPersonal)

	first := change(t, sp.ID, nil, []byte("first"))
	if err := s.PutChange(ctx, first); err != nil {
		t.Fatalf("PutChange: %v", err)
	}
	if err := s.PutChange(ctx, change(t, sp.ID, nil, []byte("second"))); err != nil {
		t.Fatalf("PutChange: %v", err)
	}
	_, cursor, err := s.ChangesSince(ctx, sp.ID, 0)
	if err != nil {
		t.Fatalf("ChangesSince(0): %v", err)
	}

	// Re-deliver the FIRST change. It keeps its original seq, so a caught-up
	// device still sees nothing.
	if err := s.PutChange(ctx, first); err != nil {
		t.Fatalf("re-PutChange: %v", err)
	}
	got, _, err := s.ChangesSince(ctx, sp.ID, cursor)
	if err != nil {
		t.Fatalf("ChangesSince after re-delivery: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a re-delivered change rewound the cursor: %d changes came back", len(got))
	}
}

// TestChangesSinceAgreesWithChangesFor: the incremental path and the full path
// must return the same log in the same order, or a device that switches between
// them diverges.
func TestChangesSinceAgreesWithChangesFor(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	sp, _ := s.CreateSpace(ctx, spaces.KindFamily)
	for i := 0; i < 5; i++ {
		if err := s.PutChange(ctx, change(t, sp.ID, nil, []byte{byte('a' + i)})); err != nil {
			t.Fatalf("PutChange %d: %v", i, err)
		}
	}
	full, err := s.ChangesFor(ctx, sp.ID)
	if err != nil {
		t.Fatalf("ChangesFor: %v", err)
	}
	inc, _, err := s.ChangesSince(ctx, sp.ID, 0)
	if err != nil {
		t.Fatalf("ChangesSince(0): %v", err)
	}
	if len(full) != len(inc) {
		t.Fatalf("ChangesFor = %d changes, ChangesSince(0) = %d", len(full), len(inc))
	}
	for i := range full {
		if full[i].ChangeID != inc[i].ChangeID {
			t.Fatalf("order diverged at %d: %s vs %s", i, full[i].ChangeID, inc[i].ChangeID)
		}
	}
}

// TestChangesSinceUnknownSpace: an unknown space is an error, not an empty tail
// that would read to a device as "you are up to date".
func TestChangesSinceUnknownSpace(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	if _, _, err := s.ChangesSince(context.Background(), "no-such-space", 0); !errors.Is(err, store.ErrUnknownSpace) {
		t.Fatalf("ChangesSince(unknown space) = %v, want ErrUnknownSpace", err)
	}
}
