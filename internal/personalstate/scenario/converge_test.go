package scenario_test

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/replication"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
)

// injected is a fixed timestamp: nothing here depends on the wall clock (ADR-0017).
// The CRDT orders by a Lamport counter and a tag tie-break, not by time, which is
// exactly why convergence is order-independent and this test is deterministic.
var injected = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

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
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.New(store.Options{Writer: db.Writer(), Reader: db.Reader(), Events: log})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// storePusher is a replication.Pusher backed by a real target store: it applies a
// push exactly as the peer route would, so a reconcile runs end-to-end without
// mTLS. It moves only opaque rows — it never decrypts a change.
type storePusher struct{ target *store.Store }

func (p storePusher) PushSpace(ctx context.Context, _ replication.Target, spaceID, kind string) error {
	_, err := p.target.PutSpace(ctx, spaceID, spaces.Kind(kind))
	return err
}

func (p storePusher) PushWrappedKey(ctx context.Context, _ replication.Target, spaceID, recipient string, wrapped []byte) error {
	_, err := p.target.PutWrappedKey(ctx, spaceID, recipient, wrapped)
	return err
}

func (p storePusher) Heads(ctx context.Context, _ replication.Target, spaceID string) ([]string, error) {
	heads, err := p.target.HeadsFor(ctx, spaceID)
	if err != nil {
		return nil, nil // not held yet: the target is missing everything
	}
	return heads, nil
}

func (p storePusher) PushChange(ctx context.Context, _ replication.Target, ch protocol.EncryptedChange) error {
	return p.target.PutChange(ctx, ch)
}

func (p storePusher) PushSnapshot(ctx context.Context, _ replication.Target, snap protocol.EncryptedSnapshot) error {
	return p.target.PutSnapshot(ctx, snap)
}

// reconcile replicates every space on source to target, one direction.
func reconcile(t *testing.T, ctx context.Context, source, target *store.Store) {
	t.Helper()
	tgt := replication.Target{Peer: mtls.Peer{PeerID: "peer"}}
	for _, o := range replication.Reconcile(ctx, source, storePusher{target: target}, []replication.Target{tgt}, nil, nil) {
		if o.Err != nil {
			t.Fatalf("reconcile deferred unexpectedly: %v", o.Err)
		}
	}
}

// putItem is the device side of "add an item": read the space's changes, rebuild
// the CRDT to advance its Lamport clock, add the item, encrypt it under the
// device's space key and push the opaque change to that device's peer store.
// It mirrors the `heyarr space put` command exactly.
func putItem(t *testing.T, ctx context.Context, peer *store.Store, m *client.Manager, spaceID, item string) {
	t.Helper()
	existing, err := peer.ChangesFor(ctx, spaceID)
	if err != nil {
		t.Fatalf("reading changes: %v", err)
	}
	decoded, err := statesync.DecodeAll(m, existing)
	if err != nil {
		t.Fatalf("decoding changes: %v", err)
	}
	st := crdt.New()
	st.Apply(decoded...)
	ch := st.Add(item)
	ec, err := statesync.Encode(m, spaceID, protocol.Heads(existing), ch)
	if err != nil {
		t.Fatalf("encoding change: %v", err)
	}
	if err := peer.PutChange(ctx, ec); err != nil {
		t.Fatalf("storing change: %v", err)
	}
}

// readState is the device side of "read the playlist": decrypt every change the
// peer holds and merge them into the converged, ordered item list.
func readState(t *testing.T, ctx context.Context, peer *store.Store, m *client.Manager, spaceID string) []string {
	t.Helper()
	changes, err := peer.ChangesFor(ctx, spaceID)
	if err != nil {
		t.Fatalf("reading changes: %v", err)
	}
	decoded, err := statesync.DecodeAll(m, changes)
	if err != nil {
		t.Fatalf("decoding changes: %v", err)
	}
	st := crdt.New()
	st.Apply(decoded...)
	return st.IDs()
}

