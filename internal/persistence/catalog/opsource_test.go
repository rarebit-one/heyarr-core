package catalog_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// seedSibling enrols a Partial Peer with a real key, the shape an ADR-0119
// sibling has, and returns its key as an op's `by` names it.
func (h *harness) seedSibling(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h.exec(t, `INSERT INTO peers (id, name, site, mode, is_self, public_key, created_at, enrolled_at)
		VALUES (?, 'sibling', 'site-b', 'partial', 0, ?, ?, ?)`, remotePeer, []byte(pub), stamp, stamp)
	return identity.FormatPublicKey(pub)
}

func (h *harness) blobSize(t *testing.T, hash string) (int64, bool) {
	t.Helper()
	var size int64
	err := h.db.Reader().QueryRow(`SELECT size FROM blobs WHERE hash = ?`, hash).Scan(&size)
	if err != nil {
		return 0, false
	}
	return size, true
}

func (h *harness) replicaStateOf(t *testing.T, hash, peer string) string {
	t.Helper()
	var state string
	if err := h.db.Reader().QueryRow(
		`SELECT state FROM replicas WHERE blob_hash = ? AND peer_id = ?`, hash, peer).Scan(&state); err != nil {
		return ""
	}
	return state
}

// TestRecordOpSourcedBlob is #800's prerequisite: a blob a signed op names
// becomes a row replicate_blob can pull, with the signer as its source, found
// by key because peer ids are node-local (ADR-0012).
func TestRecordOpSourcedBlob(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	signer := h.seedSibling(t)
	blob := hashOf('a')

	source, err := h.cat.RecordOpSourcedBlob(ctx, catalog.OpSourcedBlob{BlobHash: blob, Size: 2048, SignerKey: signer})
	if err != nil {
		t.Fatal(err)
	}
	if source != remotePeer {
		t.Fatalf("source = %q, want this node's id for the signer %q", source, remotePeer)
	}
	if size, ok := h.blobSize(t, blob); !ok || size != 2048 {
		t.Fatalf("blobs row = %d/%v, want the op's size hint 2048", size, ok)
	}
	if got := h.replicaStateOf(t, blob, remotePeer); got != "present" {
		t.Fatalf("signer's replica = %q, want present: the op is its claim about its own disk", got)
	}
	self, err := h.cat.SelfPeer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.replicaStateOf(t, blob, self); got != "" {
		t.Fatalf("this node's replica = %q; nothing is held here until the bytes verify", got)
	}
	if n := h.eventsOf(t, events.TypeBlobCreated); n != 1 {
		t.Fatalf("blob.created = %d, want 1", n)
	}
	if n := h.eventsOf(t, events.TypeReplicaPresent); n != 1 {
		t.Fatalf("replica.present = %d, want 1", n)
	}

	// Restated, it moves nothing and says nothing (invariant 9).
	before := h.eventCount(t)
	if _, err := h.cat.RecordOpSourcedBlob(ctx, catalog.OpSourcedBlob{BlobHash: blob, Size: 2048, SignerKey: signer}); err != nil {
		t.Fatal(err)
	}
	if after := h.eventCount(t); after != before {
		t.Fatalf("a restated claim emitted %d events", after-before)
	}
}

// An existing row keeps its size: a size this node recorded from bytes it
// hashed, or a verified transfer, outranks a hint.
func TestRecordOpSourcedBlobKeepsAnExistingRow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	signer := h.seedSibling(t)
	blob := hashOf('b')
	h.seedBlobs(t, blob) // 1024 bytes

	if _, err := h.cat.RecordOpSourcedBlob(context.Background(),
		catalog.OpSourcedBlob{BlobHash: blob, Size: 9999, SignerKey: signer}); err != nil {
		t.Fatal(err)
	}
	if size, _ := h.blobSize(t, blob); size != 1024 {
		t.Fatalf("size = %d, want the recorded 1024 kept", size)
	}
	if n := h.eventsOf(t, events.TypeBlobCreated); n != 0 {
		t.Fatalf("blob.created = %d for a row that already existed", n)
	}
}

// Refusals write nothing: no row, no replica for a stranger or for this node.
func TestRecordOpSourcedBlobRefusals(t *testing.T) {
	t.Parallel()
	stranger, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		blob   func(t *testing.T, h *harness, self string) catalog.OpSourcedBlob
		wantIs error
	}{
		{
			name: "a signer that is not a member",
			blob: func(*testing.T, *harness, string) catalog.OpSourcedBlob {
				return catalog.OpSourcedBlob{BlobHash: hashOf('c'), Size: 1, SignerKey: identity.FormatPublicKey(stranger)}
			},
			wantIs: catalog.ErrOpSignerNotAMember,
		},
		{
			name: "this node's own op",
			blob: func(_ *testing.T, _ *harness, self string) catalog.OpSourcedBlob {
				return catalog.OpSourcedBlob{BlobHash: hashOf('c'), Size: 1, SignerKey: self}
			},
			wantIs: catalog.ErrOpSignerIsSelf,
		},
		{
			name: "a digest that is not one",
			blob: func(t *testing.T, h *harness, _ string) catalog.OpSourcedBlob {
				return catalog.OpSourcedBlob{BlobHash: "sha256:abc", Size: 1, SignerKey: h.seedSibling(t)}
			},
		},
		{
			name: "a negative size",
			blob: func(t *testing.T, h *harness, _ string) catalog.OpSourcedBlob {
				return catalog.OpSourcedBlob{BlobHash: hashOf('c'), Size: -1, SignerKey: h.seedSibling(t)}
			},
		},
		{
			name: "a signer key that does not parse",
			blob: func(*testing.T, *harness, string) catalog.OpSourcedBlob {
				return catalog.OpSourcedBlob{BlobHash: hashOf('c'), Size: 1, SignerKey: "not-a-key"}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			selfPub, _, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			self, err := h.cat.SelfPeer(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			h.exec(t, `UPDATE peers SET public_key = ? WHERE id = ?`, []byte(selfPub), self)
			b := tc.blob(t, h, identity.FormatPublicKey(selfPub))
			before := h.eventCount(t)
			_, err = h.cat.RecordOpSourcedBlob(context.Background(), b)
			if err == nil {
				t.Fatal("recorded; want a refusal")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("err = %v, want %v", err, tc.wantIs)
			}
			var rows int
			if err := h.db.Reader().QueryRow(`SELECT count(*) FROM blobs`).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 0 {
				t.Fatalf("%d blobs rows written by a refusal", rows)
			}
			if after := h.eventCount(t); after != before {
				t.Fatalf("a refusal emitted %d events", after-before)
			}
		})
	}
}
