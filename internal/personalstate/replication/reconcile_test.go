package replication_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/replication"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
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

// storePusher is a Pusher backed by a real target store — it applies pushes to
// the store exactly as the peer route would, so a reconcile can be tested
// end-to-end without mTLS. It NEVER decrypts anything; it moves opaque rows.
type storePusher struct{ target *store.Store }

func (p storePusher) PushSpace(ctx context.Context, _ replication.Target, spaceID, kind string) error {
	_, err := p.target.PutSpace(ctx, spaceID, spaces.Kind(kind))
	return err
}

func (p storePusher) PushWrappedKey(ctx context.Context, _ replication.Target, spaceID, recipient string, wrapped []byte, epoch int) error {
	_, err := p.target.PutWrappedKey(ctx, spaceID, recipient, wrapped, epoch)
	if errors.Is(err, store.ErrStaleKeyEpoch) {
		// What the peer route answers (409 key_epoch_superseded) and the real
		// client turns into ErrWrapSuperseded.
		return fmt.Errorf("%w: %w", replication.ErrWrapSuperseded, err)
	}
	return err
}

func (p storePusher) PushKeyHistory(ctx context.Context, _ replication.Target, spaceID string, epoch int, sealedPrev []byte) error {
	return p.target.PutKeyHistory(ctx, spaceID, epoch, sealedPrev)
}

func (p storePusher) Heads(ctx context.Context, _ replication.Target, spaceID string) ([]string, error) {
	heads, err := p.target.HeadsFor(ctx, spaceID)
	if errors.Is(err, store.ErrUnknownSpace) {
		return nil, nil // the target does not hold the space yet — it is missing everything
	}
	return heads, err
}

func (p storePusher) PushChange(ctx context.Context, _ replication.Target, ch protocol.EncryptedChange) error {
	return p.target.PutChange(ctx, ch)
}

func (p storePusher) PushSnapshot(ctx context.Context, _ replication.Target, snap protocol.EncryptedSnapshot) error {
	return p.target.PutSnapshot(ctx, snap)
}

// downPusher is an unreachable peer: every call fails, as a network partition
// looks (ADR-0038).
type downPusher struct{}

var errDown = errors.New("peer unreachable")

func (downPusher) PushSpace(context.Context, replication.Target, string, string) error {
	return errDown
}

func (downPusher) PushWrappedKey(context.Context, replication.Target, string, string, []byte, int) error {
	return errDown
}

func (downPusher) PushKeyHistory(context.Context, replication.Target, string, int, []byte) error {
	return errDown
}

func (downPusher) Heads(context.Context, replication.Target, string) ([]string, error) {
	return nil, errDown
}

func (downPusher) PushChange(context.Context, replication.Target, protocol.EncryptedChange) error {
	return errDown
}

func (downPusher) PushSnapshot(context.Context, replication.Target, protocol.EncryptedSnapshot) error {
	return errDown
}

// seed writes a space, two wrapped keys and two causally-linked changes into a
// store, and returns the space id.
func seed(t *testing.T, s *store.Store) string {
	t.Helper()
	ctx := context.Background()
	sp, err := s.PutSpace(ctx, mustUUID(t), spaces.KindPersonal)
	if err != nil {
		t.Fatal(err)
	}
	// Two recipients, as opaque "x25519:<hex>" ids — this test never touches the
	// encryption package (the boundary forbids it), and the peer never opens these.
	recipients := []string{
		"x25519:" + repeatHex("a1", 32),
		"x25519:" + repeatHex("b2", 32),
	}
	for i, recip := range recipients {
		if _, err := s.PutWrappedKey(ctx, sp.ID, recip, []byte{byte(i + 1), 0x00, 0xff}, 0); err != nil {
			t.Fatal(err)
		}
	}
	chA, _ := protocol.NewChange(sp.ID, nil, []byte("OPAQUE-A"))
	if err := s.PutChange(ctx, chA); err != nil {
		t.Fatal(err)
	}
	chB, _ := protocol.NewChange(sp.ID, []string{chA.ChangeID}, []byte("OPAQUE-B"))
	if err := s.PutChange(ctx, chB); err != nil {
		t.Fatal(err)
	}
	return sp.ID
}

// repeatHex builds a hex string of n bytes by repeating a two-char unit.
func repeatHex(unit string, n int) string {
	out := ""
	for len(out) < n*2 {
		out += unit
	}
	return out[:n*2]
}

