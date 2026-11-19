package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The on-the-fly repackage (§33, ADR-0069).
//
// A Streamer runs ONE ffmpeg per client, writing fragmented MP4 to the
// client's response as it is produced, and kills it the moment the client
// goes away. It is the second thing this package does, and it is deliberately
// narrower than the first: there is still no ladder, no HLS, no segmenter and
// no hardware acceleration. Video is copied whenever the caller says it may
// be; audio is re-encoded to stereo AAC whenever it may not. The decision is
// the domain's (playback.Negotiate); this is the arm.
//
// # Bounded, on purpose
//
// A re-encode costs a core, and a phone that reconnects five times in a
// minute would otherwise leave five ffmpegs re-encoding one film for nobody.
// So there is a concurrency cap, a hard kill on disconnect, and a stderr
// buffer that keeps the tail rather than the whole — an ffmpeg that logs a
// warning per frame must not grow the process with it.

// ErrStreamBusy means the node is already running as many streams as it will.
var ErrStreamBusy = errors.New("ffmpeg: every stream slot is in use")

// ErrStreamFailed means ffmpeg exited before the client disconnected, and not
// cleanly. The detail carries the tail of its stderr.
var ErrStreamFailed = errors.New("ffmpeg: the stream failed")

// DefaultStreamConcurrency is the cap when configuration names none: two,
// because a re-encode is a core and the reference host is a small machine.
const DefaultStreamConcurrency = 2

// stderrTail is how much of ffmpeg's stderr is kept for the log line. The end
// is what says why it died; the beginning is banners.
const stderrTail = 4 << 10

// killGrace is how long a cancelled ffmpeg gets between the kill and Wait
// giving up on its pipes.
const killGrace = 2 * time.Second

// StreamSpec is one repackage.
type StreamSpec struct {
	// Source is the blob's local path. Immutable bytes; never written.
	Source string
	// AudioTrack is the zero-based audio ordinal selected by the playback plan.
	AudioTrack int
	// CopyVideo carries the source's first video stream unchanged; false
	// re-encodes it with libx264.
	CopyVideo bool
	// CopyAudio carries the source's first audio stream unchanged; false
	// re-encodes it to stereo AAC.
	CopyAudio bool
	// MaxHeight scales the picture down to this height when re-encoding.
	// Zero keeps the source height. Ignored when CopyVideo is set — a copy
	// cannot scale.
	MaxHeight int
	// ToneMap converts an HDR picture to BT.709 SDR on the way through the
	// re-encode, from the transfer it names (#886). Empty is no tone-map.
	// Ignored when CopyVideo is set, like MaxHeight.
	ToneMap ToneMap
	// Start seeks the input to this many seconds before producing anything.
	// A restarted stream, not a seek within one (§33 note in ADR-0069).
	// Ignored when Segmented is set, which carries its own start.
	Start float64
	// Segmented shapes the output for the HLS segmenter (ADR-0126) rather
	// than for a player: timestamps on a timeline every restart shares, so a
	// segment made by one ffmpeg is interchangeable with the same segment made
	// by another. Nil is the live stream of ADR-0069.
	Segmented *Segmented
}

// Segmented is an HLS production run: where it starts, on which timeline.
//
// There are two timelines, because the two video arms find their segment
// boundaries differently (see internal/media/hls):
//
//   - A copied picture is cut at the source's own keyframes, so the output
//     keeps the SOURCE's timestamps (-copyts), and the run starts by seeking
//     to just after the keyframe it begins at. The demuxer lands on that
//     keyframe — or, with a coarse index, an earlier one, whose fragments the
//     segmenter discards.
//   - A re-encoded picture is cut where this run forces keyframes, every
//     KeyframeSeconds from where it starts. The run seeks exactly to its
//     first boundary (an accurate seek) and its output timeline is offset so
//     that boundary lands where every other run puts it. -copyts is NOT used:
//     the forced-keyframe clock must start at the run's first frame, and what
//     it counts from under -copyts is not the same in every ffmpeg release.
type Segmented struct {
	// From is the first segment boundary this run produces, in seconds on the
	// segment timeline. For a copied picture that is the source's own
	// presentation time; for a re-encode it is seconds after the source's
	// start time.
	From float64
	// SeekPast is how far past From the input seek aims, for a copied
	// picture: enough that the boundary keyframe is the last one at or before
	// the seek point, however the index rounds it. Zero for a re-encode.
	SeekPast float64
	// KeyframeSeconds is the forced keyframe cadence of a re-encode, which is
	// the segment length. Ignored when the picture is copied.
	KeyframeSeconds float64
}