// TestConvergeAfterPartition is Milestone 9's payoff: two devices, one on each of
// two peers, make an OFFLINE concurrent edit during a partition; on reconnect the
// encrypted changes replicate and both devices converge to the SAME state, merged
// CLIENT-SIDE — and the peer stores held only ciphertext throughout, decrypting
// and merging nothing (§42, §43, Invariant 6, ADR-0049).
//
// SABOTAGE (the reviewer's break): make crdt.Items sort by application order
// instead of by the OrderKey (the SABOTAGE NOTE in playlist.go) — the two devices
// applied the same changes in DIFFERENT orders (peer A received y-only last, peer
// B received x-second last), so an order-dependent merge makes xState != yState
// and the convergence assertion fires. Or store plaintext instead of ciphertext,
// and the at-rest assertion fires.
func TestConvergeAfterPartition(t *testing.T) {
	ctx := context.Background()
	peerA := newStore(t)
	peerB := newStore(t)

	// Two devices, each with its own X25519 encryption key.
	xKey, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	yKey, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	xRecip := client.Recipient{ID: encryption.FormatPublicKey(xKey.PublicKey().Bytes()), Key: xKey.PublicKey()}
	yRecip := client.Recipient{ID: encryption.FormatPublicKey(yKey.PublicKey().Bytes()), Key: yKey.PublicKey()}

	// Device X creates the space, wrapping its key for X and Y, and persists the
	// opaque space + wrapped keys on peer A (as the /api/v1 create would).
	mgrX := client.New()
	sp, wrapped, err := mgrX.Create(spaces.KindShared, injected, []client.Recipient{xRecip, yRecip})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peerA.PutSpace(ctx, sp.ID, sp.Kind); err != nil {
		t.Fatal(err)
	}
	for _, w := range wrapped {
		if _, err := peerA.PutWrappedKey(ctx, sp.ID, w.Recipient, w.Wrapped); err != nil {
			t.Fatal(err)
		}
	}

	// X writes the first item, then peer A replicates the whole space to peer B.
	putItem(t, ctx, peerA, mgrX, sp.ID, "x-first")
	reconcile(t, ctx, peerA, peerB)

	// Device Y opens the space from peer B's wrapped copy — the second peer holds
	// the space, and only Y's key opens it.
	mgrY := client.New()
	yWrapped := wrappedFor(t, ctx, peerB, sp.ID, yRecip.ID)
	if err := mgrY.Open(sp.ID, yWrapped, client.NewKeyUnwrapper(yKey)); err != nil {
		t.Fatalf("device Y could not open the replicated space: %v", err)
	}
	if got := readState(t, ctx, peerB, mgrY, sp.ID); len(got) != 1 || got[0] != "x-first" {
		t.Fatalf("device Y read %v from peer B, want [x-first]", got)
	}

	// THE PARTITION: no replication, and a concurrent edit on each side.
	putItem(t, ctx, peerA, mgrX, sp.ID, "x-second")
	putItem(t, ctx, peerB, mgrY, sp.ID, "y-only")

	// During the partition the two peers genuinely diverge.
	if a := readState(t, ctx, peerA, mgrX, sp.ID); contains(a, "y-only") {
		t.Fatal("peer A already has the edit made on peer B — the partition is not real")
	}
	if b := readState(t, ctx, peerB, mgrY, sp.ID); contains(b, "x-second") {
		t.Fatal("peer B already has the edit made on peer A — the partition is not real")
	}

	// RECONNECT: replicate both ways. Peer A learns Y's change, peer B learns X's.
	reconcile(t, ctx, peerA, peerB)
	reconcile(t, ctx, peerB, peerA)

	// CONVERGENCE, merged client-side: device X (from A) and device Y (from B)
	// materialise the SAME ordered playlist, containing all three concurrent items
	// — order-independent, because each peer received the changes in a different
	// order and the two still agree.
	xState := readState(t, ctx, peerA, mgrX, sp.ID)
	yState := readState(t, ctx, peerB, mgrY, sp.ID)
	if !equal(xState, yState) {
		t.Fatalf("the two devices did NOT converge: X=%v, Y=%v", xState, yState)
	}
	if len(xState) != 3 || !contains(xState, "x-first") || !contains(xState, "x-second") || !contains(xState, "y-only") {
		t.Fatalf("the converged state lost a concurrent edit: %v", xState)
	}

	// THE SERVER NEVER SAW PLAINTEXT: peer B's stored change bytes are ciphertext
	// — no item appears in them — throughout the exchange.
	changesB, err := peerB.ChangesFor(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(changesB) != 3 {
		t.Fatalf("peer B holds %d changes, want 3", len(changesB))
	}
	for _, ch := range changesB {
		for _, item := range []string{"x-first", "x-second", "y-only"} {
			if bytes.Contains(ch.Ciphertext, []byte(item)) {
				t.Fatalf("peer B's stored change %s contains the plaintext %q — the server can read it", ch.ChangeID, item)
			}
		}
	}
}

func wrappedFor(t *testing.T, ctx context.Context, peer *store.Store, spaceID, recipient string) []byte {
	t.Helper()
	keys, err := peer.WrappedKeysFor(ctx, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k.Recipient == recipient {
			return k.Wrapped
		}
	}
	t.Fatalf("no wrapped key for %s on the peer", recipient)
	return nil
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
