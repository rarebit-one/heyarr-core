package inventory_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/peer/inventory"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// The peer half: what this node HOLDS, derived from the store.
//
// The one thing every test here is guarding is that the inventory comes from
// disk. A collector that read the peer's own catalog would report the
// controller's beliefs back to the controller, and the whole exchange would
// confirm nothing it did not already assume — which is the failure this issue
// exists to prevent, and which is invisible in any test where the two agree.

var observed = time.Date(2026, 8, 1, 6, 0, 0, 0, time.UTC)

func at(t time.Time) func() time.Time { return func() time.Time { return t } }

func newStore(t *testing.T) *cas.FS {
	t.Helper()
	store, err := cas.OpenFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func put(t *testing.T, store *cas.FS, content string) hashing.Hash {
	t.Helper()
	d, err := store.Put(context.Background(), strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return d.Hash
}

func collect(t *testing.T, store *cas.FS, now time.Time) inventory.Snapshot {
	t.Helper()
	snap, err := inventory.Collect(context.Background(), inventory.Options{
		Store: store, Quarantine: store, Now: at(now),
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func entryFor(t *testing.T, snap inventory.Snapshot, h hashing.Hash) inventory.Entry {
	t.Helper()
	for _, e := range snap.Entries() {
		if e.BlobHash == h.String() {
			return e
		}
	}
	t.Fatalf("the inventory does not mention %s", h)
	return inventory.Entry{}
}

// ---------------------------------------------------------------------------
// the inventory is the disk

func TestCollectReportsWhatIsOnDisk(t *testing.T) {
	store := newStore(t)
	a := put(t, store, "one")
	b := put(t, store, "two, which is longer")

	snap := collect(t, store, observed)
	if snap.Len() != 2 {
		t.Fatalf("the inventory holds %d blobs, want 2: %v", snap.Len(), snap.Entries())
	}
	if snap.ObservedAt != observed {
		t.Errorf("observed_at = %s, want %s", snap.ObservedAt, observed)
	}
	for _, h := range []hashing.Hash{a, b} {
		e := entryFor(t, snap, h)
		if e.State != inventory.StatePresent {
			t.Errorf("%s: state = %s, want present", h, e.State)
		}
		if e.VerifiedAt != nil {
			// Collecting reads a directory; it does not re-hash a library. A
			// collector that stamped the collection time here would
			// manufacture verification evidence out of a listing.
			t.Errorf("%s: the collector invented a verification time (%s) from a directory listing",
				h, e.VerifiedAt)
		}
	}
	if got := entryFor(t, snap, b).BytesPresent; got != int64(len("two, which is longer")) {
		t.Errorf("bytes_present = %d, want %d", got, len("two, which is longer"))
	}
}

func TestCollectReportsAnEmptyStoreAsEmpty(t *testing.T) {
	snap := collect(t, newStore(t), observed)
	if snap.Len() != 0 {
		t.Fatalf("an empty store reported %d blobs", snap.Len())
	}
	// A wiped peer must be able to say so, and a full report of nothing is how
	// it does. An implementation that read the empty set as "nothing to
	// report" would let it keep its replicas forever.
	rep := snap.Full("peer-1")
	if rep.Mode != inventory.ModeFull {
		t.Errorf("mode = %q, want full", rep.Mode)
	}
	if err := rep.Validate(); err != nil {
		t.Errorf("a full report of an empty store is invalid: %v", err)
	}
}

func TestCollectStopsHoldingABlobTheStoreNoLongerHas(t *testing.T) {
	store := newStore(t)
	kept := put(t, store, "kept")
	lost := put(t, store, "lost")
	if before := collect(t, store, observed); before.Len() != 2 {
		t.Fatalf("the store did not start with two blobs")
	}

	if err := store.Delete(context.Background(), lost); err != nil {
		t.Fatal(err)
	}

	after := collect(t, store, observed.Add(time.Hour))
	if after.Len() != 1 {
		t.Fatalf("after deleting one blob the inventory holds %d: %v", after.Len(), after.Entries())
	}
	if entryFor(t, after, kept).State != inventory.StatePresent {
		t.Error("the blob that stayed is not present")
	}
}

func TestCollectReportsAQuarantinedBlobAsCorrupt(t *testing.T) {
	store := newStore(t)
	healthy := put(t, store, "bytes that still hash to their own name")
	rotten := put(t, store, "bytes about to be rewritten underneath us")

	// The way it actually happens: an external tool rewrites the file through
	// a shared inode (#43), and Verify quarantines it rather than deleting it
	// (ADR-0018).
	path, err := store.LocalPath(context.Background(), rotten)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("something else"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Verify(context.Background(), rotten); err == nil {
		t.Fatal("Verify accepted rewritten bytes; nothing was quarantined and this test proves nothing")
	}

	snap := collect(t, store, observed)
	got := entryFor(t, snap, rotten)
	switch got.State {
	case inventory.StateCorrupt:
	case inventory.StatePresent:
		t.Errorf("a quarantined blob was reported present; the controller would believe in a copy "+
			"nothing can read (got %+v)", got)
	default:
		t.Errorf("state = %s, want corrupt", got.State)
	}
	if got.BytesPresent != 0 {
		t.Errorf("a corrupt entry claims %d servable bytes", got.BytesPresent)
	}
	if entryFor(t, snap, healthy).State != inventory.StatePresent {
		t.Error("the healthy blob stopped being present")
	}
}

// TestCollectWithoutAQuarantineSourceOmitsCorruptRatherThanGuessing. A store
// that cannot tell must not answer "nothing is corrupt", and the way it says
// so is by not implementing the interface — which is why Quarantine is a
// separate optional field rather than part of Store.
func TestCollectWithoutAQuarantineSourceReportsOnlyWhatIsAddressable(t *testing.T) {
	store := newStore(t)
	rotten := put(t, store, "bytes about to be rewritten")
	path, err := store.LocalPath(context.Background(), rotten)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("something else"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Verify(context.Background(), rotten); err == nil {
		t.Fatal("Verify accepted rewritten bytes")
	}

	snap, err := inventory.Collect(context.Background(), inventory.Options{Store: store, Now: at(observed)})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Len() != 0 {
		t.Fatalf("a collector with no quarantine source reported %d blobs: %v", snap.Len(), snap.Entries())
	}
}

func TestCollectCarriesAVerificationTimeWhenThePeerHasOne(t *testing.T) {
	store := newStore(t)
	a := put(t, store, "verified once")
	when := observed.Add(-time.Hour)

	snap, err := inventory.Collect(context.Background(), inventory.Options{
		Store: store, Quarantine: store, Now: at(observed),
		Verified: func(h hashing.Hash) (time.Time, bool) { return when, h.Equal(a) },
	})
	if err != nil {
		t.Fatal(err)
	}
	got := entryFor(t, snap, a)
	if got.VerifiedAt == nil || !got.VerifiedAt.Equal(when) {
		t.Errorf("verified_at = %v, want %s", got.VerifiedAt, when)
	}
}

func TestCollectRefusesWithoutAStore(t *testing.T) {
	if _, err := inventory.Collect(context.Background(), inventory.Options{}); err == nil {
		t.Fatal("Collect accepted no store; an inventory with nothing to read is not an empty inventory")
	}
}

// ---------------------------------------------------------------------------
// the diff

func TestSinceCarriesOnlyWhatChanged(t *testing.T) {
	a, b, c := "blake3:"+strings.Repeat("a", 64), "blake3:"+strings.Repeat("b", 64), "blake3:"+strings.Repeat("c", 64)
	before, err := inventory.NewSnapshot(observed, []inventory.Entry{
		{BlobHash: a, State: inventory.StatePresent, BytesPresent: 1},
		{BlobHash: b, State: inventory.StatePresent, BytesPresent: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := inventory.NewSnapshot(observed.Add(time.Hour), []inventory.Entry{
		{BlobHash: b, State: inventory.StatePresent, BytesPresent: 2},
		{BlobHash: c, State: inventory.StatePresent, BytesPresent: 3},
	})
	if err != nil {
		t.Fatal(err)
	}

	rep := after.Since(before, "peer-1")
	if rep.Mode != inventory.ModeIncremental {
		t.Fatalf("mode = %q", rep.Mode)
	}
	if len(rep.Entries) != 2 {
		t.Fatalf("the diff carries %d entries, want 2 (a gone, c arrived): %+v", len(rep.Entries), rep.Entries)
	}
	byHash := map[string]inventory.Entry{}
	for _, e := range rep.Entries {
		byHash[e.BlobHash] = e
	}
	// The half that makes `replicas` able to shrink: the blob that went away
	// is CARRIED, as missing, not simply omitted.
	if got, ok := byHash[a]; !ok || got.State != inventory.StateMissing {
		t.Errorf("the blob that went away is %+v, want an explicit missing entry", got)
	}
	if got, ok := byHash[c]; !ok || got.State != inventory.StatePresent {
		t.Errorf("the blob that arrived is %+v, want present", got)
	}
	if _, mentioned := byHash[b]; mentioned {
		t.Error("the diff mentions a blob that did not change, so it is not a diff")
	}
}

func TestSinceOfAnUnchangedSnapshotIsEmpty(t *testing.T) {
	a := "blake3:" + strings.Repeat("a", 64)
	one, err := inventory.NewSnapshot(observed, []inventory.Entry{
		{BlobHash: a, State: inventory.StatePresent, BytesPresent: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	two, err := inventory.NewSnapshot(observed.Add(time.Hour), []inventory.Entry{
		{BlobHash: a, State: inventory.StatePresent, BytesPresent: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep := two.Since(one, "peer-1"); len(rep.Entries) != 0 {
		t.Errorf("a cycle in which nothing happened carries %d entries: %+v", len(rep.Entries), rep.Entries)
	}
}

func TestSinceDoesNotRepeatALossItAlreadyReported(t *testing.T) {
	a := "blake3:" + strings.Repeat("a", 64)
	gone, err := inventory.NewSnapshot(observed, []inventory.Entry{
		{BlobHash: a, State: inventory.StateMissing},
	})
	if err != nil {
		t.Fatal(err)
	}
	stillGone, err := inventory.NewSnapshot(observed.Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep := stillGone.Since(gone, "peer-1"); len(rep.Entries) != 0 {
		t.Errorf("a loss already reported was reported again: %+v", rep.Entries)
	}
}

// ---------------------------------------------------------------------------
// validation

func TestValidateRefusesWhatWouldBeAmbiguous(t *testing.T) {
	a := "blake3:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name string
		rep  inventory.Report
	}{
		{"no mode", inventory.Report{PeerID: "p", ObservedAt: observed}},
		{"no observation time", inventory.Report{PeerID: "p", Mode: inventory.ModeFull}},
		{"an unparseable hash", inventory.Report{
			PeerID: "p", Mode: inventory.ModeFull, ObservedAt: observed,
			Entries: []inventory.Entry{{BlobHash: "nope", State: inventory.StatePresent}},
		}},
		{"a state a peer may not report", inventory.Report{
			PeerID: "p", Mode: inventory.ModeFull, ObservedAt: observed,
			Entries: []inventory.Entry{{BlobHash: a, State: "pending"}},
		}},
		{"negative bytes", inventory.Report{
			PeerID: "p", Mode: inventory.ModeFull, ObservedAt: observed,
			Entries: []inventory.Entry{{BlobHash: a, State: inventory.StatePresent, BytesPresent: -1}},
		}},
		{"one blob twice", inventory.Report{
			PeerID: "p", Mode: inventory.ModeFull, ObservedAt: observed,
			Entries: []inventory.Entry{
				{BlobHash: a, State: inventory.StatePresent},
				{BlobHash: a, State: inventory.StateMissing},
			},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.rep.Validate(); err == nil {
				t.Fatal("Validate accepted it")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// the reporter

// stubController is a controller's peer surface, without the mTLS. The
// transport is M4-05's and is tested there; what this asserts is the
// reporter's own decisions — which shape it sends, and when.
type stubController struct {
	received []inventory.Report
	peerID   string
}

func (s *stubController) serve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(inventory.Path, func(w http.ResponseWriter, r *http.Request) {
		var rep inventory.Report
		if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.received = append(s.received, rep)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(inventory.Outcome{
			ReportID: "r", PeerID: s.peerID, Mode: rep.Mode, Entries: len(rep.Entries),
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func newReporter(t *testing.T, url, peerID string, fullEvery int, collect func(context.Context) (inventory.Snapshot, error)) *inventory.Reporter {
	t.Helper()
	r, err := inventory.NewReporter(inventory.ReporterOptions{
		// httptest's client is not a pinned peer client, and the reporter
		// cannot tell — pinning is asserted in internal/peer/mtls, where the
		// transport lives. What matters here is that a client is REQUIRED.
		Client:        &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13}}},
		ControllerURL: url,
		PeerID:        peerID,
		FullEvery:     fullEvery,
		Collect:       collect,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestTheFirstCycleIsAlwaysFullAndLaterOnesAreDiffs(t *testing.T) {
	a, b := "blake3:"+strings.Repeat("a", 64), "blake3:"+strings.Repeat("b", 64)
	snaps := []inventory.Snapshot{}
	for _, entries := range [][]inventory.Entry{
		{{BlobHash: a, State: inventory.StatePresent, BytesPresent: 1}},
		{
			{BlobHash: a, State: inventory.StatePresent, BytesPresent: 1},
			{BlobHash: b, State: inventory.StatePresent, BytesPresent: 2},
		},
	} {
		snap, err := inventory.NewSnapshot(observed, entries)
		if err != nil {
			t.Fatal(err)
		}
		snaps = append(snaps, snap)
	}

	ctrl := &stubController{peerID: "peer-1"}
	ts := ctrl.serve(t)
	var cycle int
	r := newReporter(t, ts.URL, "peer-1", 100, func(context.Context) (inventory.Snapshot, error) {
		s := snaps[cycle]
		cycle++
		return s, nil
	})

	for range 2 {
		if _, err := r.Cycle(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(ctrl.received) != 2 {
		t.Fatalf("the controller received %d reports, want 2", len(ctrl.received))
	}
	// The first cycle has nothing to diff against, and an incremental report
	// from a peer the controller has never heard from would leave every blob
	// it holds unmentioned and therefore unconfirmed.
	if ctrl.received[0].Mode != inventory.ModeFull {
		t.Errorf("the first report was %q, want full", ctrl.received[0].Mode)
	}
	if ctrl.received[1].Mode != inventory.ModeIncremental {
		t.Errorf("the second report was %q, want incremental", ctrl.received[1].Mode)
	}
	if len(ctrl.received[1].Entries) != 1 || ctrl.received[1].Entries[0].BlobHash != b {
		t.Errorf("the second report carries %+v, want only the blob that arrived", ctrl.received[1].Entries)
	}
}

// TestAFullReportComesRoundAgain is the drift corrector. Incremental reports
// are correct only if every previous one arrived and was applied; the periodic
// full report is the only shape that can say "and nothing else".
func TestAFullReportComesRoundAgain(t *testing.T) {
	a := "blake3:" + strings.Repeat("a", 64)
	snap, err := inventory.NewSnapshot(observed, []inventory.Entry{
		{BlobHash: a, State: inventory.StatePresent, BytesPresent: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctrl := &stubController{peerID: "peer-1"}
	ts := ctrl.serve(t)
	r := newReporter(t, ts.URL, "peer-1", 3, func(context.Context) (inventory.Snapshot, error) {
		return snap, nil
	})
	for range 7 {
		if _, err := r.Cycle(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var modes []inventory.Mode
	for _, rep := range ctrl.received {
		modes = append(modes, rep.Mode)
	}
	want := []inventory.Mode{
		inventory.ModeFull, inventory.ModeIncremental, inventory.ModeIncremental,
		inventory.ModeFull, inventory.ModeIncremental, inventory.ModeIncremental,
		inventory.ModeFull,
	}
	if len(modes) != len(want) {
		t.Fatalf("got %d reports, want %d", len(modes), len(want))
	}
	for i := range want {
		if modes[i] != want[i] {
			t.Fatalf("report modes = %v, want %v", modes, want)
		}
	}
}

// TestAFailedReportIsNotTreatedAsApplied. A report that did not reach the
// controller must not become the baseline: the next diff would be computed
// against a state the controller never reached, and everything in between
// would never be reported again.
func TestAFailedReportIsNotTreatedAsApplied(t *testing.T) {
	a, b := "blake3:"+strings.Repeat("a", 64), "blake3:"+strings.Repeat("b", 64)
	first, err := inventory.NewSnapshot(observed, []inventory.Entry{
		{BlobHash: a, State: inventory.StatePresent, BytesPresent: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := inventory.NewSnapshot(observed.Add(time.Hour), []inventory.Entry{
		{BlobHash: a, State: inventory.StatePresent, BytesPresent: 1},
		{BlobHash: b, State: inventory.StatePresent, BytesPresent: 2},
	})
	if err != nil {
		t.Fatal(err)
	}

	var refuse bool
	var received []inventory.Report
	mux := http.NewServeMux()
	mux.HandleFunc(inventory.Path, func(w http.ResponseWriter, r *http.Request) {
		if refuse {
			http.Error(w, "no", http.StatusInternalServerError)
			return
		}
		var rep inventory.Report
		if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received = append(received, rep)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(inventory.Outcome{PeerID: "peer-1", Mode: rep.Mode})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	snaps := []inventory.Snapshot{first, second, second}
	var cycle int
	r := newReporter(t, ts.URL, "peer-1", 100, func(context.Context) (inventory.Snapshot, error) {
		s := snaps[cycle]
		cycle++
		return s, nil
	})

	if _, err := r.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	refuse = true
	if _, err := r.Cycle(context.Background()); err == nil {
		t.Fatal("a refused report was reported as a success")
	}
	refuse = false
	if _, err := r.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(received) != 2 {
		t.Fatalf("the controller received %d reports, want 2", len(received))
	}
	// The retry must still carry `b`. If the failed cycle had been recorded as
	// applied, the diff would be against `second` and `b` would be lost
	// forever.
	if len(received[1].Entries) != 1 || received[1].Entries[0].BlobHash != b {
		t.Errorf("the report after the failure carries %+v, want the blob the failed cycle was about",
			received[1].Entries)
	}
}

// TestAnOutcomeNamingAnotherPeerIsAFailure: the controller records against the
// peer the certificate proved, so an outcome naming somebody else means this
// node's identity and its configuration disagree — and its inventory just
// landed somewhere it did not mean.
func TestAnOutcomeNamingAnotherPeerIsAFailure(t *testing.T) {
	snap, err := inventory.NewSnapshot(observed, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctrl := &stubController{peerID: "somebody-else"}
	ts := ctrl.serve(t)
	r := newReporter(t, ts.URL, "peer-1", 1, func(context.Context) (inventory.Snapshot, error) {
		return snap, nil
	})
	if _, err := r.Cycle(context.Background()); err == nil {
		t.Fatal("an outcome recorded against another peer was accepted")
	}
}

func TestAReporterRefusesToBeBuiltWithoutAPinnedClient(t *testing.T) {
	_, err := inventory.NewReporter(inventory.ReporterOptions{
		ControllerURL: "https://controller", PeerID: "peer-1",
		Collect: func(context.Context) (inventory.Snapshot, error) { return inventory.Snapshot{}, nil },
	})
	if err == nil {
		t.Fatal("a reporter was built with no client; an unpinned transport would offer this " +
			"node's whole inventory to whoever answered")
	}
}