func mustUUID(t *testing.T) string {
	t.Helper()
	sp, err := spaces.NewSpace(spaces.KindPersonal, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return sp.ID
}

// TestReconcileConvergesATargetThenIsIdempotent: a target that starts empty ends
// up holding the space, both wrapped keys and both changes; a second reconcile
// pushes nothing (idempotent, Invariant 9).
func TestReconcileConvergesATargetThenIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	local := newStore(t)
	target := newStore(t)
	spaceID := seed(t, local)

	tgt := replication.Target{Peer: mtls.Peer{PeerID: "peer-b", Name: "peer-b"}}
	pusher := storePusher{target: target}

	outcomes := replication.Reconcile(ctx, local, pusher, []replication.Target{tgt}, nil, nil)
	if len(outcomes) != 1 || outcomes[0].Err != nil {
		t.Fatalf("first reconcile: %+v", outcomes)
	}
	if outcomes[0].Pushed != 2 {
		t.Fatalf("first reconcile pushed %d changes, want 2", outcomes[0].Pushed)
	}

	// The target now holds the whole space as ciphertext.
	keys, err := target.WrappedKeysFor(ctx, spaceID)
	if err != nil || len(keys) != 2 {
		t.Fatalf("target wrapped keys = %d (%v), want 2", len(keys), err)
	}
	changes, err := target.ChangesFor(ctx, spaceID)
	if err != nil || len(changes) != 2 {
		t.Fatalf("target changes = %d (%v), want 2", len(changes), err)
	}

	// Idempotent: a second reconcile pushes nothing new.
	again := replication.Reconcile(ctx, local, pusher, []replication.Target{tgt}, nil, nil)
	if again[0].Err != nil || again[0].Pushed != 0 {
		t.Fatalf("second reconcile should push 0, got %+v", again[0])
	}
}

// TestReconcileReplicatesTheLatestSnapshot: a space that carries a snapshot has
// that snapshot replicated to the target alongside its changes (§44), so a Full
// Peer holds a bounded snapshot + tail — not just the change log. The snapshot is
// pushed AFTER its changes, so the target holds the tail the frontier references
// before it receives the snapshot. The peer stores ciphertext it never opens.
func TestReconcileReplicatesTheLatestSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	local := newStore(t)
	target := newStore(t)
	spaceID := seed(t, local)

	// Materialise a snapshot at the current frontier — opaque ciphertext, exactly
	// as a client would push one; this test never touches the encryption package.
	heads, err := local.HeadsFor(ctx, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := protocol.NewSnapshot(spaceID, heads, []byte("OPAQUE-SNAPSHOT"))
	if err != nil {
		t.Fatal(err)
	}
	if err := local.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}

	tgt := replication.Target{Peer: mtls.Peer{PeerID: "peer-b", Name: "peer-b"}}
	pusher := storePusher{target: target}

	outcomes := replication.Reconcile(ctx, local, pusher, []replication.Target{tgt}, nil, nil)
	if len(outcomes) != 1 || outcomes[0].Err != nil {
		t.Fatalf("reconcile: %+v", outcomes)
	}

	// The target now holds the snapshot as ciphertext, content-addressed identically.
	got, has, err := target.LatestSnapshotFor(ctx, spaceID)
	if err != nil || !has {
		t.Fatalf("target snapshot has=%v err=%v, want a snapshot", has, err)
	}
	if got.SnapshotID != snap.SnapshotID {
		t.Fatalf("target snapshot id = %q, want %q", got.SnapshotID, snap.SnapshotID)
	}
	if string(got.Ciphertext) != "OPAQUE-SNAPSHOT" {
		t.Fatalf("target snapshot ciphertext = %q, want the bytes pushed verbatim", got.Ciphertext)
	}

	// Idempotent: a second reconcile re-pushes the same snapshot as a no-op.
	if again := replication.Reconcile(ctx, local, pusher, []replication.Target{tgt}, nil, nil); again[0].Err != nil {
		t.Fatalf("second reconcile errored: %+v", again[0])
	}
}

