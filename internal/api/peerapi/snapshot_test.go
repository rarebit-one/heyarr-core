// Every response in this file is drained and closed by the helper that made
// the request, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by the helpers below
package peerapi_test

import (
	"context"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	peercatalog "github.com/rarebit-one/heyarr-core/internal/peer/catalog"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
)

// The catalog snapshot over the authenticated peer link (§52, M4-06, M4-13).
//
// The point of testing it here rather than only against the store is that the
// two properties this route owes cannot be shown anywhere else: that the peer
// a snapshot is built for comes from the CERTIFICATE, and that a real peer can
// pull one over a real pinned connection and materialise it.

var snapshotEpoch = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

// recordingSource is a snapshot source that remembers what it was asked.
type recordingSource struct {
	mu      sync.Mutex
	asked   []string
	holding []int64
	full    []bool
	version int64
	title   string
}

func (s *recordingSource) BuildSnapshot(
	_ context.Context, peerID string, holding int64, full bool,
) (*peercatalog.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, peerID)
	s.holding = append(s.holding, holding)
	s.full = append(s.full, full)
	s.version++
	title := s.title
	if title == "" {
		title = "Arrival"
	}
	return &peercatalog.Snapshot{
		Meta: peercatalog.Meta{
			ControllerID: "controller-a",
			Version:      s.version,
			GeneratedAt:  snapshotEpoch,
			Kind:         peercatalog.KindFull,
			Watermark:    snapshotEpoch,
		},
		Works: []peercatalog.Work{{
			ID: "w-1", ContentType: "movie", WorkKey: "movie:w-1", Title: title,
			SortTitle: "arrival", Attributes: "{}",
			CreatedAt: snapshotEpoch, UpdatedAt: snapshotEpoch,
		}},
	}, nil
}

func (s *recordingSource) askedFor() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asked...)
}

// serveWithSnapshots is serve, plus a catalogue to snapshot from.
func serveWithSnapshots(t *testing.T, self *peerNode, members mtls.Membership, src peerapi.SnapshotSource) *listener {
	t.Helper()
	logs := &syncBuffer{}
	srv, err := peerapi.New(peerapi.Options{
		Addr:       "127.0.0.1:0",
		Material:   self.material,
		Members:    members,
		SelfPeerID: self.peerID,
		Snapshots:  src,
		Logger:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("shutting the peer surface down: %v", err)
		}
	})
	return &listener{srv: srv, self: self, addr: srv.Addr(), logs: logs}
}

// A peer pulls its snapshot over the pinned link and materialises it locally.
func TestAPeerPullsItsSnapshotOverTheAuthenticatedLink(t *testing.T) {
	ctx := context.Background()
	controller := newPeerNode(t, "controller-a", "controller")
	peerB := newPeerNode(t, "peer-b", "site-b")
	root := newTrustRoot(controller.member(), peerB.member())

	src := &recordingSource{}
	l := serveWithSnapshots(t, controller, root, src)

	store, err := peercatalog.Open(ctx, peercatalog.Options{
		Path: filepath.Join(t.TempDir(), "catalog-snapshot.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	refresher, err := peercatalog.NewRefresher(store, peercatalog.HTTPFetcher{
		Client:  dialler(t, peerB, root),
		BaseURL: "https://" + l.addr + peerapi.Prefix,
	})
	if err != nil {
		t.Fatal(err)
	}

	meta, err := refresher.Refresh(ctx, false)
	if err != nil {
		t.Fatalf("refreshing over the peer link: %v", err)
	}
	if meta.Version != 1 || meta.ControllerID != "controller-a" {
		t.Fatalf("meta = %+v, want version 1 from controller-a", meta)
	}

	// The peer the snapshot was built FOR is the one the certificate proved.
	// Nothing in the request named it, and that is the property: a member that
	// could name another peer could read that peer's snapshot and silently
	// advance its version.
	asked := src.askedFor()
	if len(asked) != 1 || asked[0] != "peer-b" {
		t.Fatalf("the controller built the snapshot for %v, want [peer-b]", asked)
	}

	contents, err := store.Contents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(contents.Works) != 1 || contents.Works[0].Title != "Arrival" {
		t.Fatalf("the snapshot did not land: %+v", contents.Works)
	}

	// A second refresh reports what it holds, so the controller can answer
	// incrementally rather than resending the library.
	src.title = "Arrival (Remastered)"
	if _, err := refresher.Refresh(ctx, false); err != nil {
		t.Fatal(err)
	}
	src.mu.Lock()
	holding := append([]int64(nil), src.holding...)
	src.mu.Unlock()
	if len(holding) != 2 || holding[0] != 0 || holding[1] != 1 {
		t.Fatalf("holding = %v, want [0 1]", holding)
	}
	contents, err = store.Contents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if contents.Works[0].Title != "Arrival (Remastered)" {
		t.Fatalf("the second refresh did not land: %+v", contents.Works)
	}
}

// A node that serves the peer fabric but holds no catalogue says so, rather
// than reporting an internal error or hiding the route.
func TestANodeWithNoCatalogueRefusesTheSnapshotHonestly(t *testing.T) {
	controller := newPeerNode(t, "peer-a", "site-a")
	peerB := newPeerNode(t, "peer-b", "site-b")
	root := newTrustRoot(controller.member(), peerB.member())
	l := serve(t, controller, root) // no Snapshots wired

	client := dialler(t, peerB, root)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		"https://"+l.addr+peerapi.Prefix+"/catalog/snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — a peer with no catalogue is not a bug", resp.StatusCode)
	}
}

// A non-member cannot reach the snapshot at all: the refusal is the failed
// handshake, not a status code.
func TestANonMemberCannotReachTheSnapshot(t *testing.T) {
	controller := newPeerNode(t, "controller-a", "controller")
	stranger := newPeerNode(t, "peer-x", "elsewhere")
	root := newTrustRoot(controller.member()) // the stranger is not in it

	l := serveWithSnapshots(t, controller, root, &recordingSource{})
	if err := handshakeTo(t, l.addr, clientConfigFor(t, stranger, root)); err == nil {
		t.Fatal("a non-member completed a usable connection to the snapshot surface")
	}
}

// A malformed `holding` is refused rather than silently treated as zero, which
// would turn a client bug into an unexplained full rebuild every time.
func TestAMalformedHoldingIsRefused(t *testing.T) {
	controller := newPeerNode(t, "controller-a", "controller")
	peerB := newPeerNode(t, "peer-b", "site-b")
	root := newTrustRoot(controller.member(), peerB.member())
	l := serveWithSnapshots(t, controller, root, &recordingSource{})

	client := dialler(t, peerB, root)
	for _, raw := range []string{"banana", "-1"} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
			"https://"+l.addr+peerapi.Prefix+"/catalog/snapshot?holding="+raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("holding=%s gave %d, want 400", raw, resp.StatusCode)
		}
	}
}
