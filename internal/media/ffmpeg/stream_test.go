package ffmpeg_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/media/ffmpeg"
)

// The command line is the contract; read it as one.
func TestStreamArgsAreTheContract(t *testing.T) {
	cases := []struct {
		name    string
		spec    ffmpeg.StreamSpec
		want    []string
		wantNot []string
	}{
		{
			name: "copy video, transcode audio — the AC-3 case",
			spec: ffmpeg.StreamSpec{Source: "/srv/x.mp4", CopyVideo: true},
			want: []string{
				"-map 0:v:0? -map 0:a:0?", "-c:v copy", "-c:a aac -ac 2 -b:a 192k",
				"-movflags frag_keyframe+empty_moov+default_base_moof+delay_moov -f mp4 -",
			},
			wantNot: []string{"libx264", "-ss", "scale="},
		},
		{
			name:    "copy both — the AVI-with-decodable-streams case",
			spec:    ffmpeg.StreamSpec{Source: "/srv/x.avi", CopyVideo: true, CopyAudio: true},
			want:    []string{"-c:v copy", "-c:a copy"},
			wantNot: []string{"aac", "libx264"},
		},
		{
			name: "transcode video capped to a height, with a start offset",
			spec: ffmpeg.StreamSpec{Source: "/srv/x.mkv", MaxHeight: 1080, Start: 61.5},
			want: []string{
				"-ss 61.500 -i /srv/x.mkv", "-c:v libx264 -preset veryfast -crf 23",
				"-vf scale=-2:'min(1080,ih)'", "-c:a aac",
			},
		},
		{
			name:    "transcode video with no cap scales nothing",
			spec:    ffmpeg.StreamSpec{Source: "/srv/x.mkv"},
			want:    []string{"-c:v libx264"},
			wantNot: []string{"-vf"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(ffmpeg.StreamArgs(tc.spec), " ")
			if !strings.HasPrefix(got, "-hide_banner -loglevel warning -nostdin") {
				t.Errorf("args = %q, want the quiet, no-stdin prefix", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("args = %q\n  want %q", got, w)
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(got, w) {
					t.Errorf("args = %q\n  must not carry %q", got, w)
				}
			}
		})
	}
}

// generate encodes a short synthetic source with the codecs the case needs —
// the committed fixtures deliberately carry only h264/aac, and the whole point
// here is a track the client cannot decode.
func generate(t *testing.T, ffmpegPath, name string, args ...string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), name)
	full := append([]string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=160x120:rate=10",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", "2",
	}, args...)
	full = append(full, out)
	if b, err := exec.CommandContext(t.Context(), ffmpegPath, full...).CombinedOutput(); err != nil {
		t.Fatalf("generating %s: %v\n%s", name, err, b)
	}
	return out
}

// probeSummary is "format|codec,codec" for captured bytes — written to a file
// first, because ffprobe on stdin cannot seek and a fragmented MP4 does not
// need it to.
func probeSummary(t *testing.T, ffprobePath string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "captured.mp4")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return streamShape(t, ffprobePath, p)
}

// streamShape is "format|codec,codec" for a file, one line of ffprobe output
// per fact, so a container name that itself contains commas stays readable.
func streamShape(t *testing.T, ffprobePath, path string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), ffprobePath,
		"-v", "error", "-show_entries", "format=format_name:stream=codec_name",
		"-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		t.Fatalf("probing %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		t.Fatalf("ffprobe said only %q about %s", out, path)
	}
	// Streams come first, the format last.
	return lines[len(lines)-1] + "|" + strings.Join(lines[:len(lines)-1], ",")
}