// TestReconcileDefersUnreachablePeerButConvergesOthers: an unreachable peer is a
// recorded fact, not a failure of the cycle — a reachable peer still converges
// (ADR-0038).
func TestReconcileDefersUnreachablePeerButConvergesOthers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	local := newStore(t)
	reachable := newStore(t)
	seed(t, local)

	down := replication.Target{Peer: mtls.Peer{PeerID: "peer-down"}}
	up := replication.Target{Peer: mtls.Peer{PeerID: "peer-up"}}

	// A composite pusher: the down peer fails, the up peer applies to `reachable`.
	pusher := routingPusher{down: down.Peer.PeerID, up: storePusher{target: reachable}}

	outcomes := replication.Reconcile(ctx, local, pusher, []replication.Target{down, up}, nil, nil)
	var deferred, converged int
	for _, o := range outcomes {
		if o.Err != nil {
			deferred++
		} else {
			converged++
		}
	}
	if deferred != 1 || converged != 1 {
		t.Fatalf("want 1 deferred + 1 converged, got %d + %d (%+v)", deferred, converged, outcomes)
	}
	// The reachable peer converged despite the other being down.
	if list, _ := reachable.ListSpaces(ctx); len(list) != 1 {
		t.Fatalf("the reachable peer holds %d spaces, want 1", len(list))
	}
}

// routingPusher sends the down peer's calls to a failing pusher and everyone
// else's to a store-backed one.
type routingPusher struct {
	down string
	up   storePusher
}

func (r routingPusher) route(t replication.Target) replication.Pusher {
	if t.Peer.PeerID == r.down {
		return downPusher{}
	}
	return r.up
}

func (r routingPusher) PushSpace(ctx context.Context, t replication.Target, s, k string) error {
	return r.route(t).PushSpace(ctx, t, s, k)
}

func (r routingPusher) PushWrappedKey(ctx context.Context, t replication.Target, s, rec string, w []byte, epoch int) error {
	return r.route(t).PushWrappedKey(ctx, t, s, rec, w, epoch)
}

func (r routingPusher) PushKeyHistory(ctx context.Context, t replication.Target, s string, epoch int, sealed []byte) error {
	return r.route(t).PushKeyHistory(ctx, t, s, epoch, sealed)
}

func (r routingPusher) Heads(ctx context.Context, t replication.Target, s string) ([]string, error) {
	return r.route(t).Heads(ctx, t, s)
}

func (r routingPusher) PushChange(ctx context.Context, t replication.Target, ch protocol.EncryptedChange) error {
	return r.route(t).PushChange(ctx, t, ch)
}

func (r routingPusher) PushSnapshot(ctx context.Context, t replication.Target, snap protocol.EncryptedSnapshot) error {
	return r.route(t).PushSnapshot(ctx, t, snap)
}

// keyState is what a peer holds of a space's key: its epoch and, per recipient,
// the epoch and bytes of the copy it serves.
func keyState(t *testing.T, s *store.Store, spaceID string) (int, map[string]store.WrappedKey) {
	t.Helper()
	ctx := context.Background()
	epoch, err := s.KeyEpoch(ctx, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := s.WrappedKeysFor(ctx, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]store.WrappedKey, len(keys))
	for _, k := range keys {
		out[k.Recipient] = k
	}
	return epoch, out
}

