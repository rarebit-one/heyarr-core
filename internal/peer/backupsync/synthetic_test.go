package backupsync_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/peer/backupsync"
	"github.com/rarebit-one/heyarr-core/internal/persistence/backup"
)

// held is a produced backup ready to be Received: its (signed) manifest and the
// on-disk path to its snapshot bytes.
type produced struct {
	m    backup.Manifest
	snap string
}

// TestConcurrentReceiveAcrossGenerations races six Receives, each carrying a
// different (increasing) generation, into ONE retain=3 store. It probes the
// rename-into-place + prune housekeeping under concurrency: the store must not
// panic or race, must settle on exactly the newest three generations, and must
// still list them newest-first.
//
// The manifests are produced serially (backup.Take uses t.Fatalf, which is
// unsafe off the test goroutine); only the Receives — and the os.Open feeding
// each — run concurrently.
func TestConcurrentReceiveAcrossGenerations(t *testing.T) {
	t.Parallel()
	src := newSource(t, "peer-gen")
	ctx := t.Context()

	const n = 6
	prods := make([]produced, n)
	for i := range prods {
		m, snap := src.take(t)
		prods[i] = produced{m: m, snap: snap}
	}

	store := backupsync.NewStore(t.TempDir(), 3)

	var wg sync.WaitGroup
	for i := range prods {
		wg.Add(1)
		go func(p produced) {
			defer wg.Done()
			f, err := os.Open(p.snap) //nolint:gosec // test path under the source's temp dir
			if err != nil {
				t.Errorf("open snapshot gen %d: %v", p.m.Generation, err)
				return
			}
			defer func() { _ = f.Close() }()
			if _, err := store.Receive(ctx, src.id, src.pub, p.m, f); err != nil {
				t.Errorf("receive gen %d: %v", p.m.Generation, err)
			}
		}(prods[i])
	}
	wg.Wait()

	held, err := store.Held(src.id)
	if err != nil {
		t.Fatalf("held: %v", err)
	}
	if len(held) != 3 {
		t.Fatalf("retain=3 settled on %d generations, want 3 (held: %+v)", len(held), held)
	}
	// Newest-first ordering.
	for i := 1; i < len(held); i++ {
		if held[i-1].Generation <= held[i].Generation {
			t.Errorf("Held not sorted newest-first: %d then %d", held[i-1].Generation, held[i].Generation)
		}
	}
	// The three survivors are the three newest produced.
	want := []int64{prods[5].m.Generation, prods[4].m.Generation, prods[3].m.Generation}
	for i, w := range want {
		if held[i].Generation != w {
			t.Errorf("held[%d] generation = %d, want the newest three %v", i, held[i].Generation, want)
		}
	}
}

// TestOpenHeldBackupEdgeCases exercises the three answers OpenHeldBackup owes a
// recovering node: generation 0 means "give me the newest", an unheld
// generation is ErrNoSuchBackup, and a source id that is a path is refused
// before it can escape the store root.
func TestOpenHeldBackupEdgeCases(t *testing.T) {
	t.Parallel()
	src := newSource(t, "peer-open")
	store := backupsync.NewStore(t.TempDir(), 10)
	ctx := t.Context()

	var gens []int64
	for i := 0; i < 3; i++ {
		m, snap := src.take(t)
		if _, err := store.Receive(ctx, src.id, src.pub, m, open(t, snap)); err != nil {
			t.Fatalf("receive %d: %v", i, err)
		}
		gens = append(gens, m.Generation)
	}
	latest := gens[len(gens)-1]

	// generation 0 → the latest held; header must decode and the reader must open.
	mj, rc, err := store.OpenHeldBackup(src.id, 0)
	if err != nil {
		t.Fatalf("open latest: %v", err)
	}
	if rc == nil {
		t.Fatal("open latest: nil reader")
	}
	var m backup.Manifest
	if uerr := json.Unmarshal(mj, &m); uerr != nil {
		t.Errorf("decode manifest header: %v", uerr)
	}
	_ = rc.Close()
	if m.Generation != latest {
		t.Errorf("OpenHeldBackup(0) served generation %d, want the latest %d", m.Generation, latest)
	}

	// A generation this peer does not hold.
	if _, _, err := store.OpenHeldBackup(src.id, latest+9999); !errors.Is(err, backupsync.ErrNoSuchBackup) {
		t.Errorf("unheld generation: got %v, want ErrNoSuchBackup", err)
	}

	// A path-traversal source id must be refused before any filesystem access.
	if _, _, err := store.OpenHeldBackup("../evil", 0); !errors.Is(err, backupsync.ErrBadSourceID) {
		t.Errorf("traversal source id: got %v, want ErrBadSourceID", err)
	}
}

