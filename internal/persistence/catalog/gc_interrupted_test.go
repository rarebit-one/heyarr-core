package catalog_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rarebit-one/void-which-binds-go/hashing"

	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
)

// #910: a reclaim interrupted partway must leave a catalog that converges, and
// fsck must never report what it leaves as damage. Two halves: the order the
// collector uses (row committed, then the bytes unlinked) is pinned by
// failing the unlink after the commit; and the state the opposite order leaves
// (bytes gone, released row still there), which nodes already carry, is
// reconciled rather than flagged.

// dyingStore is a content store whose next Delete fails, as a process killed
// after the reclaim committed and before the unlink would.
type dyingStore struct {
	*cas.FS
	die bool
}

var errDied = errors.New("the process died before the unlink")

func (s *dyingStore) Delete(ctx context.Context, h hashing.Hash) error {
	if s.die {
		s.die = false
		return errDied
	}
	return s.FS.Delete(ctx, h)
}

// releasedVaultBlob records a vault blob this node holds, pinned here, then
// unpinned by its owner: released, wanted by nothing.
func releasedVaultBlob(t *testing.T, n *sceneNode, contents string) hashing.Hash {
	t.Helper()
	ctx := context.Background()
	h := n.hold(t, contents)
	if err := n.cat.RecordVaultBlob(ctx, h.String(), int64(len(contents)), n.self, nil); err != nil {
		t.Fatal(err)
	}
	if err := n.cat.UnpinPlacement(ctx, h.String(), n.self); err != nil {
		t.Fatal(err)
	}
	return h
}

func fsck(t *testing.T, n *sceneNode, clock integrity.Clock) integrity.Report {
	t.Helper()
	checker, err := integrity.NewChecker(integrity.Options{Store: n.store, Catalog: n.cat, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	report, err := checker.Check(context.Background(), integrity.CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func TestAReclaimKilledBeforeItsUnlinkConvergesWithNoDamage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// Real time, because the untracked sweep ages bytes by their file's mtime.
	clock := &sceneClock{now: time.Now().UTC()}
	n := newSceneNode(t, clock)
	vault := releasedVaultBlob(t, n, "ciphertext the owner let go")

	store := &dyingStore{FS: n.store}
	gc, err := integrity.NewCollector(integrity.Options{
		Store: store, Catalog: n.cat, Clock: clock,
		Durability: reachableController{t: t}, HonourReleases: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out := n.sweep(t); len(out.Marked) != 1 {
		t.Fatalf("marking pass marked %+v, want the released blob", out.Marked)
	}
	clock.advance(integrity.DefaultGrace + time.Hour)

	store.die = true
	if _, err := gc.Collect(ctx, integrity.CollectOptions{Apply: true}); !errors.Is(err, errDied) {
		t.Fatalf("sweep = %v, want it to die at the unlink", err)
	}
	// The row went first, so the catalog points at nothing that is gone.
	if n.has(t, vault) {
		t.Fatal("the row survived a reclaim that died after committing it")
	}
	if _, err := n.store.Stat(ctx, vault); err != nil {
		t.Fatalf("the bytes went although the unlink never ran: %v", err)
	}
	if report := fsck(t, n, clock); report.Damage() != 0 {
		t.Fatalf("fsck after the crash found damage: %+v", report.Findings)
	}

	// The next sweep finds untracked bytes and finishes the job.
	out, err := gc.Collect(ctx, integrity.CollectOptions{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Untracked) != 1 || out.Untracked[0].Hash != vault.String() {
		t.Fatalf("recovery sweep reclaimed untracked %+v, want the interrupted blob", out.Untracked)
	}
	if _, err := n.store.Stat(ctx, vault); !errors.Is(err, cas.ErrNotFound) {
		t.Fatalf("bytes still present after the recovery sweep: %v", err)
	}
	if report := fsck(t, n, clock); len(report.Findings) != 0 {
		t.Fatalf("fsck after recovery = %+v, want nothing", report.Findings)
	}
}

// The state a bytes-first reclaim leaves when it dies, and what the nodes
// traced in #910 still carry: a released row whose replica says present and
// whose bytes are gone. On the MEDIA profile, which never honours releases and
// so used to spare the row forever under the last-copy rule.
func TestAReleasedRowWhoseBytesAreGoneIsALeftoverNotDamage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := &sceneClock{now: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	n := newSceneNode(t, clock)
	gone := releasedVaultBlob(t, n, "released, and its bytes went first")
	if err := n.store.Delete(ctx, gone); err != nil {
		t.Fatal(err)
	}
	// Loss, for contrast: a blob nothing released, with its bytes gone too.
	lost := hashing.MustParse("blake3:" + repeat("e", 64))
	n.seedBlobs(t, lost.String())

	report := fsck(t, n, clock)
	kinds := map[string]integrity.FindingKind{}
	for _, f := range report.Findings {
		kinds[f.Hash] = f.Kind
	}
	if kinds[gone.String()] != integrity.KindReleasedMissing {
		t.Errorf("released blob with no bytes = %q, want %q", kinds[gone.String()], integrity.KindReleasedMissing)
	}
	if kinds[lost.String()] != integrity.KindMissing {
		t.Errorf("unreleased blob with no bytes = %q, want %q (still damage)", kinds[lost.String()], integrity.KindMissing)
	}
	if report.Damage() != 1 {
		t.Errorf("damage = %d, want only the unreleased blob: %+v", report.Damage(), report.Findings)
	}

	media, err := integrity.NewCollector(integrity.Options{
		Store: n.store, Catalog: n.cat, Clock: clock,
		Durability: reachableController{t: t}, HonourReleases: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	sweep := func(apply bool) integrity.Collection {
		t.Helper()
		out, err := media.Collect(ctx, integrity.CollectOptions{Apply: apply})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := sweep(true); len(out.Reconciled) != 0 {
		t.Fatalf("the marking pass dropped %+v; the grace window still applies", out.Reconciled)
	}
	clock.advance(integrity.DefaultGrace + time.Hour)

	if out := sweep(false); len(out.Reconciled) != 1 || !n.has(t, gone) {
		t.Fatalf("dry run: reconciled %+v, row kept %v; want it reported and kept", out.Reconciled, n.has(t, gone))
	}
	out := sweep(true)
	if len(out.Reconciled) != 1 || out.Reconciled[0].Hash != gone.String() {
		t.Fatalf("reconciled %+v, want the released row", out.Reconciled)
	}
	if len(out.Reclaimed) != 0 || out.BytesReclaimed != 0 {
		t.Errorf("reclaimed %+v (%d bytes); dropping a row frees nothing", out.Reclaimed, out.BytesReclaimed)
	}
	if n.has(t, gone) {
		t.Error("the released row is still there")
	}
	if !n.has(t, lost) {
		t.Error("the unreleased row went; the last-copy rule must still keep it")
	}
	if got := basisOf(t, n, gone); got != integrity.BasisReleased {
		t.Errorf("evidence basis = %q, want %q", got, integrity.BasisReleased)
	}
	for _, f := range fsck(t, n, clock).Findings {
		if f.Hash == gone.String() {
			t.Errorf("fsck still reports the reconciled blob: %+v", f)
		}
	}
}
