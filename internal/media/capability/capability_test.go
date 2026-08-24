package capability_test

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/media/capability"
)

// M5-112's core assertion, and the one the whole issue turns on: a capability
// is proven by EXERCISING it, never by parsing a list.
//
// # Why there is a fake ffmpeg in this file rather than a t.Skip
//
// There is no ffmpeg on the development machines or on the macOS runners, and
// the machines that DO have it do not have the hardware whose behaviour this
// package exists to survive — silicon that lists an AV1 encoder it cannot run.
// So the interesting configuration is not reachable by installing anything.
//
// A `t.Skip` would leave this package printing `ok` having asserted nothing,
// which is the failure that cost this repo hours three times in Milestone 4
// (#157, #164, #149). Instead the test binary re-execs ITSELF as a fake ffmpeg
// (see TestMain): a real subprocess, receiving the real argv the production
// code built, exiting with real status codes and writing real stderr. Every
// assertion below runs on every machine, with nothing skipped.
//
// What that does NOT prove is stated plainly in the ADR and in the PR: no test
// here has encoded a frame on real silicon, and the fixture's "lists av1_qsv,
// fails with No capable devices found" is a reproduction of a measurement taken
// elsewhere, not a re-measurement.

// fakeFFmpegEnv switches the test binary into fake-ffmpeg mode, and names the
// fixture it should behave as.
const fakeFFmpegEnv = "HEYARR_FAKE_FFMPEG_FIXTURE"

// subprocesses counts fake-ffmpeg executions across the whole package run. It
// is asserted at the end: a package whose exec-level tests all quietly did
// nothing must fail rather than pass.
var subprocesses atomic.Int64

