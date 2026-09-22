package statesync_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync"
)

func validBlob(pair string) string { return "blake3:" + strings.Repeat(pair, 32) }

// TestDriveSnapshotBridgeRoundTrips is the W2 (#538) proof that a vault drive rides
// the generalised snapshot bridge: a materialised [crdt.Drive] encodes to an opaque
// encrypted snapshot and a device holding the key reconstructs the byte-identical
// drive, while the snapshot at rest leaks no path.
func TestDriveSnapshotBridgeRoundTrips(t *testing.T) {
	dev, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	recip := client.Recipient{ID: encryption.FormatPublicKey(dev.PublicKey().Bytes()), Key: dev.PublicKey()}
	m := client.New()
	sp, wrapped, err := m.Create(spaces.KindPersonal, time.Now().UTC(), []client.Recipient{recip})
	if err != nil {
		t.Fatal(err)
	}

	// Build a drive with an edit (version history) and a concurrent conflict.
	d := crdt.NewDrive()
	if _, err := d.Put("Secret Plans/roadmap.md", validBlob("a1"), 10, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Put("Secret Plans/roadmap.md", validBlob("b2"), 20, 2); err != nil {
		t.Fatal(err)
	}
	x, _ := crdt.NewDrive().Put("photo.jpg", validBlob("c3"), 30, 3)
	y, _ := crdt.NewDrive().Put("photo.jpg", validBlob("d4"), 40, 4)
	d.Apply(x, y)

	snap, err := statesync.EncodeDriveSnapshot(m, sp.ID, nil, d)
	if err != nil {
		t.Fatalf("encode drive snapshot: %v", err)
	}

	// Opaque at rest: no plaintext path in the ciphertext.
	for _, secret := range []string{"Secret Plans", "roadmap.md", "photo.jpg"} {
		if bytes.Contains(snap.Ciphertext, []byte(secret)) {
			t.Fatalf("the snapshot leaks the plaintext %q", secret)
		}
	}

	// A fresh device with the same key reconstructs the byte-identical drive.
	fresh := client.New()
	if err := fresh.Open(sp.ID, wrapped[0].Wrapped, client.NewKeyUnwrapper(dev)); err != nil {
		t.Fatal(err)
	}
	got, err := statesync.DecodeDriveSnapshot(fresh, snap)
	if err != nil {
		t.Fatalf("decode drive snapshot: %v", err)
	}
	if got.Encode() != d.Encode() {
		t.Fatalf("snapshot round-trip lost drive state:\n got=%s\nwant=%s", got.Encode(), d.Encode())
	}
}
