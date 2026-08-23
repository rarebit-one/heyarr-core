package capability

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Accelerator segments of the vocabulary. These are the strings that appear in
// a capability name, so they are spelled once.
const (
	AccelSoftware     = ""
	AccelVideoToolbox = "videotoolbox"
	AccelVAAPI        = "vaapi"
	AccelQSV          = "qsv"
	AccelNVENC        = "nvenc"
	AccelAMF          = "amf"
)

// exerciseFrames is how many frames a probe encodes.
//
// Enough to get past encoder initialisation — which is where "No capable
// devices found" is raised — and few enough that a probe of a dozen candidates
// costs a second or two rather than a minute. The failure this catches happens
// on the first frame; the frames after it are insurance against an encoder that
// initialises and then refuses to produce output.
const exerciseFrames = 8

// exerciseTimeout bounds ONE candidate.
//
// Short on purpose, and much shorter than the remux timeout next door: eight
// frames of 320x240 is not work. A candidate that has not finished in this long
// is a driver hanging on a device somebody else is holding, and a hang is the
// one outcome that must not be allowed to stall the beat — the whole point of
// re-verification is that it happens on time.
const exerciseTimeout = 20 * time.Second

// listTimeout bounds `-encoders`, which reads no device and touches no
// hardware. It is generous only because the binary is 45 MB and may be cold.
const listTimeout = 10 * time.Second

// vaapiDevice is the render node a VAAPI probe initialises against.
//
// Hard-coded rather than configured, and that deserves a sentence. A VAAPI
// encoder cannot be exercised at all without a device, so omitting this would
// make every VAAPI candidate fail forever — a silent, permanent false negative
// that looks exactly like "this hardware cannot encode", which is the one
// answer this package must never give wrongly by accident. The first render
// node is where a single-GPU homelab machine has it. A machine that puts it
// elsewhere gets a false negative, which costs that node the work and costs the
// fleet nothing; making it configurable is what the ADR records as the trigger
// to revisit.
const vaapiDevice = "/dev/dri/renderD128"

// ExecRunner exercises candidates by actually running FFmpeg.
type ExecRunner struct {
	bin string
}

// NewExecRunner builds a runner around a resolved binary (ADR-0023).
func NewExecRunner(ffmpegPath string) (*ExecRunner, error) {
	if ffmpegPath == "" {
		return nil, fmt.Errorf("capability: a resolved ffmpeg path is required")
	}
	return &ExecRunner{bin: ffmpegPath}, nil
}

// ListEncoders asks the binary what it has. See [Probe] for what this is and is
// not allowed to decide.
func (r *ExecRunner) ListEncoders(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()

	// #nosec G204 -- the binary is the resolved toolchain (ADR-0023) and every
	// argument is a literal.
	out, err := exec.CommandContext(ctx, r.bin, "-hide_banner", "-encoders").Output()
	if err != nil {
		return nil, fmt.Errorf("capability: listing encoders: %w", err)
	}
	return ParseEncoderList(string(out)), nil
}

// Exercise encodes a handful of frames and reports whether that worked.
//
// The error carries FFmpeg's own last words, trimmed to one line, because
// "No capable devices found" and "Device creation failed" send an operator to
// different places and both arrive here as exit status 1.
func (r *ExecRunner) Exercise(ctx context.Context, c Candidate) error {
	ctx, cancel := context.WithTimeout(ctx, exerciseTimeout)
	defer cancel()

	// #nosec G204 -- the binary is the resolved toolchain and every argument is
	// a literal or a member of the candidate table below.
	cmd := exec.CommandContext(ctx, r.bin, ExerciseArgs(c)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s did not encode: %w: %s", c.Encoder, err, lastLine(stderr.String()))
	}
	return nil
}