func TestMain(m *testing.M) {
	// Before the testing flags are parsed, because the argv this receives is
	// FFmpeg's, not `go test`'s. The parent sets the env with t.Setenv and the
	// child inherits it.
	if fixture := os.Getenv(fakeFFmpegEnv); fixture != "" {
		os.Exit(fakeFFmpeg(fixture, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeFFmpeg is the fixture, as a program.
//
// It answers `-encoders` with a list, and answers an encode by exiting 0 or by
// exiting 1 with FFmpeg's actual words. The asymmetry between what it LISTS and
// what it RUNS is the entire point.
func fakeFFmpeg(fixture string, args []string) int {
	if contains(args, "-encoders") {
		switch fixture {
		case fixtureUnreadableList:
			fmt.Fprintln(os.Stderr, "Unrecognized option '-encoders'.")
			return 1
		default:
			fmt.Print(encoderListing)
			return 0
		}
	}

	encoder := flagValue(args, "-c:v")
	switch {
	case fixture != fixtureEverythingWorks && strings.HasPrefix(encoder, "av1_"):
		// The measurement this package exists because of. The device lists the
		// encoder and refuses the work.
		fmt.Fprintln(os.Stderr, "[av1_qsv @ 0x0] No capable devices found")
		return 1
	case encoder == "":
		fmt.Fprintln(os.Stderr, "no -c:v was given; this is not a probe")
		return 2
	default:
		return 0
	}
}

const (
	// fixtureListsAV1ItCannotRun is heterogeneous hardware as measured: an AV1
	// hardware encoder is listed, and every attempt to encode with it fails.
	fixtureListsAV1ItCannotRun = "lists-av1-it-cannot-run"
	// fixtureEverythingWorks is the machine where the listing happens to be true.
	fixtureEverythingWorks = "everything-works"
	// fixtureUnreadableList is a build whose `-encoders` we cannot read.
	fixtureUnreadableList = "unreadable-list"
)

// encoderListing is `ffmpeg -encoders` output, in the real format.
const encoderListing = `Encoders:
 V..... = Video
 ------
 V....D av1_qsv              AV1 (Intel Quick Sync Video acceleration) (codec av1)
 V....D hevc_qsv             HEVC (Intel Quick Sync Video acceleration) (codec hevc)
 V..... libx264              libx264 H.264 / AVC / MPEG-4 AVC / MPEG-4 part 10 (codec h264)
 A..... aac                  AAC (Advanced Audio Coding)
`

// candidates is the slice under test: one AV1 hardware encoder, one HEVC
// hardware encoder, one software H.264 encoder, and one encoder the binary does
// not list at all.
var candidates = []capability.Candidate{
	{Codec: "av1", Accel: capability.AccelQSV, Encoder: "av1_qsv"},
	{Codec: "hevc", Accel: capability.AccelQSV, Encoder: "hevc_qsv"},
	{Codec: "h264", Accel: capability.AccelSoftware, Encoder: "libx264"},
	{Codec: "av1", Accel: capability.AccelNVENC, Encoder: "av1_nvenc"},
}

// runner builds an ExecRunner pointed at this test binary, behaving as the
// named fixture. Every call it makes is a real process.
func runner(t *testing.T, fixture string) *capability.ExecRunner {
	t.Helper()
	t.Setenv(fakeFFmpegEnv, fixture)
	r, err := capability.NewExecRunner(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// counted wraps a Runner and tallies both calls, so a test can assert that a
// candidate the binary never listed was never exercised.
type counted struct {
	inner     capability.Runner
	lists     atomic.Int64
	exercised []string
}

func (c *counted) ListEncoders(ctx context.Context) ([]string, error) {
	c.lists.Add(1)
	return c.inner.ListEncoders(ctx)
}

func (c *counted) Exercise(ctx context.Context, cand capability.Candidate) error {
	c.exercised = append(c.exercised, cand.Encoder)
	subprocesses.Add(1)
	return c.inner.Exercise(ctx, cand)
}

// THE assertion. A real subprocess lists av1_qsv and then refuses to encode
// with it. What is advertised must be exactly what encoded.
//
// Sabotage that makes Probe advertise the listed set instead of the exercised
// one turns this red: av1_qsv is listed, so a list-parsing implementation
// advertises ffmpeg.encoder.av1.qsv and ffmpeg.encoder.av1.
func TestAnEncoderThatIsListedButCannotEncodeIsNotAdvertised(t *testing.T) {
	c := &counted{inner: runner(t, fixtureListsAV1ItCannotRun)}
	held := capability.Probe(context.Background(), c, candidates, nil, nil)

	want := []string{
		"ffmpeg.encoder.h264",
		"ffmpeg.encoder.hevc",
		"ffmpeg.encoder.hevc.qsv",
	}
	assertNames(t, held, want)

	// Named explicitly, because the generic comparison above would also pass if
	// the whole set collapsed to empty for an unrelated reason.
	for _, forbidden := range []string{"ffmpeg.encoder.av1", "ffmpeg.encoder.av1.qsv"} {
		if hasExactly(held, forbidden) {
			t.Errorf("%q is advertised; the binary listed av1_qsv and then failed to encode with it — "+
				"advertisement must follow the EXERCISE, not the listing", forbidden)
		}
	}

	// h264's software encoder produces the rollup name directly, so the rollup
	// must not have created a duplicate.
	if got := len(held); got != len(want) {
		t.Errorf("held %d capabilities, want %d: %v", got, len(want), capability.Names(held))
	}
}

// The listing is allowed to make the probe do LESS work and nothing else. A
// candidate the binary never mentions is not exercised — and one it does
// mention is exercised even when it will fail.
func TestTheEncoderListingOnlyEverSkipsWork(t *testing.T) {
	c := &counted{inner: runner(t, fixtureListsAV1ItCannotRun)}
	capability.Probe(context.Background(), c, candidates, nil, nil)

	want := []string{"av1_qsv", "hevc_qsv", "libx264"}
	if !reflect.DeepEqual(c.exercised, want) {
		t.Errorf("exercised %v, want %v — av1_nvenc is not in the listing and must not be launched, "+
			"and av1_qsv IS in it and must be, precisely so its failure can be observed", c.exercised, want)
	}
	if got := c.lists.Load(); got != 1 {
		t.Errorf("listed encoders %d times, want exactly 1", got)
	}
}

// A build whose `-encoders` cannot be read must fall back to trying everything,
// not to advertising nothing. Treating an unreadable list as an empty one would
// silently disarm the feature on any FFmpeg whose output we cannot parse.
func TestAnUnreadableEncoderListingExercisesEveryCandidate(t *testing.T) {
	c := &counted{inner: runner(t, fixtureUnreadableList)}
	held := capability.Probe(context.Background(), c, candidates, nil, nil)

	if len(c.exercised) != len(candidates) {
		t.Errorf("exercised %v, want all %d candidates", c.exercised, len(candidates))
	}
	// The silicon is the same silicon: it refuses every av1_* encode whether or
	// not its encoder listing could be read. So the answer is still honest.
	assertNames(t, held, []string{
		"ffmpeg.encoder.h264",
		"ffmpeg.encoder.hevc",
		"ffmpeg.encoder.hevc.qsv",
	})
}

// On the machine where the listing happens to be true, everything listed is
// advertised — so the test above is not passing because the probe advertises
// nothing.
func TestOnCapableSiliconEverythingExercisedIsAdvertised(t *testing.T) {
	c := &counted{inner: runner(t, fixtureEverythingWorks)}
	held := capability.Probe(context.Background(), c, candidates, nil, nil)

	assertNames(t, held, []string{
		"ffmpeg.encoder.av1",
		"ffmpeg.encoder.av1.qsv",
		"ffmpeg.encoder.h264",
		"ffmpeg.encoder.hevc",
		"ffmpeg.encoder.hevc.qsv",
	})
}

// The rollup is derived from leaves that passed, and from nothing else.
func TestTheCodecRollupFollowsOnlyProvenLeaves(t *testing.T) {
	c := &counted{inner: runner(t, fixtureListsAV1ItCannotRun)}
	held := capability.Probe(context.Background(), c, candidates, nil, nil)

	if !hasExactly(held, "ffmpeg.encoder.hevc") {
		t.Error("hevc_qsv encoded, so ffmpeg.encoder.hevc must roll up")
	}
	if hasExactly(held, "ffmpeg.encoder.av1") {
		t.Error("no AV1 encoder encoded, so ffmpeg.encoder.av1 must not roll up")
	}
}

// The exec layer, asserted directly: a failing encode is an error carrying
// FFmpeg's own last words, and a succeeding one is not.
func TestTheExecRunnerReportsWhatTheProcessDid(t *testing.T) {
	r := runner(t, fixtureListsAV1ItCannotRun)
	ctx := context.Background()

	subprocesses.Add(1)
	err := r.Exercise(ctx, capability.Candidate{Codec: "av1", Accel: capability.AccelQSV, Encoder: "av1_qsv"})
	if err == nil {
		t.Fatal("av1_qsv exited non-zero; Exercise must report that as an error")
	}
	if !strings.Contains(err.Error(), "No capable devices found") {
		t.Errorf("the error lost FFmpeg's own words: %v", err)
	}

	subprocesses.Add(1)
	if err := r.Exercise(ctx, capability.Candidate{
		Codec: "hevc", Accel: capability.AccelQSV, Encoder: "hevc_qsv",
	}); err != nil {
		t.Errorf("hevc_qsv exited 0; Exercise must report success: %v", err)
	}
}

// A real process, real output, real parsing.
func TestTheExecRunnerReadsWhatTheBinaryPrints(t *testing.T) {
	r := runner(t, fixtureEverythingWorks)
	subprocesses.Add(1)
	names, err := r.ListEncoders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"av1_qsv", "hevc_qsv", "libx264", "aac"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("listed %v, want %v", names, want)
	}
}

// The argv IS the probe. A change that stopped it being an encode — a dropped
// -c:v, a sink that is not null, a source that is a file on disk — would leave
// every test above passing while the thing under test had become a no-op.
func TestTheProbeArgvIsAnEncodeToANullSink(t *testing.T) {
	tests := []struct {
		name string
		cand capability.Candidate
		want []string
	}{
		{
			name: "software",
			cand: capability.Candidate{Codec: "hevc", Encoder: "libx265"},
			want: []string{
				"-hide_banner", "-loglevel", "error", "-nostdin",
				"-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=8",
				"-frames:v", "8", "-an", "-c:v", "libx265", "-f", "null", "-",
			},
		},
		{
			name: "vaapi uploads to a hardware surface",
			cand: capability.Candidate{Codec: "av1", Accel: capability.AccelVAAPI, Encoder: "av1_vaapi"},
			want: []string{
				"-hide_banner", "-loglevel", "error", "-nostdin",
				"-vaapi_device", "/dev/dri/renderD128",
				"-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=8",
				"-vf", "format=nv12,hwupload",
				"-frames:v", "8", "-an", "-c:v", "av1_vaapi", "-f", "null", "-",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := capability.ExerciseArgs(tc.cand); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("argv:\n got %v\nwant %v", got, tc.want)
			}
		})
	}
}

// The vocabulary, spelled out. `ffmpeg` is a PREFIX of `ffmpeg.encoder.hevc`,
// so anything anywhere that compares these with a substring match is wrong —
// this is the assertion that says what the strings actually are.
func TestTheVocabularyIsDottedAndExact(t *testing.T) {
	cases := []struct{ codec, accel, want string }{
		{"hevc", "", "ffmpeg.encoder.hevc"},
		{"hevc", capability.AccelQSV, "ffmpeg.encoder.hevc.qsv"},
		{"av1", capability.AccelVAAPI, "ffmpeg.encoder.av1.vaapi"},
	}
	for _, c := range cases {
		if got := capability.EncoderCapability(c.codec, c.accel); got != c.want {
			t.Errorf("EncoderCapability(%q, %q) = %q, want %q", c.codec, c.accel, got, c.want)
		}
	}
	if capability.CodecCapability("hevc") != "ffmpeg.encoder.hevc" {
		t.Error("the rollup name must be the accelerator-free encoder name")
	}
	if capability.FFmpeg == capability.CodecCapability("hevc") {
		t.Error("the binary and a codec must be different strings")
	}
	if !strings.HasPrefix(capability.CodecCapability("hevc"), capability.FFmpeg) {
		t.Error("the hierarchy is broken; this test documents that the prefix relationship EXISTS, " +
			"which is exactly why every comparison of these strings must be equality")
	}
}

// Merge is how the startup-resolved binary half and the probed hardware half
// become one advertisement. First writer wins, so a binary capability is not
// overwritten by a probe that happens to share its name.
func TestMergeKeepsTheFirstWriter(t *testing.T) {
	binary := []capability.Held{{Name: "ffmpeg", Source: capability.SourceBinary, Detail: "resolved at startup"}}
	probed := []capability.Held{
		{Name: "ffmpeg", Source: capability.SourceProbe, Detail: "should not win"},
		{Name: "ffmpeg.encoder.hevc", Source: capability.SourceProbe},
	}
	got := capability.Merge(binary, probed)
	assertNames(t, got, []string{"ffmpeg", "ffmpeg.encoder.hevc"})
	if got[0].Source != capability.SourceBinary {
		t.Errorf("ffmpeg came from %q, want %q", got[0].Source, capability.SourceBinary)
	}
}

func TestParseEncoderListReadsTheRealFormat(t *testing.T) {
	got := capability.ParseEncoderList(encoderListing)
	want := []string{"av1_qsv", "hevc_qsv", "libx264", "aac"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsed %v, want %v", got, want)
	}
	if len(capability.ParseEncoderList("")) != 0 {
		t.Error("empty output must parse to no encoders")
	}
}

// A cancelled context stops the sweep rather than launching the rest of the
// candidates against a shutting-down process.
func TestACancelledProbeStops(t *testing.T) {
	c := &counted{inner: runner(t, fixtureEverythingWorks)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	held := capability.Probe(ctx, c, candidates, nil, nil)
	if len(held) != 0 {
		t.Errorf("a cancelled probe advertised %v", capability.Names(held))
	}
}

// The proof carries when it happened, because a re-advertisement that reused a
// cached proof must not be able to look fresh.
func TestAProofCarriesTheMomentItWasTaken(t *testing.T) {
	at := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	c := &counted{inner: runner(t, fixtureEverythingWorks)}
	held := capability.Probe(context.Background(), c, candidates, func() time.Time { return at }, nil)
	if len(held) == 0 {
		t.Fatal("nothing was proven, so nothing is being asserted")
	}
	for _, h := range held {
		if !h.ProvedAt.Equal(at) {
			t.Errorf("%s was proved at %s, want %s", h.Name, h.ProvedAt, at)
		}
		if h.Source != capability.SourceProbe {
			t.Errorf("%s has source %q, want %q", h.Name, h.Source, capability.SourceProbe)
		}
	}
}

// The guard. This package's whole claim is that it exercises things; a run in
// which nothing was executed must be red, not `ok`.
//
// It is a test rather than a TestMain check because a TestMain that fails after
// m.Run has already reported success is confusing to read in CI output.
func TestZZZTheseTestsActuallyLaunchedSubprocesses(t *testing.T) {
	const atLeast = 10
	if got := subprocesses.Load(); got < atLeast {
		t.Fatalf("only %d subprocesses were launched in this package, want at least %d — "+
			"the exec-level assertions did not run, and a green result here would mean nothing",
			got, atLeast)
	}
}

func assertNames(t *testing.T, held []capability.Held, want []string) {
	t.Helper()
	got := capability.Names(held)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("advertised:\n got %v\nwant %v", got, want)
	}
}

// hasExactly is equality, never containment. See TestTheVocabularyIsDottedAndExact.
func hasExactly(held []capability.Held, name string) bool {
	for _, h := range held {
		if h.Name == name {
			return true
		}
	}
	return false
}

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
