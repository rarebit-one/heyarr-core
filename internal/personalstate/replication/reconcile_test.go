package replication_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/replication"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
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

// storePusher is a Pusher backed by a real target store — it applies pushes to
// the store exactly as the peer route would, so a reconcile can be tested
// end-to-end without mTLS. It NEVER decrypts anything; it moves opaque rows.
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
	if errors.Is(err, store.ErrUnknownSpace) {
		return nil, nil // the target does not hold the space yet — it is missing everything
	}
	return heads, err
}

func (p storePusher) PushChange(ctx context.Context, _ replication.Target, ch protocol.EncryptedChange) error {
	return p.target.PutChange(ctx, ch)
}

// downPusher is an unreachable peer: every call fails, as a network partition
// looks (ADR-0038).
type downPusher struct{}

var errDown = errors.New("peer unreachable")

func (downPusher) PushSpace(context.Context, replication.Target, string, string) error {
	return errDown
}

func (downPusher) PushWrappedKey(context.Context, replication.Target, string, string, []byte) error {
	return errDown
}

func (downPusher) Heads(context.Context, replication.Target, string) ([]string, error) {
	return nil, errDown
}

func (downPusher) PushChange(context.Context, replication.Target, protocol.EncryptedChange) error {
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
		if _, err := s.PutWrappedKey(ctx, sp.ID, recip, []byte{byte(i + 1), 0x00, 0xff}); err != nil {
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

// TestReconcileDefersUnreachablePeerButConvergesOthers: an unreachable peer is a
// recorded fact, not a failure of the cycle — a reachable peer still converges
// (ADR-0038).
func TestReconcileDefersUnreachablePeerButConvergesOthers(t *testing.T) {
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

func (r routingPusher) PushWrappedKey(ctx context.Context, t replication.Target, s, rec string, w []byte) error {
	return r.route(t).PushWrappedKey(ctx, t, s, rec, w)
}

func (r routingPusher) Heads(ctx context.Context, t replication.Target, s string) ([]string, error) {
	return r.route(t).Heads(ctx, t, s)
}

func (r routingPusher) PushChange(ctx context.Context, t replication.Target, ch protocol.EncryptedChange) error {
	return r.route(t).PushChange(ctx, t, ch)
}