// SegmentTimelineOffset is added to every output timestamp of a segmented
// run. A fragment's decode time is unsigned, and the first frames of a film
// with B-frames, or the encoder priming of an AAC track, decode BEFORE zero;
// without the offset they wrap to the far end of a 64-bit clock. Ten seconds
// is far more than any decoder delay. The segmenter subtracts it again.
const SegmentTimelineOffset = 10.0

// ToneMap names the HDR transfer a re-encode tone-maps from.
type ToneMap string

const (
	// ToneMapNone leaves the picture's samples as the decoder hands them.
	ToneMapNone ToneMap = ""
	// ToneMapPQ is SMPTE ST 2084: HDR10, HDR10+, and the HDR10 base layer of
	// Dolby Vision profiles 7 and 8.1.
	ToneMapPQ ToneMap = "pq"
	// ToneMapHLG is ARIB STD-B67, including Dolby Vision 8.4's base.
	ToneMapHLG ToneMap = "hlg"
)

// toneMapFilters is the software tone-map, as -vf filters (#886).
//
// It is zimg (zscale) and ffmpeg's tonemap, because that is what the pinned
// static build carries: libzimg, and neither libplacebo nor a hardware
// tone-mapper this node would encode after (the encoder is libx264). In
// order:
//
//	zscale tin/min/pin/rin  name the source's transfer, matrix, primaries and
//	                        range rather than trusting the frames to carry
//	                        them; a remux that dropped its colour tags would
//	                        otherwise fail with "no path between colorspaces".
//	                        HDR is mastered in BT.2020; that is assumed.
//	t=linear:npl=100        to linear light, with SDR white at 100 nits, in
//	format=gbrpf32le        float RGB, which is what tonemap works on.
//	zscale p=bt709          into the BT.709 gamut, before the curve compresses
//	                        the range, so out-of-gamut colour is compressed too.
//	tonemap hable desat=0   Hable's filmic curve, which keeps highlight detail
//	                        rather than clipping it; no desaturation, because
//	                        hable already rolls saturation off with the luminance.
//	zscale t/m/r bt709 tv   back to BT.709 gamma, the BT.709 matrix, limited
//	format=yuv420p          range, eight-bit 4:2:0 for libx264.
func toneMapFilters(from ToneMap) []string {
	tin := "smpte2084"
	if from == ToneMapHLG {
		tin = "arib-std-b67"
	}
	return []string{
		"zscale=tin=" + tin + ":min=bt2020nc:pin=bt2020:rin=tv:t=linear:npl=100",
		"format=gbrpf32le",
		"zscale=p=bt709",
		"tonemap=tonemap=hable:desat=0",
		"zscale=t=bt709:m=bt709:r=tv",
		"format=yuv420p",
	}
}