// TestReconcileKeyEpochs is ADR-0103's replication half. A peer at epoch 1 must
// not be regressed by a sibling still at epoch 0 — the stale sibling's old wraps
// (including the one the rotation revoked) are skipped, not resurrected, and the
// reconcile still converges the rest of the space. And a rotation replicates
// forward: a target at epoch 0 ends at epoch 1 holding only the epoch-1 wraps, so
// the revoked recipient loses its copy there too.
func TestReconcileKeyEpochs(t *testing.T) {
	t.Parallel()
	keep := "x25519:" + repeatHex("a1", 32)
	revoked := "x25519:" + repeatHex("b2", 32)

	// seedRotated gives s the seed space (two epoch-0 wraps: keep + revoked) and
	// rotates it to epoch 1 for keep only — the revocation.
	seedRotated := func(t *testing.T, s *store.Store, spaceID string) {
		t.Helper()
		if _, err := s.RotateKey(context.Background(), spaceID, 0, []byte("k0-under-k1"),
			[]store.RecipientWrap{{Recipient: keep, Wrapped: []byte("k1-keep")}}, []string{revoked}, nil); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name          string
		rotateSource  bool
		rotateTarget  bool
		wantEpoch     int
		wantRecipient map[string]string // recipient -> wrapped bytes the target serves
	}{
		{
			name:          "a stale source cannot regress a rotated target",
			rotateSource:  false,
			rotateTarget:  true,
			wantEpoch:     1,
			wantRecipient: map[string]string{keep: "k1-keep"},
		},
		{
			name:          "a rotation replicates forward and drops the revoked copy",
			rotateSource:  true,
			rotateTarget:  false,
			wantEpoch:     1,
			wantRecipient: map[string]string{keep: "k1-keep"},
		},
		{
			name:          "two peers at the same epoch stay put",
			rotateSource:  true,
			rotateTarget:  true,
			wantEpoch:     1,
			wantRecipient: map[string]string{keep: "k1-keep"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			source := newStore(t)
			target := newStore(t)
			spaceID := seed(t, source)
			// The target already held the space at epoch 0, exactly as an
			// earlier reconcile would have left it.
			tgt := replication.Target{Peer: mtls.Peer{PeerID: "peer-b"}}
			if o := replication.Reconcile(ctx, source, storePusher{target: target}, []replication.Target{tgt}, nil, nil); o[0].Err != nil {
				t.Fatalf("initial reconcile: %v", o[0].Err)
			}
			if tc.rotateSource {
				seedRotated(t, source, spaceID)
			}
			if tc.rotateTarget {
				seedRotated(t, target, spaceID)
			}

			for i := 0; i < 2; i++ { // twice: the second run is the idempotent one
				outcomes := replication.Reconcile(ctx, source, storePusher{target: target}, []replication.Target{tgt}, nil, nil)
				if len(outcomes) != 1 || outcomes[0].Err != nil {
					t.Fatalf("reconcile %d: %+v", i, outcomes)
				}
			}

			epoch, held := keyState(t, target, spaceID)
			if epoch != tc.wantEpoch {
				t.Fatalf("target epoch = %d, want %d", epoch, tc.wantEpoch)
			}
			if len(held) != len(tc.wantRecipient) {
				t.Fatalf("target serves %d wraps, want %d: %v", len(held), len(tc.wantRecipient), held)
			}
			for r, want := range tc.wantRecipient {
				if got := held[r]; string(got.Wrapped) != want || got.Epoch != tc.wantEpoch {
					t.Fatalf("target wrap for %s = epoch %d %q, want epoch %d %q", r, got.Epoch, got.Wrapped, tc.wantEpoch, want)
				}
			}
			if _, ok := held[revoked]; ok {
				t.Fatal("the revoked recipient's copy was resurrected on the target")
			}
			history, err := target.KeyHistory(ctx, spaceID)
			if err != nil || len(history) != 1 || string(history[0].SealedPrev) != "k0-under-k1" {
				t.Fatalf("target history = %+v, %v; want the one rotation row", history, err)
			}
			// The changes still converged — a skipped stale wrap does not stall
			// the space.
			if changes, err := target.ChangesFor(ctx, spaceID); err != nil || len(changes) != 2 {
				t.Fatalf("target changes = %d (%v), want 2", len(changes), err)
			}
		})
	}
}

// TestReconcileSurfacesAForkedHistory: two peers that rotated the same epoch
// independently hold different rows; replication refuses to overwrite either and
// defers the space instead of silently picking one.
func TestReconcileSurfacesAForkedHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source := newStore(t)
	target := newStore(t)
	spaceID := seed(t, source)
	tgt := replication.Target{Peer: mtls.Peer{PeerID: "peer-b"}}
	if o := replication.Reconcile(ctx, source, storePusher{target: target}, []replication.Target{tgt}, nil, nil); o[0].Err != nil {
		t.Fatal(o[0].Err)
	}
	recip := "x25519:" + repeatHex("a1", 32)
	other := "x25519:" + repeatHex("b2", 32)
	for _, s := range []struct {
		st     *store.Store
		sealed string
	}{{source, "fork-a"}, {target, "fork-b"}} {
		if _, err := s.st.RotateKey(ctx, spaceID, 0, []byte(s.sealed), []store.RecipientWrap{{Recipient: recip, Wrapped: []byte(s.sealed)}}, []string{other}, nil); err != nil {
			t.Fatal(err)
		}
	}
	outcomes := replication.Reconcile(ctx, source, storePusher{target: target}, []replication.Target{tgt}, nil, nil)
	if len(outcomes) != 1 || !errors.Is(outcomes[0].Err, store.ErrKeyHistoryConflict) {
		t.Fatalf("forked reconcile = %+v, want a deferred ErrKeyHistoryConflict", outcomes)
	}
	if h, _ := target.KeyHistory(ctx, spaceID); len(h) != 1 || string(h[0].SealedPrev) != "fork-b" {
		t.Fatalf("the target's own row was overwritten: %+v", h)
	}
}