// ExerciseArgs is the command line for one probe, in one place so it can be
// read and asserted without hardware.
//
// Exported because the argv IS the design here: the difference between a probe
// that proves something and a probe that proves nothing is entirely in these
// flags, and a test that cannot see them can only assert that a process ran.
//
//	-nostdin      a worker has no terminal; without this a probe can block
//	              forever on one that is not there.
//	-f lavfi -i testsrc
//	              a synthetic source, so the probe needs no sample file on disk
//	              and cannot be confounded by the properties of one.
//	-frames:v N   a handful. See exerciseFrames.
//	-an           no audio. The question is about a video encoder.
//	-c:v <enc>    the candidate. This is the whole experiment.
//	-f null -     a null sink. Nothing is written, so the probe cannot fill a
//	              disk and cannot leave a file behind to be mistaken for output.
func ExerciseArgs(c Candidate) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin"}

	// VAAPI encodes from hardware surfaces, so a probe that fed it the raw
	// synthetic frames would fail on every machine including the ones that
	// work. The device and the upload are part of asking the question fairly.
	if c.Accel == AccelVAAPI {
		args = append(args, "-vaapi_device", vaapiDevice)
	}
	args = append(args,
		"-f", "lavfi",
		"-i", fmt.Sprintf("testsrc=size=320x240:rate=25:duration=%d", exerciseFrames),
	)
	if c.Accel == AccelVAAPI {
		args = append(args, "-vf", "format=nv12,hwupload")
	}
	return append(args,
		"-frames:v", fmt.Sprint(exerciseFrames),
		"-an",
		"-c:v", c.Encoder,
		"-f", "null", "-",
	)
}

// DefaultCandidates is every encoder worth asking about.
//
// It is a fixed table rather than something derived from `-encoders`, and that
// is the same decision as everywhere else in this package: what the binary
// lists is not evidence. The table says what Heyarr would USE if it worked; the
// probe says which of those actually do.
//
// Software encoders are included alongside hardware ones. A software HEVC
// encoder is a real capability — slower, and the correct place to route work
// when nothing in the fleet has silicon for it — and a fleet view that showed
// only hardware would report a node that can encode as one that cannot.
//
// Ordered software-first per codec so that the rollup's detail names a software
// encoder when one is present, which is the one that will still be there after
// a driver update.
func DefaultCandidates() []Candidate {
	return []Candidate{
		{Codec: "h264", Accel: AccelSoftware, Encoder: "libx264"},
		{Codec: "h264", Accel: AccelVideoToolbox, Encoder: "h264_videotoolbox"},
		{Codec: "h264", Accel: AccelVAAPI, Encoder: "h264_vaapi"},
		{Codec: "h264", Accel: AccelQSV, Encoder: "h264_qsv"},
		{Codec: "h264", Accel: AccelNVENC, Encoder: "h264_nvenc"},
		{Codec: "h264", Accel: AccelAMF, Encoder: "h264_amf"},

		{Codec: "hevc", Accel: AccelSoftware, Encoder: "libx265"},
		{Codec: "hevc", Accel: AccelVideoToolbox, Encoder: "hevc_videotoolbox"},
		{Codec: "hevc", Accel: AccelVAAPI, Encoder: "hevc_vaapi"},
		{Codec: "hevc", Accel: AccelQSV, Encoder: "hevc_qsv"},
		{Codec: "hevc", Accel: AccelNVENC, Encoder: "hevc_nvenc"},
		{Codec: "hevc", Accel: AccelAMF, Encoder: "hevc_amf"},

		// AV1 is the codec the motivating measurement was taken on: a device
		// that LISTS av1_qsv, fails to encode with "No capable devices found",
		// and decodes AV1 perfectly well. Every entry here is a candidate for
		// exactly that outcome.
		{Codec: "av1", Accel: AccelSoftware, Encoder: "libsvtav1"},
		{Codec: "av1", Accel: AccelVideoToolbox, Encoder: "av1_videotoolbox"},
		{Codec: "av1", Accel: AccelVAAPI, Encoder: "av1_vaapi"},
		{Codec: "av1", Accel: AccelQSV, Encoder: "av1_qsv"},
		{Codec: "av1", Accel: AccelNVENC, Encoder: "av1_nvenc"},
		{Codec: "av1", Accel: AccelAMF, Encoder: "av1_amf"},
	}
}

// lastLine is FFmpeg's final complaint, which is the useful one.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
