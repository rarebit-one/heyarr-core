package worker_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/media/capability"
	"github.com/rarebit-one/heyarr-core/internal/worker"
)

// The capability beat (§6, §75, ADR-0037, M5-112).
//
// # What this file asserts and what it deliberately does not
//
// It asserts the JOIN: what the probe found becomes what is advertised, the
// asymmetry between the binary half and the hardware half holds, and the
// advertisement narrows when a probe stops passing. The probe's own exec
// behaviour — real subprocesses, real exit codes, the "listed but not capable"
// fixture — is asserted in internal/media/capability against a fake ffmpeg the
// test binary re-execs as. The database half is asserted in
// internal/persistence/catalog against a real SQLite file.
//
// The recorder here is an in-memory double on purpose. Wiring a real catalog in
// would re-test what the catalog tests already test and would say nothing about
// the beat.

// flakyHardware is a Runner whose answers can be changed between passes, which
// is the whole point: a device claimed by another process or broken by a kernel
// update changes nothing about the binary and everything about what encodes.
type flakyHardware struct {
	mu sync.Mutex
	// listed is what the binary SAYS it has, and it never changes here —
	// because the failure this models does not change it either.
	listed []string
	// failing is the set of encoders that refuse to run.
	failing map[string]bool
	// exercised records every candidate that was actually launched.
	exercised []string
}

func newFlakyHardware(listed ...string) *flakyHardware {
	return &flakyHardware{listed: listed, failing: map[string]bool{}}
}

func (f *flakyHardware) ListEncoders(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.listed...), nil
}

func (f *flakyHardware) Exercise(_ context.Context, c capability.Candidate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exercised = append(f.exercised, c.Encoder)
	if f.failing[c.Encoder] {
		return errors.New("No capable devices found")
	}
	return nil
}

func (f *flakyHardware) breaks(encoder string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing[encoder] = true
}

// recorder captures advertisements and computes the change the same way the
// catalog does, so the beat's own behaviour can be asserted without a database.
type recorder struct {
	mu   sync.Mutex
	last capability.Advertisement
	held map[string]bool
	err  error
	// calls counts passes, so a test can assert the beat actually ran.
	calls int
}

func newRecorder() *recorder { return &recorder{held: map[string]bool{}} }

func (r *recorder) AdvertiseCapabilities(
	_ context.Context, ad capability.Advertisement,
) (capability.Change, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return capability.Change{}, r.err
	}

	want := map[string]bool{}
	var change capability.Change
	for _, h := range ad.Held {
		want[h.Name] = true
		if !r.held[h.Name] {
			change.Gained = append(change.Gained, h.Name)
		}
	}
	for name := range r.held {
		if !want[name] {
			change.Lost = append(change.Lost, name)
		}
	}
	r.held = want
	r.last = ad
	return change, nil
}

func (r *recorder) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return capability.Names(r.last.Held)
}

func (r *recorder) passes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

var qsvCandidates = []capability.Candidate{
	{Codec: "hevc", Accel: capability.AccelQSV, Encoder: "hevc_qsv"},
	{Codec: "av1", Accel: capability.AccelQSV, Encoder: "av1_qsv"},
}

func newBeat(t *testing.T, hw capability.Runner, rec worker.Advertiser) *worker.CapabilityBeat {
	t.Helper()
	beat, err := worker.NewCapabilityBeat(worker.AdvertiserOptions{
		WorkerID:   "worker-1",
		PeerID:     "peer-a",
		PeerName:   "node-a",
		Binary:     worker.BinaryCapabilities([]string{"ffmpeg", "ffprobe"}, time.Now()),
		Runner:     hw,
		Candidates: qsvCandidates,
		Recorder:   rec,
	})
	if err != nil {
		t.Fatal(err)
	}
	return beat
}

// THE narrowing, end to end through the beat: a capability is advertised, its
// probe is then made to fail, and the advertisement is observed to SHRINK.
//
// Nothing about the binary changes between the two passes, and nothing
// restarts. That is the condition the whole re-verification decision exists
// for: a device can be claimed by another process, or broken by a kernel
// update, without touching the binary or its path.
func TestTheAdvertisementNarrowsWhenAProbeStopsPassing(t *testing.T) {
	hw := newFlakyHardware("hevc_qsv", "av1_qsv")
	rec := newRecorder()
	beat := newBeat(t, hw, rec)
	ctx := context.Background()

	if _, err := beat.Advertise(ctx); err != nil {
		t.Fatal(err)
	}
	wide := []string{
		"ffmpeg", "ffmpeg.encoder.av1", "ffmpeg.encoder.av1.qsv",
		"ffmpeg.encoder.hevc", "ffmpeg.encoder.hevc.qsv", "ffprobe",
	}
	if got := rec.names(); !reflect.DeepEqual(got, wide) {
		t.Fatalf("the first pass did not establish the wider set:\n got %v\nwant %v", got, wide)
	}

	// Somebody else took the device. The binary is untouched, and the encoder
	// is STILL LISTED — which is exactly the state that makes list-parsing
	// wrong.
	hw.breaks("av1_qsv")

	if _, err := beat.Advertise(ctx); err != nil {
		t.Fatal(err)
	}
	narrow := []string{"ffmpeg", "ffmpeg.encoder.hevc", "ffmpeg.encoder.hevc.qsv", "ffprobe"}
	if got := rec.names(); !reflect.DeepEqual(got, narrow) {
		t.Errorf("the advertisement did not narrow:\n got %v\nwant %v", got, narrow)
	}

	// The candidate was still exercised on the second pass — a probe that
	// stopped trying after a success would never notice the loss.
	var av1Attempts int
	for _, e := range hw.exercised {
		if e == "av1_qsv" {
			av1Attempts++
		}
	}
	if av1Attempts != 2 {
		t.Errorf("av1_qsv was exercised %d times across two passes, want 2 — a capability that is "+
			"only ever proven once cannot be observed to stop working", av1Attempts)
	}
}