// StreamArgs is the command line, in one place so it can be read and tested.
//
// Every flag is load-bearing:
//
//	-nostdin            no terminal to block on (see remuxArgs).
//	-ss before -i       an input seek, which lands on a keyframe fast rather
//	                    than decoding up to the instant.
//	-map 0:V:0? / 0:a:N?  the first real video (excluding attached cover art)
//	                    and the planned audio track. Only that audio is encoded;
//	                    selecting it before streaming preserves
//	                    language preference without encoding unused tracks.
//	-c:v copy           the contract when the client can decode the picture.
//	-c:a aac -ac 2      stereo AAC is what every client on earth decodes; 5.1
//	                    AAC is not, and a downmix is what a phone would do anyway.
//	-movflags           fragmented MP4 that plays as it arrives: an empty moov
//	                    up front, a fragment per keyframe, offsets that do not
//	                    need the whole file. delay_moov holds the moov until
//	                    the first packet of every stream has been seen — a
//	                    copied AC-3 track has no frame size until then, and
//	                    without it the muxer refuses to write the header.
//	-frag_duration      flush a fragment after one second of media, even when
//	                    copying video with widely spaced source keyframes.
//	-force_key_frames   encoded video gets a keyframe each second so the first
//	                    playable fragment does not wait for x264's default GOP.
//	-vf scale,...       the height cap first, so a 2160p source is tone-mapped
//	                    at the client's size rather than at four times it;
//	                    then the tone-map (see toneMapFilters).
//	-color_*            a tone-mapped stream is TAGGED BT.709. Without the
//	                    tags libx264 inherits the source's PQ and BT.2020, and
//	                    a player that honours them undoes the tone-map.
//
//	-f mp4 -            to stdout, which is the response.
func StreamArgs(spec StreamSpec) []string {
	if spec.Segmented != nil {
		return segmentedArgs(spec)
	}
	args := []string{"-hide_banner", "-loglevel", "warning", "-nostdin"}
	if spec.Start > 0 {
		args = append(args, "-ss", strconv.FormatFloat(spec.Start, 'f', 3, 64))
	}
	args = append(args, "-i", spec.Source)
	args = append(args, codecArgs(spec, "expr:gte(t,n_forced*1)")...)
	return append(args,
		"-movflags", "frag_keyframe+empty_moov+default_base_moof+delay_moov",
		"-frag_duration", "1000000", "-f", "mp4", "-")
}

// segmentedArgs is StreamArgs for an HLS production run (ADR-0126). The
// codec decisions are StreamArgs' own; what differs is the clock:
//
//	-noaccurate_seek    a copied picture starts at the keyframe the demuxer
//	-ss -seek_timestamp lands on, and so does the audio beside it. -ss is
//	-copyts             the source's own timestamp, not offset by its start
//	                    time, and the output keeps the source's timestamps.
//	-ss (re-encode)     an accurate seek to the first boundary, relative to
//	                    the source's start like ffmpeg's ordinary -ss, so the
//	                    forced-keyframe clock (t) starts at it.
//	-force_key_frames   one keyframe per segment, at every boundary.
//	-output_ts_offset   SegmentTimelineOffset, plus a re-encode's From: every
//	                    run of the same segment carries the same timestamps.
//	-avoid_negative_ts  disabled, and +frag_discont: each fragment's decode
//	                    time is the packet's, not a count from zero. Without
//	                    both, the muxer rebases every run to start at zero
//	                    and segment 40 of one run is segment 0 of another.
//	-use_editlist 0     no edit list, so the init segment is the same bytes
//	-map_metadata -1    from every run; it carries no per-run offset and no
//	-map_chapters -1    creation time.
//	-video_track_timescale  fixed, for the same reason.
//	-movflags           a fragment per keyframe (the segmenter's unit) and no
//	                    -frag_duration, which would cut fragments between
//	                    keyframes and leave a segment boundary mid-fragment.
func segmentedArgs(spec StreamSpec) []string {
	seg := spec.Segmented
	args := []string{"-hide_banner", "-loglevel", "warning", "-nostdin"}
	offset := SegmentTimelineOffset
	keyframes := ""
	if spec.CopyVideo {
		args = append(args, "-noaccurate_seek")
		if seg.From > 0 || seg.SeekPast > 0 {
			args = append(args, "-ss", strconv.FormatFloat(seg.From+seg.SeekPast, 'f', 6, 64), "-seek_timestamp", "1")
		}
		args = append(args, "-copyts")
	} else {
		if seg.From > 0 {
			args = append(args, "-ss", strconv.FormatFloat(seg.From, 'f', 6, 64))
		}
		offset += seg.From
		every := seg.KeyframeSeconds
		if every <= 0 {
			every = 6
		}
		keyframes = "expr:gte(t,n_forced*" + strconv.FormatFloat(every, 'f', -1, 64) + ")"
	}
	args = append(args, "-i", spec.Source)
	args = append(args, codecArgs(spec, keyframes)...)
	return append(args,
		"-avoid_negative_ts", "disabled",
		"-output_ts_offset", strconv.FormatFloat(offset, 'f', 6, 64),
		"-use_editlist", "0", "-map_metadata", "-1", "-map_chapters", "-1",
		"-video_track_timescale", "90000",
		"-movflags", "frag_keyframe+empty_moov+default_base_moof+delay_moov+frag_discont",
		"-f", "mp4", "-")
}

