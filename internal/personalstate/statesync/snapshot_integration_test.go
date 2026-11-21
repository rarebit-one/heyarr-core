package statesync_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/encryption"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	db := testdb.Migrated(t)
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

func put(t *testing.T, ctx context.Context, s *store.Store, m *client.Manager, spaceID, item string) {
	t.Helper()
	existing, err := s.ChangesFor(ctx, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := statesync.DecodeAll(m, existing)
	if err != nil {
		t.Fatal(err)
	}
	st := crdt.New()
	st.Apply(decoded...)
	ec, err := statesync.Encode(m, spaceID, protocol.Heads(existing), st.Add(item))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutChange(ctx, ec); err != nil {
		t.Fatal(err)
	}
}

// TestFreshDeviceReachesStateFromSnapshotPlusTail is §44's payoff: after several
// changes, a snapshot is taken and the log compacted; a FRESH device reaches the
// same converged state from the snapshot plus the tail — without the pre-snapshot
// changes, which are gone — and the snapshot at rest is ciphertext the peer
// cannot read.
func TestFreshDeviceReachesStateFromSnapshotPlusTail(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	// A device creates the space (holds the key) and persists it.
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
	if _, err := s.PutSpace(ctx, sp.ID, sp.Kind); err != nil {
		t.Fatal(err)
	}

	// Several changes. The items double as the needles the opacity check below
	// looks for in the ciphertext, so each is long enough that random bytes never
	// contain it by chance: a 3-byte item ("one") turns up somewhere in a ~600-byte
	// ciphertext about once in 14,000 runs, which is how this test went red (#904).
	items := []string{"snapshot-item-one", "snapshot-item-two", "snapshot-item-three"}
	for _, item := range items {
		put(t, ctx, s, m, sp.ID, item)
	}

	// The full converged state, for comparison.
	full := readState(t, ctx, s, m, sp.ID)

	// Take a snapshot at the current frontier and store it.
	heads, err := s.HeadsFor(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	cur := crdt.New()
	{
		changes, _ := s.ChangesFor(ctx, sp.ID)
		decoded, _ := statesync.DecodeAll(m, changes)
		cur.Apply(decoded...)
	}
	snap, err := statesync.EncodeSnapshot(m, sp.ID, heads, cur)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}

	// The snapshot at rest is ciphertext — no item appears in its bytes.
	stored, _, err := s.LatestSnapshotFor(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if bytes.Contains(stored.Ciphertext, []byte(item)) {
			t.Fatalf("the stored snapshot contains the plaintext %q — it is not opaque", item)
		}
	}

	// Add a tail change AFTER the snapshot.
	put(t, ctx, s, m, sp.ID, "tail-item-four")

	// Compact against the snapshot, on a node with no other replica, so the
	// pre-snapshot changes are dropped. The tail ("tail-item-four") survives.
	res, err := s.CompactChanges(ctx, sp.ID, snap.SnapshotID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Dropped != 3 {
		t.Fatalf("compaction dropped %d changes, want 3", res.Dropped)
	}

	// A FRESH device with the same key opens the space, fetches the snapshot +
	// the tail, and reaches the SAME state — without the compacted changes.
	fresh := client.New()
	if err := fresh.Open(sp.ID, wrapped[0].Wrapped, client.NewKeyUnwrapper(dev)); err != nil {
		t.Fatal(err)
	}
	base, err := statesync.DecodeSnapshot(fresh, stored)
	if err != nil {
		t.Fatalf("fresh device could not decode the snapshot: %v", err)
	}
	tail, err := s.ChangesFor(ctx, sp.ID) // only the tail remains after compaction
	if err != nil {
		t.Fatal(err)
	}
	tailDecoded, err := statesync.DecodeAll(fresh, tail)
	if err != nil {
		t.Fatal(err)
	}
	base.Apply(tailDecoded...)

	if got := base.IDs(); !equalStrings(got, append(full, "tail-item-four")) {
		t.Fatalf("snapshot+tail reached %v, want %v", got, append(full, "tail-item-four"))
	}
}

func readState(t *testing.T, ctx context.Context, s *store.Store, m *client.Manager, spaceID string) []string {
	t.Helper()
	changes, err := s.ChangesFor(ctx, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := statesync.DecodeAll(m, changes)
	if err != nil {
		t.Fatal(err)
	}
	st := crdt.New()
	st.Apply(decoded...)
	return st.IDs()
}