// TestDistributeManyPartialFailures scales the progress-with-whoever-it-has
// property: twenty targets, ten wired to fail in the pusher and ten to succeed.
// Every target gets an outcome; each success carries a nil error and a recorded
// belief; each failure carries a non-nil error and NO belief.
func TestDistributeManyPartialFailures(t *testing.T) {
	t.Parallel()
	ctrl := newController(t)

	src := newSource(t, "controller-self")
	m, snap := src.take(t)
	backupDir := filepath.Dir(snap)

	const nSucc, nFail = 10, 10
	accept := map[string]int64{}
	fail := map[string]error{}
	var success, failure []Target
	for i := 0; i < nSucc; i++ {
		tg := ctrl.addPeer(t, fmt.Sprintf("ok-%02d", i))
		accept[tg.Peer.PeerID] = m.Generation
		success = append(success, tg)
	}
	for i := 0; i < nFail; i++ {
		tg := ctrl.addPeer(t, fmt.Sprintf("bad-%02d", i))
		fail[tg.Peer.PeerID] = fmt.Errorf("connection refused to bad-%02d", i)
		failure = append(failure, tg)
	}

	pusher := &fakePusher{accept: accept, fail: fail}
	beliefs := backupsync.NewBeliefs(ctrl.db)

	targets := make([]Target, 0, nSucc+nFail)
	targets = append(targets, success...)
	targets = append(targets, failure...)

	outcomes := backupsync.Distribute(t.Context(), pusher, beliefs, targets, backupDir, fixedClock{t: time.Now()}, nil)
	if len(outcomes) != nSucc+nFail {
		t.Fatalf("got %d outcomes, want %d", len(outcomes), nSucc+nFail)
	}
	byPeer := map[string]backupsync.Outcome{}
	for _, o := range outcomes {
		byPeer[o.PeerID] = o
	}
	if len(byPeer) != nSucc+nFail {
		t.Fatalf("outcomes cover %d distinct peers, want %d", len(byPeer), nSucc+nFail)
	}

	for _, tg := range success {
		o, ok := byPeer[tg.Peer.PeerID]
		if !ok {
			t.Errorf("no outcome for success peer %s", tg.Peer.Name)
			continue
		}
		if o.Err != nil {
			t.Errorf("success peer %s reported error %v", tg.Peer.Name, o.Err)
		}
		gen, ok, err := beliefs.Of(t.Context(), tg.Peer.PeerID)
		if err != nil || !ok || gen != m.Generation {
			t.Errorf("belief for %s = (%d,%v,%v), want (%d,true,nil)", tg.Peer.Name, gen, ok, err, m.Generation)
		}
	}
	for _, tg := range failure {
		o, ok := byPeer[tg.Peer.PeerID]
		if !ok {
			t.Errorf("no outcome for failure peer %s", tg.Peer.Name)
			continue
		}
		if o.Err == nil {
			t.Errorf("failure peer %s reported nil error", tg.Peer.Name)
		}
		if _, ok, _ := beliefs.Of(t.Context(), tg.Peer.PeerID); ok {
			t.Errorf("a belief was recorded for failed peer %s", tg.Peer.Name)
		}
	}
}

// TestBeliefConcurrentRecord races eight Records of increasing generations for
// one peer. The monotonic upsert must land on the maximum with no torn state,
// under -race.
func TestBeliefConcurrentRecord(t *testing.T) {
	t.Parallel()
	ctrl := newController(t)
	p := ctrl.addPeer(t, "peer-conc") // membership.Register satisfies the FK.
	beliefs := backupsync.NewBeliefs(ctrl.db)
	ctx := t.Context()
	now := time.Now()

	const n = 8
	var wg sync.WaitGroup
	for i := int64(1); i <= n; i++ {
		wg.Add(1)
		go func(gen int64) {
			defer wg.Done()
			if err := beliefs.Record(ctx, p.Peer.PeerID, gen, fmt.Sprintf("blake3:%02d", gen), now); err != nil {
				t.Errorf("record generation %d: %v", gen, err)
			}
		}(i)
	}
	wg.Wait()

	gen, ok, err := beliefs.Of(ctx, p.Peer.PeerID)
	if err != nil || !ok {
		t.Fatalf("of: ok=%v err=%v", ok, err)
	}
	if gen != n {
		t.Errorf("final belief = %d, want the maximum %d (monotonic under concurrency)", gen, n)
	}
}

// TestHeldBackupsDescendingOutOfOrder stores generations in a shuffled receive
// order and asserts HeldBackups still returns them strictly descending — the
// ordering is the store's, not the arrival sequence's.
func TestHeldBackupsDescendingOutOfOrder(t *testing.T) {
	t.Parallel()
	src := newSource(t, "peer-order")
	store := backupsync.NewStore(t.TempDir(), 10)
	ctx := t.Context()

	const n = 4
	prods := make([]produced, n)
	for i := range prods {
		m, snap := src.take(t)
		prods[i] = produced{m: m, snap: snap}
	}

	// Receive out of generation order: indices 2, 0, 3, 1.
	for _, idx := range []int{2, 0, 3, 1} {
		if _, err := store.Receive(ctx, src.id, src.pub, prods[idx].m, open(t, prods[idx].snap)); err != nil {
			t.Fatalf("receive idx %d (gen %d): %v", idx, prods[idx].m.Generation, err)
		}
	}

	gens, err := store.HeldBackups(src.id)
	if err != nil {
		t.Fatalf("held backups: %v", err)
	}
	if len(gens) != n {
		t.Fatalf("held %d generations, want %d (%v)", len(gens), n, gens)
	}
	for i := 1; i < len(gens); i++ {
		if gens[i-1] <= gens[i] {
			t.Fatalf("HeldBackups not strictly descending: %v", gens)
		}
	}
}
