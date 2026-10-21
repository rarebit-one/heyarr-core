package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/peer/backupsync"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
)

// countingCatalogSyncer records how many times the beat drove it.
type countingCatalogSyncer struct {
	calls  atomic.Int64
	fired  chan struct{}
	closed atomic.Bool
}

func (c *countingCatalogSyncer) SyncAll(context.Context) (int, int, error) {
	if c.calls.Add(1) == 1 && !c.closed.Swap(true) {
		close(c.fired)
	}
	return 0, 0, nil
}

// TestCatalogOpsSyncBeatDrivesTheSyncerOnATick is the mechanism-with-a-caller
// proof for #449's driver: the beat, on its interval, actually calls SyncAll — it
// is not a converge routine nobody schedules. Injected syncer + a hand-fired ticker, the
// fire channel the synchronisation, context cancelled to stop the goroutine.
func TestCatalogOpsSyncBeatDrivesTheSyncerOnATick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := &countingCatalogSyncer{fired: make(chan struct{})}
	clock := newFakeTicker()
	runCatalogOpsSyncBeat(ctx, s, time.Minute, slog.New(slog.DiscardHandler), clock.newTicker)
	clock.fire(t)

	select {
	case <-s.fired:
		// The beat called SyncAll — the caller exists.
	case <-time.After(5 * time.Second):
		t.Fatal("the catalog-ops sync beat never drove the syncer — it is a mechanism with no caller")
	}
	if s.calls.Load() < 1 {
		t.Fatalf("syncer was called %d times, want >= 1", s.calls.Load())
	}
}

// TestCatalogOpsSyncBeatRespectsAStoppedContext: a cancelled context means the
// runner never drives the syncer.
func TestCatalogOpsSyncBeatRespectsAStoppedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &countingCatalogSyncer{fired: make(chan struct{})}
	cancel()
	clock := newFakeTicker()
	runCatalogOpsSyncBeat(ctx, s, time.Minute, slog.New(slog.DiscardHandler), clock.newTicker)
	clock.awaitStop(t)
	if s.calls.Load() != 0 {
		t.Fatalf("a cancelled beat still drove the syncer %d times", s.calls.Load())
	}
}

// TestPartialPeersSyncCatalogOpsButReceiveNoBytes: two sites that each fetch
// their own media (ADR-0119) enrol each other as Partial Peers. The catalog-ops
// sync reaches them, but the byte-moving fan-outs, which are Full-only by §19,
// do not: state replication (fullPeerLister) and backups
// (backupsync.FullPeerTargets) skip a Partial Peer, and so do cache, archive and
// compute peers, which hold no catalog to agree on.
func TestPartialPeersSyncCatalogOpsButReceiveNoBytes(t *testing.T) {
	ctx := context.Background()
	store := realFabric(t)
	register := func(name, mode string, self bool) string {
		t.Helper()
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		res, err := store.Register(ctx, membership.Registration{
			Name: name, Site: "site-" + name, Mode: mode, PublicKey: pub, IsSelf: self,
			Endpoint: "https://" + name + ".invalid:7780",
		})
		if err != nil {
			t.Fatal(err)
		}
		return res.Member.PeerID
	}
	self := register("self", "full", true)
	full := register("north", "full", false)
	partial := register("south", "partial", false)
	register("edge", "cache", false)

	ids := func(targets []string) map[string]bool {
		out := map[string]bool{}
		for _, id := range targets {
			out[id] = true
		}
		return out
	}

	siblings, err := catalogSiblings{members: store, self: self}.Siblings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var syncIDs []string
	for _, s := range siblings {
		syncIDs = append(syncIDs, s.Peer.PeerID)
	}
	got := ids(syncIDs)
	if len(got) != 2 || !got[full] || !got[partial] {
		t.Fatalf("catalog-ops siblings = %v, want exactly the Full and the Partial peer", syncIDs)
	}

	state, err := fullPeerLister{members: store, self: self}.FullPeerTargets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(state) != 1 || state[0].Peer.PeerID != full {
		t.Fatalf("state-replication targets = %v, want only the Full peer", state)
	}
	all, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	backups := backupsync.FullPeerTargets(all)
	if len(backups) != 1 || backups[0].Peer.PeerID != full {
		t.Fatalf("backup targets = %v, want only the Full peer", backups)
	}
}
