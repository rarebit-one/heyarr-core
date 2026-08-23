package catalog_test

import (
	"context"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/media/capability"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// M5-112 against a real database (§6, §75, ADR-0037).
//
// Three properties, and each one is a way this feature silently stops meaning
// anything:
//
//  1. It NARROWS. A capability that stops passing stops being advertised —
//     without the binary changing and without a restart. An advertisement that
//     can only grow lies after the first driver update, and every test in this
//     file that merely asserted "the capability is there" would keep passing
//     while it did.
//  2. It EXPIRES. A worker that dies stops advertising within the TTL. That is
//     asserted by reading STATE, not by looking for a log line: the deaths that
//     matter — power cut, OOM kill, severed partition — write no log line.
//  3. It is EXACT. `ffmpeg` is a prefix of `ffmpeg.encoder.hevc`, so every
//     comparison here is equality on a whole string.

// capClock is movable, because the expiry assertion is about the passage of
// time and a test that used time.Sleep for it would be asserting on how busy
// the machine is.
type capClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *capClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *capClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type capHarness struct {
	cat    *catalog.Catalog
	clock  *capClock
	events *events.Log
}

func newCapHarness(t *testing.T) *capHarness {
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
	clock := &capClock{t: time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{
		DB: db, Events: log, PeerName: "node-a", PeerSite: "site-a", Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &capHarness{cat: cat, clock: clock, events: log}
}

const capTTL = 10 * time.Minute

// held builds an advertisement's worth of proven capabilities.
func held(names ...string) []capability.Held {
	out := make([]capability.Held, 0, len(names))
	for _, n := range names {
		out = append(out, capability.Held{Name: n, Source: capability.SourceProbe, Detail: "probed"})
	}
	return out
}

func (h *capHarness) advertise(t *testing.T, workerID, peerID, peerName string, names ...string) capability.Change {
	t.Helper()
	change, err := h.cat.AdvertiseCapabilities(context.Background(), capability.Advertisement{
		WorkerID: workerID, PeerID: peerID, PeerName: peerName,
		Held: held(names...), TTL: capTTL,
	})
	if err != nil {
		t.Fatal(err)
	}
	return change
}

// namesFor reads back what one worker currently advertises, live rows only.
func (h *capHarness) namesFor(t *testing.T, workerID string) []string {
	t.Helper()
	fleet, err := h.cat.FleetCapabilities(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range fleet {
		if a.WorkerID == workerID {
			return capability.Names(a.Held)
		}
	}
	return nil
}

// THE narrowing assertion. Advertise two capabilities, then advertise one, and
// watch the stored answer SHRINK.
//
// It reproduces the gained state first on purpose. A test that only asserted
// the end state would pass on an implementation that never stored the AV1
// capability at all — which is a way of passing that proves nothing about
// narrowing, and is exactly the mistake the inventory tests next door record.
func TestAnAdvertisementNarrowsWhenAProbeStopsPassing(t *testing.T) {
	h := newCapHarness(t)

	gained := h.advertise(t, "worker-1", "peer-a", "node-a",
		"ffmpeg", "ffmpeg.encoder.av1.qsv", "ffmpeg.encoder.hevc.qsv")
	if got := h.namesFor(t, "worker-1"); !reflect.DeepEqual(got,
		[]string{"ffmpeg", "ffmpeg.encoder.av1.qsv", "ffmpeg.encoder.hevc.qsv"}) {
		t.Fatalf("the fixture did not establish the wider set: %v", got)
	}
	if !reflect.DeepEqual(gained.Gained, []string{"ffmpeg", "ffmpeg.encoder.av1.qsv", "ffmpeg.encoder.hevc.qsv"}) {
		t.Errorf("first advertisement gained %v", gained.Gained)
	}

	// The device is claimed by another process, or a kernel update broke it.
	// The BINARY has not changed and nothing has restarted.
	h.clock.advance(time.Minute)
	change := h.advertise(t, "worker-1", "peer-a", "node-a",
		"ffmpeg", "ffmpeg.encoder.hevc.qsv")

	if got := h.namesFor(t, "worker-1"); !reflect.DeepEqual(got,
		[]string{"ffmpeg", "ffmpeg.encoder.hevc.qsv"}) {
		t.Errorf("after narrowing the worker advertises %v; ffmpeg.encoder.av1.qsv stopped passing "+
			"and must stop being advertised", got)
	}
	if !reflect.DeepEqual(change.Lost, []string{"ffmpeg.encoder.av1.qsv"}) {
		t.Errorf("the change reported Lost=%v, want [ffmpeg.encoder.av1.qsv]", change.Lost)
	}
	if len(change.Gained) != 0 {
		t.Errorf("nothing was gained, but the change says %v", change.Gained)
	}
}

// Narrowing to nothing is a legitimate advertisement, not a no-op. A worker
// whose accelerator has gone entirely must be routed AROUND, and an
// implementation that treated an empty set as "nothing to say" would leave the
// whole stale advertisement standing.
func TestAnAdvertisementMayNarrowToNothing(t *testing.T) {
	h := newCapHarness(t)
	h.advertise(t, "worker-1", "peer-a", "node-a", "ffmpeg", "ffmpeg.encoder.hevc.qsv")

	change, err := h.cat.AdvertiseCapabilities(context.Background(), capability.Advertisement{
		WorkerID: "worker-1", PeerID: "peer-a", PeerName: "node-a", TTL: capTTL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := h.namesFor(t, "worker-1"); len(got) != 0 {
		t.Errorf("the worker still advertises %v after advertising nothing", got)
	}
	if !reflect.DeepEqual(change.Lost, []string{"ffmpeg", "ffmpeg.encoder.hevc.qsv"}) {
		t.Errorf("Lost=%v", change.Lost)
	}
}

// Binary presence and hardware capability are re-verified on DIFFERENT rules,
// and the asymmetry is tested in both directions (ADR-0023, ADR-0037).
//
// Direction one: the hardware goes, the binary stays. `ffmpeg` survives — it
// was resolved at startup and is not re-resolved — while the encoder that
// stopped passing is dropped.
//
// Direction two is in TestAWorkerThatDiesStopsAdvertisingWithinTheTTL: nothing
// re-resolves the binary, so the ONLY thing that ever removes a binary
// capability is the advertisement expiring with the process that made it.
func TestHardwareNarrowsWhileTheBinaryCapabilitySurvives(t *testing.T) {
	h := newCapHarness(t)
	ctx := context.Background()

	_, err := h.cat.AdvertiseCapabilities(ctx, capability.Advertisement{
		WorkerID: "worker-1", PeerID: "peer-a", PeerName: "node-a", TTL: capTTL,
		Held: []capability.Held{
			{Name: "ffmpeg", Source: capability.SourceBinary, Detail: "resolved at startup"},
			{Name: "ffmpeg.encoder.av1.qsv", Source: capability.SourceProbe, Detail: "probed"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	h.clock.advance(time.Minute)
	// The next beat: the binary half is unchanged because nothing re-resolves
	// it, and the probe half found the device gone.
	_, err = h.cat.AdvertiseCapabilities(ctx, capability.Advertisement{
		WorkerID: "worker-1", PeerID: "peer-a", PeerName: "node-a", TTL: capTTL,
		Held: []capability.Held{
			{Name: "ffmpeg", Source: capability.SourceBinary, Detail: "resolved at startup"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	fleet, err := h.cat.FleetCapabilities(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(fleet) != 1 || len(fleet[0].Held) != 1 {
		t.Fatalf("expected one worker holding one capability, got %+v", fleet)
	}
	if got := fleet[0].Held[0].Name; got != "ffmpeg" {
		t.Errorf("the surviving capability is %q, want %q", got, "ffmpeg")
	}
	if got := fleet[0].Held[0].Source; got != capability.SourceBinary {
		t.Errorf("the surviving capability's source is %q, want %q — the source is what says which "+
			"re-verification rule applies to it", got, capability.SourceBinary)
	}
}

// A worker that dies stops advertising within the TTL, asserted by STATE.
//
// Nothing tidies up here: the advertisement is simply never renewed, which is
// what happens when the process is gone. The deaths that matter write no log
// line and get no chance to run a shutdown hook.
func TestAWorkerThatDiesStopsAdvertisingWithinTheTTL(t *testing.T) {
	h := newCapHarness(t)
	h.advertise(t, "worker-1", "peer-a", "node-a", "ffmpeg", "ffmpeg.encoder.hevc.qsv")

	// Still inside the TTL: honoured.
	h.clock.advance(capTTL - time.Second)
	if got := h.namesFor(t, "worker-1"); len(got) != 2 {
		t.Fatalf("inside the TTL the advertisement must stand, got %v", got)
	}

	// Past it: gone. No sweep has run and no writer has touched the table —
	// the read itself must refuse to honour a stale claim.
	h.clock.advance(2 * time.Second)
	if got := h.namesFor(t, "worker-1"); got != nil {
		t.Errorf("past the TTL the dead worker still advertises %v", got)
	}
	fleet, err := h.cat.FleetCapabilities(context.Background(), "ffmpeg.encoder.hevc.qsv")
	if err != nil {
		t.Fatal(err)
	}
	if len(fleet) != 0 {
		t.Errorf("a stale advertisement is still answering the fleet query: %+v", fleet)
	}
}

// An advertisement expiring exactly now has expired. Rounding the other way
// honours a claim for an instant longer than the worker promised it.
func TestAnAdvertisementExpiringExactlyNowIsNotHonoured(t *testing.T) {
	h := newCapHarness(t)
	h.advertise(t, "worker-1", "peer-a", "node-a", "ffmpeg")
	h.clock.advance(capTTL)
	if got := h.namesFor(t, "worker-1"); got != nil {
		t.Errorf("an advertisement at its exact expiry is still honoured: %v", got)
	}
}

// The fleet view answers "which nodes hold capability X" across MORE THAN ONE
// node — the thing ADR-0023 says is missing and that a fleet of one cannot
// exercise.
//
// Honest scope: these are two workers on two peer identities against one
// database, not two machines. What it proves is that the query groups and
// filters by node; what it does not prove is that a second machine's
// advertisement arrives here at all.
func TestTheFleetViewAnswersAcrossMoreThanOneNode(t *testing.T) {
	h := newCapHarness(t)
	h.advertise(t, "worker-a", "peer-a", "node-a", "ffmpeg", "ffmpeg.encoder.hevc.qsv")
	h.advertise(t, "worker-b", "peer-b", "node-b", "ffmpeg", "ffmpeg.encoder.hevc.qsv",
		"ffmpeg.encoder.av1.qsv")
	h.advertise(t, "worker-c", "peer-c", "node-c", "ffmpeg")

	cases := []struct {
		capability string
		wantNodes  []string
	}{
		{"ffmpeg", []string{"node-a", "node-b", "node-c"}},
		{"ffmpeg.encoder.hevc.qsv", []string{"node-a", "node-b"}},
		{"ffmpeg.encoder.av1.qsv", []string{"node-b"}},
		{"ffmpeg.encoder.vp9", nil},
	}
	for _, tc := range cases {
		t.Run(tc.capability, func(t *testing.T) {
			fleet, err := h.cat.FleetCapabilities(context.Background(), tc.capability)
			if err != nil {
				t.Fatal(err)
			}
			var nodes []string
			for _, a := range fleet {
				nodes = append(nodes, a.PeerName)
			}
			if !reflect.DeepEqual(nodes, tc.wantNodes) {
				t.Errorf("%q is held by %v, want %v", tc.capability, nodes, tc.wantNodes)
			}
		})
	}
}

// The prefix trap, asserted directly. `ffmpeg` is a prefix of
// `ffmpeg.encoder.hevc`, so a LIKE or a substring match in the query would
// answer "which nodes have the binary" with every node that can encode
// anything — and, worse, would answer "which nodes can encode AV1" with a node
// that merely has ffmpeg installed.
func TestTheFleetQueryMatchesTheWholeCapabilityAndNotAPrefix(t *testing.T) {
	h := newCapHarness(t)
	h.advertise(t, "worker-encoder-only", "peer-b", "node-b", "ffmpeg.encoder.hevc.qsv")

	fleet, err := h.cat.FleetCapabilities(context.Background(), "ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	if len(fleet) != 0 {
		t.Errorf("asking for %q matched a worker that only advertises %q: %+v",
			"ffmpeg", "ffmpeg.encoder.hevc.qsv", fleet)
	}

	fleet, err = h.cat.FleetCapabilities(context.Background(), "ffmpeg.encoder")
	if err != nil {
		t.Fatal(err)
	}
	if len(fleet) != 0 {
		t.Errorf("a partial dotted segment matched: %+v", fleet)
	}
}

// A re-run of the same advertisement is free: same rows, no change reported, no
// event (invariant 9). A beat that emitted an event every time it found the
// world unaltered would bury the one that matters.
func TestReAdvertisingTheSameSetChangesNothing(t *testing.T) {
	h := newCapHarness(t)
	h.advertise(t, "worker-1", "peer-a", "node-a", "ffmpeg", "ffmpeg.encoder.hevc.qsv")
	before := h.eventTypes(t)

	h.clock.advance(time.Minute)
	change := h.advertise(t, "worker-1", "peer-a", "node-a", "ffmpeg", "ffmpeg.encoder.hevc.qsv")
	if !change.Empty() {
		t.Errorf("an unchanged advertisement reported %+v", change)
	}
	if after := h.eventTypes(t); len(after) != len(before) {
		t.Errorf("an unchanged advertisement emitted %d new events", len(after)-len(before))
	}

	// The expiry DID move, which is the whole point of re-advertising.
	h.clock.advance(capTTL - time.Minute)
	if got := h.namesFor(t, "worker-1"); len(got) != 2 {
		t.Errorf("the renewal did not extend the advertisement: %v", got)
	}
}

// The narrowing is the transition worth seeing, so it emits — once, with both
// halves in the payload.
func TestANarrowingEmitsOneEvent(t *testing.T) {
	h := newCapHarness(t)
	h.advertise(t, "worker-1", "peer-a", "node-a", "ffmpeg", "ffmpeg.encoder.av1.qsv")
	before := len(h.eventTypes(t))

	h.advertise(t, "worker-1", "peer-a", "node-a", "ffmpeg")
	after := h.eventTypes(t)
	if len(after)-before != 1 {
		t.Fatalf("a narrowing emitted %d events, want exactly 1", len(after)-before)
	}
	if got := after[len(after)-1]; got != events.TypeWorkerCapabilitiesChanged {
		t.Errorf("emitted %q, want %q", got, events.TypeWorkerCapabilitiesChanged)
	}
}

// An advertisement with no TTL is refused rather than stored forever. A row
// that never expires outlives the process that wrote it, which is the one thing
// this table exists to prevent.
func TestAnAdvertisementWithoutATTLIsRefused(t *testing.T) {
	h := newCapHarness(t)
	_, err := h.cat.AdvertiseCapabilities(context.Background(), capability.Advertisement{
		WorkerID: "worker-1", Held: held("ffmpeg"),
	})
	if err == nil {
		t.Fatal("an advertisement with no TTL was accepted")
	}
}

func (h *capHarness) eventTypes(t *testing.T) []string {
	t.Helper()
	evs, err := h.events.Since(context.Background(), 0, nil, 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}
