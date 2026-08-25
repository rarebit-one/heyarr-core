package backupsync_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/backupsync"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// controller is a control plane with a membership store, for belief tests.
type controller struct {
	db      *sqlite.DB
	members *membership.Store
}

func newController(t *testing.T) *controller {
	t.Helper()
	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(t.Context(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	m, err := membership.New(membership.Options{DB: db, Events: log})
	if err != nil {
		t.Fatalf("membership: %v", err)
	}
	return &controller{db: db, members: m}
}

// addPeer registers a Full Peer and returns a push target for it.
func (c *controller) addPeer(t *testing.T, name string) backupsync.Target {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.members.Register(t.Context(), membership.Registration{
		Name: name, Mode: "full", Endpoint: "https://" + name + ".invalid:7443", PublicKey: pub,
	})
	if err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	return backupsync.Target{
		Peer:     mtls.Peer{PeerID: res.Member.PeerID, Name: name, PublicKey: pub},
		Endpoint: res.Member.Endpoint,
	}
}

// fakePusher stands in for the mTLS push, so the cycle logic is tested without a
// server. Each peer either accepts (storing a generation) or fails.
type fakePusher struct {
	accept map[string]int64 // peer id -> generation it will store
	fail   map[string]error // peer id -> failure
}

func (f *fakePusher) PushTo(_ context.Context, target Target, _ string) (int64, error) {
	if err := f.fail[target.Peer.PeerID]; err != nil {
		return 0, err
	}
	return f.accept[target.Peer.PeerID], nil
}

// Target is an alias so the fake can satisfy the unexported interface via the
// exported one.
type Target = backupsync.Target

func TestDistributeMakesProgressWithWhoeverItHas(t *testing.T) {
	t.Parallel()
	ctrl := newController(t)
	a := ctrl.addPeer(t, "peer-a")
	b := ctrl.addPeer(t, "peer-b")
	dead := ctrl.addPeer(t, "peer-dead")

	// A real signed backup on disk for the manifest digest.
	src := newSource(t, "controller-self")
	m, snap := src.take(t)
	backupDir := filepath.Dir(snap)

	pusher := &fakePusher{
		accept: map[string]int64{a.Peer.PeerID: m.Generation, b.Peer.PeerID: m.Generation},
		fail:   map[string]error{dead.Peer.PeerID: errors.New("connection refused")},
	}
	beliefs := backupsync.NewBeliefs(ctrl.db)

	outcomes := backupsync.Distribute(t.Context(), pusher, beliefs,
		[]backupsync.Target{a, b, dead}, backupDir, fixedClock{t: time.Now()}, nil)

	if len(outcomes) != 3 {
		t.Fatalf("got %d outcomes, want 3", len(outcomes))
	}
	// The dead peer failed; the other two succeeded — the cycle did not block.
	byPeer := map[string]backupsync.Outcome{}
	for _, o := range outcomes {
		byPeer[o.PeerID] = o
	}
	if byPeer[dead.Peer.PeerID].Err == nil {
		t.Error("the dead peer's push did not fail")
	}
	for _, live := range []Target{a, b} {
		if byPeer[live.Peer.PeerID].Err != nil {
			t.Errorf("live peer %s failed: %v", live.Peer.Name, byPeer[live.Peer.PeerID].Err)
		}
	}
	// Beliefs recorded for the two that succeeded, and NOT for the dead one.
	for _, live := range []Target{a, b} {
		gen, ok, err := beliefs.Of(t.Context(), live.Peer.PeerID)
		if err != nil || !ok || gen != m.Generation {
			t.Errorf("belief for %s = (%d,%v,%v), want (%d,true,nil)", live.Peer.Name, gen, ok, err, m.Generation)
		}
	}
	if _, ok, _ := beliefs.Of(t.Context(), dead.Peer.PeerID); ok {
		t.Error("a belief was recorded for a peer that never received")
	}
}

func TestBeliefIsMonotonic(t *testing.T) {
	t.Parallel()
	ctrl := newController(t)
	p := ctrl.addPeer(t, "peer-a")
	beliefs := backupsync.NewBeliefs(ctrl.db)

	now := time.Now()
	if err := beliefs.Record(t.Context(), p.Peer.PeerID, 5, "blake3:aa", now); err != nil {
		t.Fatal(err)
	}
	// A lower generation arriving later must not move the belief backwards.
	if err := beliefs.Record(t.Context(), p.Peer.PeerID, 3, "blake3:bb", now); err != nil {
		t.Fatal(err)
	}
	gen, ok, err := beliefs.Of(t.Context(), p.Peer.PeerID)
	if err != nil || !ok {
		t.Fatalf("of: %v %v", ok, err)
	}
	if gen != 5 {
		t.Errorf("belief moved backwards to %d, want it to stay 5", gen)
	}
}

// TestBeliefSurvivesAnUnreachablePeer is the acceptance shape: after a peer is
// pushed to and then goes away, the controller still reports what it holds — the
// belief, not a live query.
func TestBeliefSurvivesAnUnreachablePeer(t *testing.T) {
	t.Parallel()
	ctrl := newController(t)
	p := ctrl.addPeer(t, "peer-b")
	beliefs := backupsync.NewBeliefs(ctrl.db)

	if err := beliefs.Record(t.Context(), p.Peer.PeerID, 5, "blake3:aa", time.Now()); err != nil {
		t.Fatal(err)
	}
	// The peer is now "unreachable" — we do not query it. The controller's own
	// latest generation has moved to 6; the peer is behind by one, and that is
	// answerable from the belief alone.
	const localLatest = 6
	held, ok, err := beliefs.Of(t.Context(), p.Peer.PeerID)
	if err != nil || !ok {
		t.Fatalf("of: %v %v", ok, err)
	}
	if behind := localLatest - held; behind != 1 {
		t.Errorf("peer reported %d behind, want 1 (local %d, believed %d)", behind, localLatest, held)
	}
}