// The asymmetry, in the direction ADR-0023 fixed and ADR-0037 keeps: the binary
// half is a captured startup value and survives a hardware collapse.
func TestTheBinaryHalfSurvivesAHardwareCollapse(t *testing.T) {
	hw := newFlakyHardware("hevc_qsv", "av1_qsv")
	rec := newRecorder()
	beat := newBeat(t, hw, rec)

	if _, err := beat.Advertise(context.Background()); err != nil {
		t.Fatal(err)
	}
	hw.breaks("hevc_qsv")
	hw.breaks("av1_qsv")
	change, err := beat.Advertise(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"ffmpeg", "ffprobe"}
	if got := rec.names(); !reflect.DeepEqual(got, want) {
		t.Errorf("after every probe failed the worker advertises %v, want %v — the binary was "+
			"resolved at startup and nothing here re-resolves it", got, want)
	}
	if len(change.Lost) != 4 {
		t.Errorf("lost %v, want all four encoder capabilities", change.Lost)
	}
	// And the source is what records WHY it survived.
	for _, h := range rec.last.Held {
		if h.Source != capability.SourceBinary {
			t.Errorf("%s survived with source %q, want %q", h.Name, h.Source, capability.SourceBinary)
		}
	}
}

// A node with no ffmpeg has no runner, advertises the binary half alone — which
// may be empty — and that is a legitimate advertisement rather than silence.
//
// A worker that said nothing because it had nothing to say would be
// indistinguishable from one that had died, and the whole expiry mechanism
// depends on that distinction.
func TestANodeWithNoToolchainStillAdvertises(t *testing.T) {
	rec := newRecorder()
	beat, err := worker.NewCapabilityBeat(worker.AdvertiserOptions{
		WorkerID: "worker-bare", Recorder: rec,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := beat.Advertise(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec.passes() != 1 {
		t.Fatalf("a bare worker made %d advertisements, want 1", rec.passes())
	}
	if got := rec.names(); len(got) != 0 {
		t.Errorf("a bare worker advertised %v", got)
	}
	if rec.last.TTL <= 0 {
		t.Error("an advertisement with no TTL never expires, which outlives the worker that made it")
	}
	if rec.last.WorkerID != "worker-bare" {
		t.Errorf("the advertisement names %q", rec.last.WorkerID)
	}
}

// The TTL must be comfortably longer than the interval, or a pass that ran late
// expires a perfectly healthy worker and routes work around it.
func TestTheTTLOutlastsSeveralMissedBeats(t *testing.T) {
	if worker.AdvertiseTTL <= worker.AdvertiseInterval {
		t.Fatalf("TTL %s does not outlast the interval %s", worker.AdvertiseTTL, worker.AdvertiseInterval)
	}
	if worker.AdvertiseTTL < 3*worker.AdvertiseInterval {
		t.Errorf("TTL %s is less than three intervals (%s); one late pass would expire a healthy worker",
			worker.AdvertiseTTL, worker.AdvertiseInterval)
	}
}

// A failed recording is a failed pass. A beat that exercised the hardware and
// then swallowed the write error would leave a stale advertisement standing and
// report success for it.
func TestAFailedRecordingIsAFailedPass(t *testing.T) {
	rec := newRecorder()
	rec.err = errors.New("the database is unreachable")
	beat := newBeat(t, newFlakyHardware("hevc_qsv"), rec)

	if _, err := beat.Advertise(context.Background()); err == nil {
		t.Fatal("a pass that could not record its answer reported success")
	}
}

// Run advertises immediately rather than waiting out the first interval: a
// restart is exactly when the fleet needs to know what this node can do.
func TestRunAdvertisesAtStartupAndThenOnTheBeat(t *testing.T) {
	rec := newRecorder()
	beat, err := worker.NewCapabilityBeat(worker.AdvertiserOptions{
		WorkerID: "worker-1", Recorder: rec,
		Runner: newFlakyHardware("hevc_qsv"), Candidates: qsvCandidates,
		Interval: 5 * time.Millisecond,
		TTL:      time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); beat.Run(ctx) }()

	// Poll for the condition rather than sleeping a fixed duration: a fixed
	// wait is a bet on how busy the machine is, and every one of those bets in
	// this repo has eventually lost on CI.
	deadline := time.Now().Add(2 * time.Second)
	for rec.passes() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done

	if got := rec.passes(); got < 3 {
		t.Errorf("the beat made %d passes in two seconds at a 5ms interval", got)
	}
}

// The beat refuses to be built without the two things it cannot do its job
// without, rather than running and quietly recording nothing.
func TestTheBeatRefusesToBeBuiltIncomplete(t *testing.T) {
	if _, err := worker.NewCapabilityBeat(worker.AdvertiserOptions{Recorder: newRecorder()}); err == nil {
		t.Error("a beat with no worker id was built")
	}
	if _, err := worker.NewCapabilityBeat(worker.AdvertiserOptions{WorkerID: "w"}); err == nil {
		t.Error("a beat with no recorder was built; its advertisement would be a local variable")
	}
}
