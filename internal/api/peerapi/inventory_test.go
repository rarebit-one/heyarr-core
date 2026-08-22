// Every response in this file is drained and closed by the helper that made
// the request, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by peerSend
package peerapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/peer/inventory"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
)

// M4-07's surface, and M4-06's rule applied to it (ADR-0033).
//
// The property under test is the same sentence the attach tests are about, and
// it has to be re-asserted here rather than assumed: THE ACTING PEER COMES
// FROM THE CERTIFICATE AND NEVER FROM THE REQUEST BODY. This route is where it
// starts to matter, because it is the first one that WRITES — an inventory
// report filed under somebody else's id does not merely misreport a peer, it
// overwrites that peer's replicas with a stranger's disk.
//
// A test that only sent well-formed reports would pass against a server that
// read the acting peer straight out of the body. So the central test sends a
// report declaring a DIFFERENT peer and asserts a refusal, paired with a
// control proving the same request shape succeeds when the declaration
// matches — and with an assertion on the id the SINK was called with, because
// a 200 is not evidence that the right peer was written.

// recordingSink captures what the surface handed the control plane.
//
// It records the peerID ARGUMENT separately from the report's declaration,
// which is the only way to tell a server that authorised correctly and then
// wrote the wrong id from one that did neither.
type recordingSink struct {
	mu       sync.Mutex
	calls    int
	actedAs  []string
	declared []string
	reports  []inventory.Report
	err      error
}

func (s *recordingSink) ReconcileInventory(
	_ context.Context, peerID string, report inventory.Report,
) (inventory.Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.actedAs = append(s.actedAs, peerID)
	s.declared = append(s.declared, report.PeerID)
	s.reports = append(s.reports, report)
	if s.err != nil {
		return inventory.Outcome{}, s.err
	}
	return inventory.Outcome{
		ReportID: "report-1", PeerID: peerID, Mode: report.Mode,
		Entries: len(report.Entries), Added: len(report.Entries),
	}, nil
}

func (s *recordingSink) snapshot() (calls int, actedAs, declared []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, append([]string(nil), s.actedAs...), append([]string(nil), s.declared...)
}

