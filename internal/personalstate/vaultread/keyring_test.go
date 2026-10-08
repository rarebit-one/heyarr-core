package vaultread_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/encryption"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/vaultread"
)

// TestReadAcrossARotation is ADR-0103 for the vault: nothing is re-encrypted
// when a space rotates, so a file pushed before the rotation stays under the
// old key and one pushed after is under the new. A fresh reader holding the
// keyring (unrolled from the new key and the history row) reads both, whole and
// by range, and the single-key readers keep their old behaviour.
func TestReadAcrossARotation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	priv, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	me, err := client.ParseRecipient(encryption.FormatPublicKey(priv.PublicKey().Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	writer := client.New()
	sp, _, err := writer.Create(spaces.KindPersonal, time.Unix(0, 0).UTC(), []client.Recipient{me})
	if err != nil {
		t.Fatal(err)
	}
	f := newMemFetcher()
	k0, _ := writer.SpaceKey(sp.ID)
	before := pattern(3*1024 + 17)
	beforeID, _ := seedVault(t, f, k0, before)

	rot, err := writer.Rotate(sp.ID, []client.Recipient{me})
	if err != nil {
		t.Fatal(err)
	}
	k1, _ := writer.SpaceKey(sp.ID)
	after := bytes.Repeat([]byte("after the rotation "), 300)
	afterID, _ := seedVault(t, f, k1, after)

	reader := client.New()
	if err := reader.OpenWithHistory(sp.ID, rot.Wrapped[0].Wrapped, rot.Epoch,
		[]client.HistoryEntry{{Epoch: rot.Epoch, SealedPrev: rot.SealedPrev}}, client.NewKeyUnwrapper(priv)); err != nil {
		t.Fatal(err)
	}
	keys, ok := reader.Keys(sp.ID)
	if !ok || len(keys) != 2 {
		t.Fatalf("reader keyring = %d keys, want 2", len(keys))
	}

	for _, tc := range []struct {
		name string
		id   string
		want []byte
	}{{"pushed before", beforeID, before}, {"pushed after", afterID, after}} {
		got, err := vaultread.ReadAllWithKeys(ctx, f, keys, tc.id)
		if err != nil || !bytes.Equal(got, tc.want) {
			t.Fatalf("%s: ReadAllWithKeys = %d bytes, %v", tc.name, len(got), err)
		}
		part, err := vaultread.ReadRangeWithKeys(ctx, f, keys, tc.id, 100, 1500)
		if err != nil || !bytes.Equal(part, tc.want[100:1600]) {
			t.Fatalf("%s: ReadRangeWithKeys = %v", tc.name, err)
		}
	}

	// The current key alone cannot read the pre-rotation file; that is why the
	// ring exists.
	if _, err := vaultread.ReadAll(ctx, f, keys[0], beforeID); !errors.Is(err, encryption.ErrDecrypt) {
		t.Fatalf("the current key alone read a pre-rotation file: %v", err)
	}
	// A key off the ring opens neither, with the opaque refusal.
	if _, err := vaultread.ReadAllWithKeys(ctx, f, []encryption.SpaceKey{mustKey(t)}, afterID); !errors.Is(err, encryption.ErrDecrypt) {
		t.Fatalf("a foreign key: %v", err)
	}
	if _, err := vaultread.ReadAllWithKeys(ctx, f, nil, afterID); err == nil {
		t.Fatal("an empty keyring read a file")
	}
}

// TestOpenManifestWithKeysReturnsTheOpeningKey: the key handed back is the one
// that sealed the manifest, whatever its place on the ring.
func TestOpenManifestWithKeysReturnsTheOpeningKey(t *testing.T) {
	t.Parallel()
	old, cur := mustKey(t), mustKey(t)
	f := newMemFetcher()
	id, want := seedVault(t, f, old, pattern(64))
	m, sk, err := vaultread.OpenManifestWithKeys([]encryption.SpaceKey{cur, old}, id, f.blobs[id])
	if err != nil {
		t.Fatal(err)
	}
	if m.FileID != want.FileID {
		t.Fatalf("opened manifest %s, want %s", m.FileID, want.FileID)
	}
	probe, _ := encryption.EncryptChange(old, []byte("p"))
	if _, err := encryption.DecryptChange(sk, probe); err != nil {
		t.Fatal("OpenManifestWithKeys returned a key other than the one that sealed the manifest")
	}
}