// codecArgs is the stream selection and the per-stream copy or re-encode,
// shared by both output shapes. forceKeyFrames is the re-encode's keyframe
// expression.
func codecArgs(spec StreamSpec, forceKeyFrames string) []string {
	args := []string{"-map", "0:V:0?", "-map", fmt.Sprintf("0:a:%d?", spec.AudioTrack)}
	if spec.CopyVideo {
		args = append(args, "-c:v", "copy")
	} else {
		args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p",
			"-force_key_frames", forceKeyFrames)
		var filters []string
		if spec.MaxHeight > 0 {
			// -2 keeps the width even, which libx264 with yuv420p requires.
			filters = append(filters, fmt.Sprintf("scale=-2:'min(%d,ih)'", spec.MaxHeight))
		}
		if spec.ToneMap != ToneMapNone {
			filters = append(filters, toneMapFilters(spec.ToneMap)...)
		}
		if len(filters) > 0 {
			args = append(args, "-vf", strings.Join(filters, ","))
		}
		if spec.ToneMap != ToneMapNone {
			args = append(args, "-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709")
		}
	}
	if spec.CopyAudio {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-ac", "2", "-b:a", "192k")
	}
	return args
}

// StreamerOptions configure a Streamer.
type StreamerOptions struct {
	// FFmpegPath is the resolved binary (ADR-0023). Required.
	FFmpegPath string
	// MaxConcurrent caps live streams. Zero means DefaultStreamConcurrency.
	MaxConcurrent int
	Logger        *slog.Logger
}

// Streamer runs repackages.
type Streamer struct {
	bin    string
	slots  chan struct{}
	active atomic.Int64
	log    *slog.Logger

	// toneMapMu guards the answer ToneMaps learned, once it learned one.
	toneMapMu    sync.Mutex
	toneMapKnown bool
	toneMaps     bool
}

// NewStreamer builds a Streamer.
func NewStreamer(opts StreamerOptions) (*Streamer, error) {
	if opts.FFmpegPath == "" {
		return nil, errors.New("ffmpeg: a binary path is required")
	}
	n := opts.MaxConcurrent
	if n <= 0 {
		n = DefaultStreamConcurrency
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Streamer{
		bin:   opts.FFmpegPath,
		slots: make(chan struct{}, n),
		log:   log.With("component", "ffmpeg.stream"),
	}, nil
}

// toneMapListTimeout bounds `-filters`, which reads no input and touches no
// device; generous only because the binary may be cold.
const toneMapListTimeout = 10 * time.Second

// toneMapNeeds is every filter toneMapFilters uses that a build can lack.
// zscale is libzimg, which is an optional dependency of ffmpeg; tonemap is
// built in, but a trimmed build can leave it out.
var toneMapNeeds = []string{"zscale", "tonemap"}

// ToneMaps reports whether this ffmpeg has the filters a tone-map needs.
//
// It asks the binary once and remembers the answer. A binary that could not be
// asked — a timeout, a cancelled request — is "no" for this call and asked
// again next time, because that is a property of the moment and not of the
// build. Asking is cheap and touches nothing; a stream that is planned with a
// tone-map and then fails to start on a missing filter is a black player.
func (s *Streamer) ToneMaps(ctx context.Context) bool {
	s.toneMapMu.Lock()
	defer s.toneMapMu.Unlock()
	if s.toneMapKnown {
		return s.toneMaps
	}
	ctx, cancel := context.WithTimeout(ctx, toneMapListTimeout)
	defer cancel()
	// #nosec G204 -- the binary is the resolved toolchain (ADR-0023) and every
	// argument is a literal.
	out, err := exec.CommandContext(ctx, s.bin, "-hide_banner", "-filters").Output()
	if err != nil {
		s.log.Warn("could not list ffmpeg's filters; streaming without a tone-map for now", "error", err)
		return false
	}
	have := map[string]bool{}
	for _, f := range ParseFilterList(string(out)) {
		have[f] = true
	}
	s.toneMaps, s.toneMapKnown = true, true
	for _, need := range toneMapNeeds {
		if !have[need] {
			s.toneMaps = false
			s.log.Warn("this ffmpeg has no "+need+" filter; HDR re-encodes will not be tone-mapped", "binary", Base(s.bin))
		}
	}
	return s.toneMaps
}

// ParseFilterList reads `ffmpeg -filters`: a legend, then one filter per
// line as three flag characters, the name, its pads ("V->V") and a
// description. A line is a filter when it has that shape; the legend's lines
// ("T.. = Timeline support") do not, and there is no separator line to wait
// for, unlike `-encoders`.
func ParseFilterList(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || len(fields[0]) != 3 || strings.Trim(fields[0], "TSC.") != "" ||
			!strings.Contains(fields[2], "->") {
			continue
		}
		names = append(names, fields[1])
	}
	return names
}

