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
	// Start seeks the input to this many seconds before producing anything.
	// A restarted stream, not a seek within one (§33 note in ADR-0069).
	Start float64
}

// StreamArgs is the command line, in one place so it can be read and tested.
//
// Every flag is load-bearing:
//
//	-nostdin            no terminal to block on (see remuxArgs).
//	-ss before -i       an input seek, which lands on a keyframe fast rather
//	                    than decoding up to the instant.
//	-map 0:v:0 / 0:a:0  the FIRST video and audio stream. A client that could
//	                    pick tracks would not need this route; a repackage that
//	                    carried six audio tracks would re-encode five for nobody.
//	-c:v copy           the contract when the client can decode the picture.
//	-c:a aac -ac 2      stereo AAC is what every client on earth decodes; 5.1
//	                    AAC is not, and a downmix is what a phone would do anyway.
//	-movflags           fragmented MP4 that plays as it arrives: an empty moov
//	                    up front, a fragment per keyframe, offsets that do not
//	                    need the whole file. delay_moov holds the moov until
//	                    the first packet of every stream has been seen — a
//	                    copied AC-3 track has no frame size until then, and
//	                    without it the muxer refuses to write the header.
//	-f mp4 -            to stdout, which is the response.
func StreamArgs(spec StreamSpec) []string {
	args := []string{"-hide_banner", "-loglevel", "warning", "-nostdin"}
	if spec.Start > 0 {
		args = append(args, "-ss", strconv.FormatFloat(spec.Start, 'f', 3, 64))
	}
	args = append(args, "-i", spec.Source, "-map", "0:v:0?", "-map", "0:a:0?")
	if spec.CopyVideo {
		args = append(args, "-c:v", "copy")
	} else {
		args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p")
		if spec.MaxHeight > 0 {
			// -2 keeps the width even, which libx264 with yuv420p requires.
			args = append(args, "-vf", fmt.Sprintf("scale=-2:'min(%d,ih)'", spec.MaxHeight))
		}
	}
	if spec.CopyAudio {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-ac", "2", "-b:a", "192k")
	}
	return append(args,
		"-movflags", "frag_keyframe+empty_moov+default_base_moof+delay_moov",
		"-f", "mp4", "-")
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
	s.log.Info("stream starting", "source", Base(spec.Source),
		"copy_video", spec.CopyVideo, "copy_audio", spec.CopyAudio,
		"max_height", spec.MaxHeight, "start", spec.Start, "active", s.Active())

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
