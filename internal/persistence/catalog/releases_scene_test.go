package catalog_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/hashing"

	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
)

// The two-node scene for #779 (ADR-0114): north holds a vault blob and south
// took it from north. North's owner unpins it. North records release intent,
// stops offering the placement, and south's exchange records the withdrawal as
// release intent too. Once the grace window has passed, each site's personal
// GC reclaims its copy, the second one being the last copy in the fabric, with
// the basis `released`. A media blob at south that once carried a stray pin is
// never released, and is still kept.

type sceneClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *sceneClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *sceneClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// reachableController is a node that can reach its controller. Holds is never
// consulted for a released blob, and a blob no peer claims is refused before
// anything is dialled, so asking is a test failure.
type reachableController struct{ t *testing.T }

func (reachableController) Controller(context.Context) error { return nil }

func (d reachableController) Holds(context.Context, integrity.Peer, hashing.Hash) error {
	d.t.Error("a peer was asked whether it holds a blob; nothing in this scene should need to ask")
	return integrity.ErrPeerUnreachable
}

type sceneNode struct {
	*harness
	self  string
	store *cas.FS
	gc    *integrity.Collector
}

func newSceneNode(t *testing.T, clock *sceneClock) *sceneNode {
	t.Helper()
	h := newHarness(t)
	self := h.seedRemotePeer(t) // the other site, as this node knows it
	store, err := cas.OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gc, err := integrity.NewCollector(integrity.Options{
		Store: store, Catalog: h.cat, Clock: clock,
		Durability: reachableController{t: t}, HonourReleases: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &sceneNode{harness: h, self: self, store: store, gc: gc}
}

// hold stores the bytes in this node's content store.
func (n *sceneNode) hold(t *testing.T, contents string) hashing.Hash {
	t.Helper()
	d, err := n.store.Put(context.Background(), strings.NewReader(contents))
	if err != nil {
		t.Fatal(err)
	}
	return d.Hash
}

func (n *sceneNode) sweep(t *testing.T) integrity.Collection {
	t.Helper()
	out, err := n.gc.Collect(context.Background(), integrity.CollectOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func (n *sceneNode) has(t *testing.T, h hashing.Hash) bool {
	t.Helper()
	known, err := n.cat.Known(context.Background(), []hashing.Hash{h})
	if err != nil {
		t.Fatal(err)
	}
	return known[h.String()]
}

func basisOf(t *testing.T, n *sceneNode, h hashing.Hash) string {
	t.Helper()
	var basis string
	if err := n.db.Reader().QueryRowContext(context.Background(),
		`SELECT basis FROM durability_evidence WHERE blob_hash = ?`, h.String()).Scan(&basis); err != nil {
		t.Fatalf("no durability evidence for %s: %v", h, err)
	}
	return basis
}

func TestSceneAReleasedVaultBlobIsReclaimedAtBothSitesAndMediaIsKept(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := &sceneClock{now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	north, south := newSceneNode(t, clock), newSceneNode(t, clock)

	// North: the owner's upload, pinned here.
	vault := north.hold(t, "ciphertext the owner will delete")
	if err := north.cat.RecordVaultBlob(ctx, vault.String(), int64(len("ciphertext the owner will delete")), north.self); err != nil {
		t.Fatal(err)
	}
	// South: took the placement from north and pulled the bytes.
	offered, err := north.cat.OfferPlacements(ctx, remotePeer, true, all)
	if err != nil {
		t.Fatal(err)
	}
	if len(offered) != 1 || offered[0].BlobHash != vault.String() {
		t.Fatalf("north offered %+v, want the vault blob", offered)
	}
	if _, _, err := south.cat.ReplacePlacements(ctx, remotePeer, all, offered); err != nil {
		t.Fatal(err)
	}
	south.hold(t, "ciphertext the owner will delete")

	// South also holds library media that once carried a stray pin.
	media := south.hold(t, "a film in the library")
	south.seedBlobs(t, media.String())
	south.exec(t, `INSERT INTO libraries (id, name, content_type, enabled, created_at)
		VALUES ('lib-1', 'Films', 'movie', 1, ?)`, stamp)
	south.exec(t, `INSERT INTO library_roots (id, library_id, path, ingest_mode, enabled, created_at)
		VALUES ('root-1', 'lib-1', '/srv/films', 'link', 1, ?)`, stamp)
	south.exec(t, `INSERT INTO scanned_files (root_id, path, size, mtime_ns, blob_hash, scanned_at)
		VALUES ('root-1', 'film.mkv', 1024, 1, ?, ?)`, media.String(), stamp)
	if err := south.cat.PinPlacement(ctx, media.String(), south.self); err != nil {
		t.Fatal(err)
	}
	if err := south.cat.UnpinPlacement(ctx, media.String(), south.self); err != nil {
		t.Fatal(err)
	}

	// The owner deletes the file: north unpins, and the next exchange carries
	// the withdrawal to south.
	if err := north.cat.UnpinPlacement(ctx, vault.String(), north.self); err != nil {
		t.Fatal(err)
	}
	offered, err = north.cat.OfferPlacements(ctx, remotePeer, true, all)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := south.cat.ReplacePlacements(ctx, remotePeer, all, offered); err != nil {
		t.Fatal(err)
	}

	// First sweep at each site starts the grace window and reclaims nothing.
	for name, n := range map[string]*sceneNode{"north": north, "south": south} {
		if out := n.sweep(t); len(out.Reclaimed) != 0 {
			t.Fatalf("%s reclaimed %+v on the marking pass", name, out.Reclaimed)
		}
	}
	clock.advance(integrity.DefaultGrace + time.Hour)

	// South reclaims first: north still holds a copy, but release intent is
	// the basis, not north's claim.
	out := south.sweep(t)
	if len(out.Reclaimed) != 1 || out.Reclaimed[0].Hash != vault.String() {
		t.Fatalf("south reclaimed %+v, want only the released vault blob", out.Reclaimed)
	}
	if got := basisOf(t, south, vault); got != integrity.BasisReleased {
		t.Fatalf("south's evidence basis = %q, want %q", got, integrity.BasisReleased)
	}
	if !south.has(t, media) {
		t.Fatal("south reclaimed library media")
	}
	spared := false
	for _, s := range out.Spared {
		if s.Hash == media.String() && s.Reason == integrity.ReasonNoOtherPeer {
			spared = true
		}
	}
	if !spared {
		t.Fatalf("south's media was not spared under the last-copy rule: %+v", out.Spared)
	}

	// North now holds the last copy in the fabric, and reclaims it too.
	out = north.sweep(t)
	if len(out.Reclaimed) != 1 || out.Reclaimed[0].Hash != vault.String() {
		t.Fatalf("north reclaimed %+v, want the released vault blob (the fabric's last copy)", out.Reclaimed)
	}
	if got := basisOf(t, north, vault); got != integrity.BasisReleased {
		t.Fatalf("north's evidence basis = %q, want %q", got, integrity.BasisReleased)
	}
	for name, n := range map[string]*sceneNode{"north": north, "south": south} {
		if n.has(t, vault) {
			t.Errorf("%s still has a row for the released vault blob", name)
		}
		if _, err := n.store.Stat(ctx, vault); err == nil {
			t.Errorf("%s still holds the released vault blob's bytes", name)
		}
	}
}