// Active is how many streams are running now — the metrics gauge.
func (s *Streamer) Active() int { return int(s.active.Load()) }

// Capacity is the cap.
func (s *Streamer) Capacity() int { return cap(s.slots) }

// Stream runs one repackage, writing fragmented MP4 to w until the input is
// exhausted or ctx ends.
//
// A cancelled ctx — the client disconnected — kills ffmpeg and returns
// ctx.Err(). ffmpeg exiting non-zero on its own returns ErrStreamFailed with
// its stderr tail. It refuses with ErrStreamBusy rather than queueing when
// every slot is taken: a client waiting on a stream that has not started is a
// black player, and it is better told to try again.
func (s *Streamer) Stream(ctx context.Context, spec StreamSpec, w io.Writer) error {
	if spec.Source == "" {
		return errors.New("ffmpeg: a stream needs a source path")
	}
	select {
	case s.slots <- struct{}{}:
	default:
		return ErrStreamBusy
	}
	defer func() { <-s.slots }()
	s.active.Add(1)
	defer s.active.Add(-1)

	// #nosec G204 -- the binary is the resolved toolchain (ADR-0023) and every
	// argument is a literal or a path the CAS handed back.
	cmd := exec.CommandContext(ctx, s.bin, StreamArgs(spec)...)
	// Kill, not interrupt: an ffmpeg that gets SIGINT tries to finalise the
	// output, which is exactly the work the disconnected client will not
	// receive. WaitDelay bounds how long Wait holds the pipes open after that.
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	cmd.WaitDelay = killGrace
	stderr := &tailBuffer{max: stderrTail}
	cmd.Stderr = stderr
	cmd.Stdout = w

	started := time.Now()
	start, shape := spec.Start, "live"
	if spec.Segmented != nil {
		start, shape = spec.Segmented.From, "segmented"
	}
	s.log.Info("stream starting", "source", Base(spec.Source), "shape", shape,
		"copy_video", spec.CopyVideo, "copy_audio", spec.CopyAudio,
		"max_height", spec.MaxHeight, "tone_map", string(spec.ToneMap), "start", start, "active", s.Active())

	err := cmd.Run()
	elapsed := time.Since(started)
	switch {
	case ctx.Err() != nil:
		// The client went away. Whatever ffmpeg said about being killed is
		// not a fault worth a warning.
		s.log.Info("stream ended: client gone", "source", Base(spec.Source), "elapsed", elapsed)
		return ctx.Err()
	case err != nil:
		detail := stderr.String()
		s.log.Warn("stream failed", "source", Base(spec.Source), "elapsed", elapsed,
			"error", err, "stderr", detail)
		return fmt.Errorf("%w: %w: %s", ErrStreamFailed, err, detail)
	}
	s.log.Info("stream complete", "source", Base(spec.Source), "elapsed", elapsed)
	return nil
}

// tailBuffer keeps the last max bytes written. It exists so an ffmpeg that
// warns once per frame for two hours costs a fixed amount of memory, and so
// the log line carries the end of stderr — where the reason is — rather than
// the banner.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}
