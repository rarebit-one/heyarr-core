//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/resources"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/inventory"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// Every HTTP response in this file is closed by the t.Cleanup the harness
// registers, which bodyclose cannot see through.
//
// M4-07, at the layer an operator actually looks at: a peer's report has to be
// VISIBLE on GET /api/v1/replicas?peer_id=… — and the rows have to describe
// the remote peer.
//
// This is the assertion the catalog tests cannot make. They prove the rows are
// written; this proves the read path shows them, filtered by the peer, with
// the freshness column populated. A `replicas` table nobody can query per peer
// is a table nobody can use to decide anything.

// remoteInventoryPeer is a second peer, which the fixture's seed does not
// create — the seeded one is this node.
const remoteInventoryPeer = "01990000-0000-7000-8000-0000000remot"

func TestAPeersInventoryReportIsVisibleOnTheReplicasRoute(t *testing.T) {
	h := newHarness(t).seed()
	ctx := context.Background()

	h.exec(`INSERT INTO peers (id, name, site, mode, is_self, created_at, enrolled_at)
		VALUES (?, 'remote-peer', 'site-b', 'full', 0, ?, ?)`, remoteInventoryPeer, seedTime, seedTime)

	// A catalog over the harness's own database. It is the same construction
	// the controller wires behind the peer surface, and using the real one is
	// the point: a test that inserted the rows by hand would assert that this
	// file knows the schema, not that a report produces them.
	eventLog, err := events.New(events.Options{
		Writer: h.db.Writer(), Reader: h.db.Reader(), Clock: h.clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{
		DB: h.db, Events: eventLog, PeerName: "test", PeerSite: "test-site",
		Clock: h.clock, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}

	observedAt := time.Date(2026, 8, 20, 11, 0, 0, 0, time.UTC)
	if _, err := cat.ReconcileInventory(ctx, remoteInventoryPeer, inventory.Report{
		PeerID: remoteInventoryPeer, Mode: inventory.ModeFull, ObservedAt: observedAt,
		Entries: []inventory.Entry{
			{BlobHash: blob1Hash, State: inventory.StatePresent, BytesPresent: 4096},
		},
	}); err != nil {
		t.Fatal(err)
	}

	resp := h.get("/api/v1/replicas?peer_id=" + remoteInventoryPeer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET replicas = %d", resp.StatusCode)
	}
	var page struct {
		Items []resources.Replica `json:"items"`
	}
	if err := json.Unmarshal(h.body(resp), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("the remote peer's replicas = %d rows, want 1: %+v", len(page.Items), page.Items)
	}
	got := page.Items[0]
	// The first non-self replicas row this system has ever served.
	if got.PeerID != remoteInventoryPeer {
		t.Errorf("peer_id = %q, want the remote peer %q", got.PeerID, remoteInventoryPeer)
	}
	if got.PeerID == peerID {
		t.Errorf("the row describes THIS node (%s); the report was folded in against the self peer", peerID)
	}
	if got.BlobHash != blob1Hash || got.State != "present" || got.BytesPresent != 4096 {
		t.Errorf("row = %+v, want %s present with 4096 bytes", got, blob1Hash)
	}
	// Freshness, on the wire. A `replicas` row whose confirmation date the API
	// does not expose is a row nothing downstream can age.
	if got.ReportedAt == nil {
		t.Fatal("reported_at is null on a row a peer just confirmed")
	}
	if !got.ReportedAt.Equal(observedAt) {
		t.Errorf("reported_at = %s, want the report's observation time %s", got.ReportedAt, observedAt)
	}

	// The filter is a filter: the seeded self-peer rows are still there and
	// still separate. Without this, a route that ignored peer_id entirely
	// would pass everything above.
	all := h.get("/api/v1/replicas")
	var everything struct {
		Items []resources.Replica `json:"items"`
	}
	if err := json.Unmarshal(h.body(all), &everything); err != nil {
		t.Fatal(err)
	}
	if len(everything.Items) != 3 {
		t.Fatalf("the whole table = %d rows, want 3 (two seeded on this node, one reported): %+v",
			len(everything.Items), everything.Items)
	}
	var selfRows int
	for _, r := range everything.Items {
		if r.PeerID == peerID {
			selfRows++
		}
	}
	if selfRows != 2 {
		t.Errorf("this node holds %d rows, want the 2 it was seeded with — a peer's report "+
			"overwrote another peer's replicas", selfRows)
	}
}