// serveWithSink is serve() with a control plane behind the inventory route.
func serveWithSink(t *testing.T, self *peerNode, members mtls.Membership, sink peerapi.InventorySink) *listener {
	t.Helper()
	logs := &syncBuffer{}
	srv, err := peerapi.New(peerapi.Options{
		Addr:       "127.0.0.1:0",
		Material:   self.material,
		Members:    members,
		SelfPeerID: self.peerID,
		Inventory:  sink,
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

func (l *listener) inventoryURL() string {
	return "https://" + l.addr + peerapi.Prefix + "/inventory"
}

const someHash = "blake3:1111111111111111111111111111111111111111111111111111111111111111"

// reportBody renders a report declaring declared.
func reportBody(t *testing.T, declared string) string {
	t.Helper()
	body, err := json.Marshal(inventory.Report{
		PeerID:     declared,
		Mode:       inventory.ModeFull,
		ObservedAt: time.Date(2026, 8, 1, 6, 0, 0, 0, time.UTC),
		Entries: []inventory.Entry{
			{BlobHash: someHash, State: inventory.StatePresent, BytesPresent: 1024},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// ---------------------------------------------------------------------------
// the impersonation refusal — M4-06's rule on M4-07's route

// TestAPeerCannotReportAnotherPeersInventory is the sabotage target.
//
// Both ends are honest in every other test in this package. This is the one
// that is not, and without it a surface that read the acting peer out of the
// body would authenticate every peer perfectly and then let any of them
// overwrite any other's replicas — and every test in which nobody lies would
// still pass.
func TestAPeerCannotReportAnotherPeersInventory(t *testing.T) {
	controller := newPeerNode(t, "controller-id", "controller")
	honest := newPeerNode(t, "honest-peer-id", "honest-peer")
	victim := newPeerNode(t, "victim-peer-id", "victim-peer")
	root := newTrustRoot(controller.member(), honest.member(), victim.member())
	sink := &recordingSink{}
	l := serveWithSink(t, controller, root, sink)
	c := dialler(t, honest, root)

	status, body, _, err := peerSend(t, c, http.MethodPost, l.inventoryURL(), reportBody(t, victim.peerID))
	if err != nil {
		t.Fatalf("the request did not complete: %v", err)
	}
	if status != http.StatusForbidden {
		t.Fatalf("a peer reporting another peer's inventory got %d, want 403\n%s", status, body)
	}
	// The refusal must happen BEFORE the control plane is touched. A server
	// that wrote the rows and then answered 403 would have already replaced
	// the victim's inventory.
	if calls, _, _ := sink.snapshot(); calls != 0 {
		t.Errorf("the control plane was called %d times on a refused report; the write must not happen at all", calls)
	}
	if !strings.Contains(body, victim.peerID) || !strings.Contains(body, honest.peerID) {
		t.Errorf("the refusal names neither peer, so an operator cannot tell what was attempted:\n%s", body)
	}

	// The control: the same request shape, honestly declared, succeeds. Without
	// it, a route that refused EVERY report would pass the assertion above.
	status, body, _, err = peerSend(t, c, http.MethodPost, l.inventoryURL(), reportBody(t, honest.peerID))
	if err != nil {
		t.Fatalf("the honest request did not complete: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("an honestly declared report got %d, want 200\n%s", status, body)
	}
	calls, actedAs, declared := sink.snapshot()
	if calls != 1 {
		t.Fatalf("the control plane was called %d times, want 1", calls)
	}
	// The assertion a status code cannot make: the id the control plane was
	// told to write against is the one the CERTIFICATE proved.
	if actedAs[0] != honest.peerID {
		t.Errorf("the controller recorded the report against %q, want the certificate's peer %q",
			actedAs[0], honest.peerID)
	}
	if declared[0] != honest.peerID {
		t.Errorf("the declaration reaching the sink was %q, want %q", declared[0], honest.peerID)
	}
}

// TestTheActingPeerIsTheCertificateEvenWhenTheBodyAgrees closes the gap the
// test above cannot: when both ends are honest, "took it from the certificate"
// and "took it from the body" produce identical results.
//
// So this makes them differ. The body declares the acting peer correctly, and
// the assertion is that the sink was handed the certificate's id for a peer
// whose NAME differs from anything in the body — a server reading the body
// would have no way to produce it.
func TestTheActingPeerIsTheCertificateEvenWhenTheBodyAgrees(t *testing.T) {
	controller := newPeerNode(t, "controller-id", "controller")
	remote := newPeerNode(t, "remote-peer-id", "remote-peer")
	root := newTrustRoot(controller.member(), remote.member())
	sink := &recordingSink{}
	l := serveWithSink(t, controller, root, sink)
	c := dialler(t, remote, root)

	status, body, _, err := peerSend(t, c, http.MethodPost, l.inventoryURL(), reportBody(t, remote.peerID))
	if err != nil {
		t.Fatalf("the request did not complete: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("POST inventory = %d, want 200\n%s", status, body)
	}

	var outcome inventory.Outcome
	if err := json.Unmarshal([]byte(body), &outcome); err != nil {
		t.Fatalf("the peer surface answered something that is not an outcome: %v\n%s", err, body)
	}
	if outcome.PeerID != remote.peerID {
		t.Errorf("the outcome names peer %q, want %q", outcome.PeerID, remote.peerID)
	}
	if outcome.Entries != 1 {
		t.Errorf("entries = %d, want 1", outcome.Entries)
	}
	_, actedAs, _ := sink.snapshot()
	if len(actedAs) != 1 || actedAs[0] != remote.peerID {
		t.Errorf("the sink was handed %v, want [%s] derived from the certificate", actedAs, remote.peerID)
	}
}

// ---------------------------------------------------------------------------
// the ordinary refusals

func TestAnInventoryReportWithNoDeclaredPeerIsRefused(t *testing.T) {
	controller := newPeerNode(t, "controller-id", "controller")
	remote := newPeerNode(t, "remote-peer-id", "remote-peer")
	root := newTrustRoot(controller.member(), remote.member())
	sink := &recordingSink{}
	l := serveWithSink(t, controller, root, sink)
	c := dialler(t, remote, root)

	// Not defaulted to the certificate's id: a body that may be omitted is a
	// body that stops being compared, and the comparison is the only reason it
	// exists.
	status, body, _, err := peerSend(t, c, http.MethodPost, l.inventoryURL(),
		`{"mode":"full","observed_at":"2026-08-01T06:00:00Z","entries":[]}`)
	if err != nil {
		t.Fatalf("the request did not complete: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("a report with no peer_id got %d, want 400\n%s", status, body)
	}
	if calls, _, _ := sink.snapshot(); calls != 0 {
		t.Errorf("the control plane was called %d times on a malformed report", calls)
	}
}

func TestAMalformedInventoryReportIsRefusedBeforeTheControlPlane(t *testing.T) {
	controller := newPeerNode(t, "controller-id", "controller")
	remote := newPeerNode(t, "remote-peer-id", "remote-peer")
	root := newTrustRoot(controller.member(), remote.member())
	sink := &recordingSink{}
	l := serveWithSink(t, controller, root, sink)
	c := dialler(t, remote, root)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"an unreportable state", `{"peer_id":"remote-peer-id","mode":"full","observed_at":"2026-08-01T06:00:00Z",` +
			`"entries":[{"blob_hash":"` + someHash + `","state":"pending"}]}`},
		{"a hash that is not one", `{"peer_id":"remote-peer-id","mode":"full","observed_at":"2026-08-01T06:00:00Z",` +
			`"entries":[{"blob_hash":"not-a-hash","state":"present"}]}`},
		{"no observation time", `{"peer_id":"remote-peer-id","mode":"full","entries":[]}`},
		{"a mode nobody defined", `{"peer_id":"remote-peer-id","mode":"sideways","observed_at":"2026-08-01T06:00:00Z"}`},
		{"one blob twice", `{"peer_id":"remote-peer-id","mode":"full","observed_at":"2026-08-01T06:00:00Z",` +
			`"entries":[{"blob_hash":"` + someHash + `","state":"present"},` +
			`{"blob_hash":"` + someHash + `","state":"missing"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body, _, err := peerSend(t, c, http.MethodPost, l.inventoryURL(), tc.body)
			if err != nil {
				t.Fatalf("the request did not complete: %v", err)
			}
			if status != http.StatusBadRequest {
				t.Fatalf("got %d, want 400\n%s", status, body)
			}
		})
	}
	if calls, _, _ := sink.snapshot(); calls != 0 {
		t.Errorf("the control plane was called %d times on malformed reports", calls)
	}
}

// TestANodeWithNoCatalogRefusesInventoryRatherThanAcceptingSilently. The route
// is mounted whether or not there is a sink — the OpenAPI parity test walks
// this router, and an unmounted route would be documented and unserved — so
// the refusal has to be a real answer rather than a 404.
func TestANodeWithNoCatalogRefusesInventoryRatherThanAcceptingSilently(t *testing.T) {
	controller := newPeerNode(t, "controller-id", "controller")
	remote := newPeerNode(t, "remote-peer-id", "remote-peer")
	root := newTrustRoot(controller.member(), remote.member())
	l := serve(t, controller, root) // no sink
	c := dialler(t, remote, root)

	status, body, _, err := peerSend(t, c, http.MethodPost, l.inventoryURL(), reportBody(t, remote.peerID))
	if err != nil {
		t.Fatalf("the request did not complete: %v", err)
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("a node with no catalog answered %d, want 503 — a 200 would tell the peer "+
			"its inventory landed when nothing happened\n%s", status, body)
	}
}

// TestAnUnenrolledPeerCannotReportInventory: membership is the only trust root
// in this path, and it is consulted per request, not per connection (M4-04).
func TestAnUnenrolledPeerCannotReportInventory(t *testing.T) {
	controller := newPeerNode(t, "controller-id", "controller")
	remote := newPeerNode(t, "remote-peer-id", "remote-peer")
	root := newTrustRoot(controller.member(), remote.member())
	sink := &recordingSink{}
	l := serveWithSink(t, controller, root, sink)
	c := dialler(t, remote, root)

	// A first report proves the connection works, so the refusal below cannot
	// be "the listener was never reachable".
	if status, body, _, err := peerSend(t, c, http.MethodPost, l.inventoryURL(),
		reportBody(t, remote.peerID)); err != nil || status != http.StatusOK {
		t.Fatalf("the enrolled peer could not report: %d %v\n%s", status, err, body)
	}

	// Revocation is deletion; there is no CRL (ADR-0012).
	root.remove(remote.pub)

	status, body, reused, err := peerSend(t, c, http.MethodPost, l.inventoryURL(), reportBody(t, remote.peerID))
	if err != nil {
		// A severed connection is also a refusal.
		return
	}
	if status != http.StatusForbidden {
		t.Fatalf("a removed peer reported inventory and got %d (connection reused: %v), want 403\n%s",
			status, reused, body)
	}
	if calls, _, _ := sink.snapshot(); calls != 1 {
		t.Errorf("the control plane was called %d times, want 1 — the removed peer's report was folded in", calls)
	}
}

// TestASinkRefusalIsTranslatedRatherThanLeaked: a peer that is a member but
// has no catalog row is an operator problem, and saying so is more useful than
// a 500.
func TestASinkRefusalIsTranslatedRatherThanLeaked(t *testing.T) {
	controller := newPeerNode(t, "controller-id", "controller")
	remote := newPeerNode(t, "remote-peer-id", "remote-peer")
	root := newTrustRoot(controller.member(), remote.member())
	sink := &recordingSink{err: fmt.Errorf("catalog: %w: peer-with-no-row", inventory.ErrUnknownPeer)}
	l := serveWithSink(t, controller, root, sink)
	c := dialler(t, remote, root)

	status, body, _, err := peerSend(t, c, http.MethodPost, l.inventoryURL(), reportBody(t, remote.peerID))
	if err != nil {
		t.Fatalf("the request did not complete: %v", err)
	}
	if status != http.StatusForbidden {
		t.Fatalf("got %d, want 403\n%s", status, body)
	}
	if !strings.Contains(body, "no catalog row") {
		t.Errorf("the refusal does not name the disagreement between membership and the catalog:\n%s", body)
	}
}

// TestTheReporterAndTheRouteAgreeOnThePath. inventory.Path is what the peer
// side posts to and this router is what serves it, and they are two constants
// in two packages. A mismatch would not fail to compile — it would 404 at the
// far end of a network, on a deployment nobody is watching.
func TestTheReporterAndTheRouteAgreeOnThePath(t *testing.T) {
	if want := peerapi.Prefix + "/inventory"; inventory.Path != want {
		t.Fatalf("the reporter posts to %q and the peer surface serves %q", inventory.Path, want)
	}
}
