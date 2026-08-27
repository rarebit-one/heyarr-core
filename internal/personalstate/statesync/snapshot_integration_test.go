package statesync_test

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
)

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

	// Several changes.
	for _, item := range []string{"one", "two", "three"} {
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
	for _, item := range []string{"one", "two", "three"} {
		if bytes.Contains(stored.Ciphertext, []byte(item)) {
			t.Fatalf("the stored snapshot contains the plaintext %q — it is not opaque", item)
		}
	}

	// Add a tail change AFTER the snapshot.
	put(t, ctx, s, m, sp.ID, "four")

	// Compact: every replica has acknowledged the snapshot's frontier, so the
	// pre-snapshot changes are dropped. The tail ("four") survives.
	dropped, err := s.CompactChanges(ctx, sp.ID, heads)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 3 {
		t.Fatalf("compaction dropped %d changes, want 3", dropped)
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

	if got := base.IDs(); !equalStrings(got, append(full, "four")) {
		t.Fatalf("snapshot+tail reached %v, want %v", got, append(full, "four"))
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
