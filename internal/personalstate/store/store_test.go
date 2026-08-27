package store_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
)

type fixedClock struct{ t time.Time }

func (c *fixedClock) Now() time.Time { return c.t }

var now = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	clock := &fixedClock{t: now}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.New(store.Options{Writer: db.Writer(), Reader: db.Reader(), Events: log, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// device draws an X25519 device key and its rendered recipient id.
func device(t *testing.T) (*ecdh.PrivateKey, string) {
	t.Helper()
	k, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k, encryption.FormatPublicKey(k.PublicKey().Bytes())
}

func TestCreateSpaceAndList(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	sp, err := s.CreateSpace(ctx, spaces.KindPersonal)
	if err != nil {
		t.Fatalf("CreateSpace: %v", err)
	}
	if sp.ID == "" || sp.Kind != spaces.KindPersonal {
		t.Fatalf("unexpected space: %+v", sp)
	}
	got, err := s.Space(ctx, sp.ID)
	if err != nil || got.ID != sp.ID {
		t.Fatalf("Space round-trip failed: %v (%+v)", err, got)
	}
	list, err := s.ListSpaces(ctx)
	if err != nil || len(list) != 1 || list[0].ID != sp.ID {
		t.Fatalf("ListSpaces = %v, %v", list, err)
	}
}

func TestCreateSpaceRejectsUnknownKind(t *testing.T) {
	s := newStore(t)
	if _, err := s.CreateSpace(context.Background(), spaces.Kind("nope")); !errors.Is(err, spaces.ErrUnknownKind) {
		t.Fatalf("CreateSpace(bad kind) = %v, want ErrUnknownKind", err)
	}
}

func TestUnknownSpaceRefused(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.Space(ctx, "missing"); !errors.Is(err, store.ErrUnknownSpace) {
		t.Fatalf("Space(missing) = %v, want ErrUnknownSpace", err)
	}
	if _, err := s.PutWrappedKey(ctx, "missing", "x25519:aa", []byte{1}); !errors.Is(err, store.ErrUnknownSpace) {
		t.Fatalf("PutWrappedKey(missing space) = %v, want ErrUnknownSpace", err)
	}
}

func TestPutAndFetchWrappedKeys(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	sp, _ := s.CreateSpace(ctx, spaces.KindFamily)

	_, aID := device(t)
	_, bID := device(t)
	blobA, blobB := []byte("wrapped-for-a"), []byte("wrapped-for-b")
	if _, err := s.PutWrappedKey(ctx, sp.ID, aID, blobA); err != nil {
		t.Fatalf("PutWrappedKey a: %v", err)
	}
	if _, err := s.PutWrappedKey(ctx, sp.ID, bID, blobB); err != nil {
		t.Fatalf("PutWrappedKey b: %v", err)
	}

	keys, err := s.WrappedKeysFor(ctx, sp.ID)
	if err != nil {
		t.Fatalf("WrappedKeysFor: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("got %d wrapped keys, want 2", len(keys))
	}
	byRecipient := map[string][]byte{}
	for _, k := range keys {
		byRecipient[k.Recipient] = k.Wrapped
	}
	if !bytes.Equal(byRecipient[aID], blobA) || !bytes.Equal(byRecipient[bID], blobB) {
		t.Fatal("wrapped bytes did not round-trip opaquely through the store")
	}
}

// TestStoredWrappedKeyOpensOnlyForItsTarget is the store's contribution to the
// #28 evidence: a peer holds the wrapped key as OPAQUE bytes it cannot read, the
// intended device unwraps the STORED copy to the space key (proven by decrypting
// a change), and a THIRD device — standing in for any other peer or party — gets
// nothing. The store itself has no unwrap path; this drives Seal/Unwrap around it.
func TestStoredWrappedKeyOpensOnlyForItsTarget(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	sp, _ := s.CreateSpace(ctx, spaces.KindPersonal)

	targetPriv, targetID := device(t)
	strangerPriv, _ := device(t)

	// The client seals the space key for the target and hands the peer the opaque
	// bytes; the peer stores them, holding no key of its own.
	spaceKey, err := encryption.NewSpaceKey()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := encryption.Seal(spaceKey, targetPriv.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutWrappedKey(ctx, sp.ID, targetID, wrapped); err != nil {
		t.Fatalf("PutWrappedKey: %v", err)
	}

	// A change encrypted under the space key, to prove the recovered key is the
	// real one and that the ciphertext is not its plaintext.
	plaintext := []byte("the playlist is named after a place")
	change, err := encryption.EncryptChange(spaceKey, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(change, plaintext) {
		t.Fatal("the change ciphertext contains its plaintext")
	}

	// Read the STORED wrapped key back and have the target unwrap it.
	keys, err := s.WrappedKeysFor(ctx, sp.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("WrappedKeysFor: %v (%d)", err, len(keys))
	}
	storedWrap := keys[0].Wrapped

	recovered, err := encryption.Unwrap(storedWrap, targetPriv)
	if err != nil {
		t.Fatalf("the target could not unwrap its stored key: %v", err)
	}
	got, err := encryption.DecryptChange(recovered, change)
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("the recovered key did not decrypt the change: %v", err)
	}

	// A third party (any other peer/device) cannot unwrap the stored copy.
	if _, err := encryption.Unwrap(storedWrap, strangerPriv); !errors.Is(err, encryption.ErrUnwrap) {
		t.Fatal("a non-target unwrapped the stored key — the peer's storage is not opaque")
	}
}

// TestReWrapReplacesInPlace: storing a new wrapped copy for a recipient replaces
// the old one — a recipient has exactly one current copy, which is how a re-wrap
// after revocation (§41) lands.
func TestReWrapReplacesInPlace(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	sp, _ := s.CreateSpace(ctx, spaces.KindShared)
	_, rID := device(t)

	if _, err := s.PutWrappedKey(ctx, sp.ID, rID, []byte("old")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutWrappedKey(ctx, sp.ID, rID, []byte("new")); err != nil {
		t.Fatal(err)
	}
	keys, err := s.WrappedKeysFor(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("re-wrap left %d copies, want 1", len(keys))
	}
	if !bytes.Equal(keys[0].Wrapped, []byte("new")) {
		t.Fatalf("re-wrap did not replace the bytes: %q", keys[0].Wrapped)
	}
}

func TestEmptyInputsRefused(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	sp, _ := s.CreateSpace(ctx, spaces.KindResearch)
	if _, err := s.PutWrappedKey(ctx, sp.ID, "", []byte{1}); !errors.Is(err, store.ErrEmptyRecipient) {
		t.Fatalf("empty recipient = %v, want ErrEmptyRecipient", err)
	}
	if _, err := s.PutWrappedKey(ctx, sp.ID, "x25519:aa", nil); !errors.Is(err, store.ErrEmptyWrapped) {
		t.Fatalf("empty wrapped = %v, want ErrEmptyWrapped", err)
	}
}

// TestPutSpace records a client-minted space, is idempotent on a re-push, and
// refuses a re-push under a different kind or a non-uuid id.
func TestPutSpace(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	sp, err := spaces.NewSpace(spaces.KindFamily, now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.PutSpace(ctx, sp.ID, sp.Kind)
	if err != nil {
		t.Fatalf("PutSpace: %v", err)
	}
	if got.ID != sp.ID || got.Kind != spaces.KindFamily {
		t.Fatalf("unexpected space: %+v", got)
	}
	back, err := s.Space(ctx, sp.ID)
	if err != nil || back.ID != sp.ID {
		t.Fatalf("Space after PutSpace: %+v %v", back, err)
	}

	// Idempotent: a second push of the same id+kind is a no-op success.
	if _, err := s.PutSpace(ctx, sp.ID, spaces.KindFamily); err != nil {
		t.Fatalf("idempotent re-push: %v", err)
	}

	// A different kind under the same id is a conflict, not an overwrite.
	if _, err := s.PutSpace(ctx, sp.ID, spaces.KindPersonal); err == nil {
		t.Fatal("re-recording a space under a different kind should be refused")
	}

	// A non-uuid id is refused.
	if _, err := s.PutSpace(ctx, "not-a-uuid", spaces.KindPersonal); err == nil {
		t.Fatal("a non-uuid space id should be refused")
	}

	// An unknown kind is refused (spaces.ErrUnknownKind).
	if _, err := s.PutSpace(ctx, mustUUIDStr(t), spaces.Kind("bogus")); !errors.Is(err, spaces.ErrUnknownKind) {
		t.Fatalf("unknown kind = %v, want ErrUnknownKind", err)
	}
}

func mustUUIDStr(t *testing.T) string {
	t.Helper()
	sp, err := spaces.NewSpace(spaces.KindPersonal, now)
	if err != nil {
		t.Fatal(err)
	}
	return sp.ID
}