// A real ffmpeg, a real AC-3 source, and the bytes a phone would receive:
// h264 copied, audio now AAC, in an MP4 that plays as it arrives.
func TestStreamRepackagesWhatTheClientCannotDecode(t *testing.T) {
	ffmpegPath, ffprobePath := toolchain(t)
	s, err := ffmpeg.NewStreamer(ffmpeg.StreamerOptions{FFmpegPath: ffmpegPath})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		source string
		spec   ffmpeg.StreamSpec
		want   string
	}{
		{
			name:   "AC-3 in MP4: video copied, audio to AAC",
			source: generate(t, ffmpegPath, "ac3.mp4", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "ac3"),
			spec:   ffmpeg.StreamSpec{CopyVideo: true},
			want:   "mov,mp4,m4a,3gp,3g2,mj2|h264,aac",
		},
		{
			name:   "H.264 + MP2 in AVI: remuxed, audio to AAC",
			source: generate(t, ffmpegPath, "mp2.avi", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "mp2", "-f", "avi"),
			spec:   ffmpeg.StreamSpec{CopyVideo: true},
			want:   "mov,mp4,m4a,3gp,3g2,mj2|h264,aac",
		},
		{
			name:   "AC-3 the client declares: audio copied too",
			source: generate(t, ffmpegPath, "ac3-copy.mp4", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "ac3"),
			spec:   ffmpeg.StreamSpec{CopyVideo: true, CopyAudio: true},
			want:   "mov,mp4,m4a,3gp,3g2,mj2|h264,ac3",
		},
		{
			name:   "video the client cannot decode: re-encoded to h264 and capped",
			source: generate(t, ffmpegPath, "mpeg2.mp4", "-c:v", "mpeg2video", "-c:a", "aac"),
			spec:   ffmpeg.StreamSpec{MaxHeight: 96, CopyAudio: true},
			want:   "mov,mp4,m4a,3gp,3g2,mj2|h264,aac",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			spec := tc.spec
			spec.Source = tc.source
			if err := s.Stream(t.Context(), spec, &out); err != nil {
				t.Fatalf("stream: %v", err)
			}
			if got := probeSummary(t, ffprobePath, out.Bytes()); got != tc.want {
				t.Errorf("captured bytes probe as %q, want %q", got, tc.want)
			}
			// Fragmented: the moov comes first and is empty, so a player can
			// start before the end exists. The `moof` box is what makes it
			// fragmented at all.
			if !bytes.Contains(out.Bytes(), []byte("moof")) {
				t.Error("the output carries no movie fragment; a player would wait for the end")
			}
			if s.Active() != 0 {
				t.Errorf("active = %d after the stream ended", s.Active())
			}
		})
	}
	// The source was never touched: a repackage reads a blob, and blobs are
	// immutable (invariant 1).
	if got := streamShape(t, ffprobePath, cases[0].source); got != "mov,mp4,m4a,3gp,3g2,mj2|h264,ac3" {
		t.Errorf("the AC-3 source now probes as %q", got)
	}
}

// A writer that accepts nothing until the context ends — a client that has
// stopped reading. ffmpeg's stdout pipe fills, ffmpeg blocks, and the only way
// the stream can end is by being killed.
type stalledWriter struct {
	ctx   context.Context
	once  sync.Once
	first chan struct{}
}

func (w *stalledWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.first) })
	<-w.ctx.Done()
	return 0, w.ctx.Err()
}

// The client disconnects; ffmpeg dies. Not "eventually": within the grace,
// and the slot is given back.
func TestStreamKillsFFmpegWhenTheClientDisconnects(t *testing.T) {
	ffmpegPath, _ := toolchain(t)
	src := generate(t, ffmpegPath, "long.mp4", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "ac3")
	s, err := ffmpeg.NewStreamer(ffmpeg.StreamerOptions{FFmpegPath: ffmpegPath, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	w := &stalledWriter{ctx: ctx, first: make(chan struct{})}
	done := make(chan error, 1)
	// Re-encode video too, so ffmpeg has real work to be killed in the middle
	// of, rather than finishing a copy before the pipe fills.
	go func() { done <- s.Stream(ctx, ffmpeg.StreamSpec{Source: src}, w) }()

	select {
	case <-w.first:
	case err := <-done:
		t.Fatalf("the stream ended before writing anything: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("ffmpeg produced nothing in 30s")
	}
	// While it runs, the one slot is taken.
	if err := s.Stream(t.Context(), ffmpeg.StreamSpec{Source: src, CopyVideo: true, CopyAudio: true}, &bytes.Buffer{}); !errors.Is(err, ffmpeg.ErrStreamBusy) {
		t.Errorf("a second stream past the cap returned %v, want ErrStreamBusy", err)
	}
	if s.Active() != 1 {
		t.Errorf("active = %d, want 1", s.Active())
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("stream returned %v, want the cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ffmpeg outlived the client by more than 10s")
	}
	if s.Active() != 0 {
		t.Errorf("active = %d after the disconnect, want 0", s.Active())
	}
	// And the slot came back.
	if err := s.Stream(t.Context(), ffmpeg.StreamSpec{Source: src, CopyVideo: true, CopyAudio: true}, &bytes.Buffer{}); err != nil {
		t.Errorf("a stream after the disconnect failed: %v", err)
	}
}

// An input ffmpeg cannot open is a failed stream with the reason attached,
// not a hang and not a silent empty body.
func TestStreamReportsFFmpegFailure(t *testing.T) {
	ffmpegPath, _ := toolchain(t)
	s, err := ffmpeg.NewStreamer(ffmpeg.StreamerOptions{FFmpegPath: ffmpegPath})
	if err != nil {
		t.Fatal(err)
	}
	notMedia := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(notMedia, []byte("not media"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = s.Stream(t.Context(), ffmpeg.StreamSpec{Source: notMedia, CopyVideo: true, CopyAudio: true}, &bytes.Buffer{})
	if !errors.Is(err, ffmpeg.ErrStreamFailed) {
		t.Fatalf("err = %v, want ErrStreamFailed", err)
	}
	if !strings.Contains(err.Error(), "Invalid data") && !strings.Contains(err.Error(), "invalid") {
		t.Errorf("the failure carries no ffmpeg detail: %v", err)
	}
}
