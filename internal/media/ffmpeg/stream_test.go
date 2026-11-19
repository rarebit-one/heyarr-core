package ffmpeg_test

import (
	"bytes"
	"context"
	"encoding/binary"
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
				"-map 0:V:0? -map 0:a:0?", "-c:v copy", "-c:a aac -ac 2 -b:a 192k",
				"-movflags frag_keyframe+empty_moov+default_base_moof+delay_moov -frag_duration 1000000 -f mp4 -",
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
			name: "HDR10 tone-mapped for an SDR client, scaled first",
			spec: ffmpeg.StreamSpec{Source: "/srv/x.mkv", MaxHeight: 1080, ToneMap: ffmpeg.ToneMapPQ},
			want: []string{
				"-vf scale=-2:'min(1080,ih)',zscale=tin=smpte2084:min=bt2020nc:pin=bt2020:rin=tv:t=linear:npl=100," +
					"format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv,format=yuv420p",
				"-color_primaries bt709 -color_trc bt709 -colorspace bt709",
				"-c:v libx264", "-pix_fmt yuv420p",
			},
		},
		{
			name: "HLG tone-mapped with no height cap",
			spec: ffmpeg.StreamSpec{Source: "/srv/x.mkv", ToneMap: ffmpeg.ToneMapHLG},
			want: []string{
				"-vf zscale=tin=arib-std-b67:min=bt2020nc:pin=bt2020:rin=tv:t=linear:npl=100,format=gbrpf32le,",
				"-color_trc bt709",
			},
			wantNot: []string{"scale=-2", "smpte2084"},
		},
		{
			name:    "a copied picture is never tone-mapped",
			spec:    ffmpeg.StreamSpec{Source: "/srv/x.mkv", CopyVideo: true, ToneMap: ffmpeg.ToneMapPQ},
			want:    []string{"-c:v copy"},
			wantNot: []string{"-vf", "zscale", "tonemap", "-color_trc"},
		},
		{
			name:    "an SDR re-encode carries no colour tags of its own",
			spec:    ffmpeg.StreamSpec{Source: "/srv/x.mkv", MaxHeight: 720},
			want:    []string{"-vf scale=-2:'min(720,ih)'"},
			wantNot: []string{"zscale", "-color_trc"},
		},
		{
			name:    "transcode video with no cap scales nothing",
			spec:    ffmpeg.StreamSpec{Source: "/srv/x.mkv"},
			want:    []string{"-c:v libx264"},
			wantNot: []string{"-vf"},
		},
		{
			name: "an HLS run of a copied picture keeps the source's clock (ADR-0126)",
			spec: ffmpeg.StreamSpec{
				Source: "/srv/x.mkv", CopyVideo: true, Start: 99,
				Segmented: &ffmpeg.Segmented{From: 1210.5, SeekPast: 0.25},
			},
			want: []string{
				"-noaccurate_seek -ss 1210.750000 -seek_timestamp 1 -copyts -i /srv/x.mkv",
				"-c:v copy", "-c:a aac",
				"-avoid_negative_ts disabled -output_ts_offset 10.000000",
				"-use_editlist 0 -map_metadata -1 -map_chapters -1 -video_track_timescale 90000",
				"-movflags frag_keyframe+empty_moov+default_base_moof+delay_moov+frag_discont -f mp4 -",
			},
			wantNot: []string{"-frag_duration", "-force_key_frames", "-ss 99"},
		},
		{
			name:    "an HLS run from the first segment does not seek",
			spec:    ffmpeg.StreamSpec{Source: "/srv/x.mkv", CopyVideo: true, Segmented: &ffmpeg.Segmented{}},
			want:    []string{"-noaccurate_seek -copyts -i /srv/x.mkv"},
			wantNot: []string{"-ss"},
		},
		{
			name: "an HLS re-encode forces a keyframe at every boundary and offsets its clock",
			spec: ffmpeg.StreamSpec{
				Source: "/srv/x.mkv", MaxHeight: 720,
				Segmented: &ffmpeg.Segmented{From: 600, KeyframeSeconds: 6},
			},
			want: []string{
				"-ss 600.000000 -i /srv/x.mkv",
				"-force_key_frames expr:gte(t,n_forced*6)",
				"-vf scale=-2:'min(720,ih)'",
				"-output_ts_offset 610.000000",
				"+frag_discont",
			},
			wantNot: []string{"-copyts", "-noaccurate_seek", "-seek_timestamp", "-frag_duration"},
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
			name: "MP3 with embedded cover: audio to AAC, cover excluded",
			source: generate(t, ffmpegPath, "album-art.mp3",
				"-map", "1:a:0", "-map", "0:v:0",
				"-c:a", "libmp3lame", "-c:v", "mjpeg", "-frames:v", "1",
				"-disposition:v:0", "attached_pic", "-f", "mp3"),
			spec: ffmpeg.StreamSpec{},
			want: "mov,mp4,m4a,3gp,3g2,mj2|aac",
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

func TestFirstStreamingFragmentIsPlayableBeforeTheLongSourceGopEnds(t *testing.T) {
	ffmpegPath, ffprobePath := toolchain(t)
	for _, tc := range []struct {
		name      string
		copyVideo bool
		codec     string
		start     float64
	}{
		{"copied AAC", true, "aac", 0},
		{"encoded AAC", false, "aac", 0},
		{"copied AC3 after seek", true, "ac3", 0.5},
		{"encoded AC3 after seek", false, "ac3", 0.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := generate(t, ffmpegPath, "long-gop.mp4", "-c:v", "libx264", "-preset", "ultrafast", "-g", "1000", "-sc_threshold", "0", "-c:a", tc.codec)
			streamer, err := ffmpeg.NewStreamer(ffmpeg.StreamerOptions{FFmpegPath: ffmpegPath})
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := streamer.Stream(t.Context(), ffmpeg.StreamSpec{Source: source, CopyVideo: tc.copyVideo, CopyAudio: true, Start: tc.start}, &out); err != nil {
				t.Fatal(err)
			}
			data := out.Bytes()
			fragments, firstEnd := 0, 0
			for offset := 0; offset < len(data); {
				if len(data)-offset < 8 {
					t.Fatal("truncated MP4 box")
				}
				size := int(binary.BigEndian.Uint32(data[offset : offset+4]))
				if size < 8 || size > len(data)-offset {
					t.Fatalf("invalid box size %d", size)
				}
				if string(data[offset+4:offset+8]) == "moof" {
					fragments++
				}
				if fragments == 1 && string(data[offset+4:offset+8]) == "mdat" {
					firstEnd = offset + size
				}
				offset += size
			}
			if fragments < 2 {
				t.Fatalf("only %d fragment: output still waits for the long GOP or EOF", fragments)
			}
			if firstEnd == 0 {
				t.Fatal("first fragment has no media")
			}
			prefix := filepath.Join(t.TempDir(), "first-fragment.mp4")
			if err := os.WriteFile(prefix, data[:firstEnd], 0o600); err != nil {
				t.Fatal(err)
			}
			if got := streamShape(t, ffprobePath, prefix); got != "mov,mp4,m4a,3gp,3g2,mj2|h264,"+tc.codec {
				t.Fatal(got)
			}
			if b, err := exec.CommandContext(t.Context(), ffmpegPath, "-v", "error", "-i", prefix, "-frames:v", "1", "-f", "null", "-").CombinedOutput(); err != nil {
				t.Fatalf("first fragment is not playable: %v %s", err, b)
			}
		})
	}
}

// The listing is matched by line shape: the legend above it is not a filter,
// and ffmpeg 6 and 7 print no separator line before the first one.
func TestParseFilterList(t *testing.T) {
	out := `Filters:
  T.. = Timeline support
  .S. = Slice threading
  ..C = Command support
  A = Audio input/output
  V = Video input/output
  N = Dynamic number and/or type of input/output
  | = Source or sink filter
 ... abench            A->A       Benchmark part of a filtergraph.
 .S. tonemap           V->V       Conversion to/from different dynamic ranges.
 .SC zscale            V->V       Apply resizing, colorspace and bit depth conversion.
 ... testsrc2          |->V       Generate another test pattern.
`
	got := strings.Join(ffmpeg.ParseFilterList(out), ",")
	if got != "abench,tonemap,zscale,testsrc2" {
		t.Errorf("filters = %q", got)
	}
}

// colourTags is ffprobe's transfer and primaries for a file's first video
// stream, "transfer/primaries".
func colourTags(t *testing.T, ffprobePath, path string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), ffprobePath, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name,pix_fmt,height,color_transfer,color_primaries,color_space",
		"-of", "default=nw=1", path).Output()
	if err != nil {
		t.Fatalf("probing %s: %v", path, err)
	}
	return strings.TrimSpace(string(out))
}

// A real HDR10 and HLG source through a real ffmpeg (#886). Without the
// tone-map, the eight-bit H.264 a phone receives is still tagged PQ BT.2020,
// which is the washed-out picture the issue reports; with it, the stream is
// BT.709 SDR and says so. The pinned build has zscale; a build without it is
// what ToneMaps reports, and this asserts the pinned one is not that build.
func TestStreamToneMapsHDRToTaggedSDR(t *testing.T) {
	ffmpegPath, ffprobePath := toolchain(t)
	s, err := ffmpeg.NewStreamer(ffmpeg.StreamerOptions{FFmpegPath: ffmpegPath})
	if err != nil {
		t.Fatal(err)
	}
	if !s.ToneMaps(t.Context()) {
		t.Fatal("the pinned ffmpeg reports no zscale/tonemap; the tone-map would never run")
	}

	sources := []struct {
		name     string
		transfer string
		toneMap  ffmpeg.ToneMap
	}{
		{"hdr10.mkv", "smpte2084", ffmpeg.ToneMapPQ},
		{"hlg.mkv", "arib-std-b67", ffmpeg.ToneMapHLG},
	}
	for _, src := range sources {
		t.Run(src.name, func(t *testing.T) {
			path := generate(t, ffmpegPath, src.name, "-map", "0:v", "-map", "1:a",
				"-vf", "scale=320:240,format=yuv420p10le", "-c:v", "libx265", "-x265-params", "log-level=error",
				"-color_primaries", "bt2020", "-color_trc", src.transfer, "-colorspace", "bt2020nc",
				"-c:a", "aac")
			if got := colourTags(t, ffprobePath, path); !strings.Contains(got, "color_transfer="+src.transfer) {
				t.Fatalf("the source is not what the test needs: %s", got)
			}

			stream := func(spec ffmpeg.StreamSpec) string {
				t.Helper()
				var buf bytes.Buffer
				if err := s.Stream(t.Context(), spec, &buf); err != nil {
					t.Fatal(err)
				}
				out := filepath.Join(t.TempDir(), "out.mp4")
				if err := os.WriteFile(out, buf.Bytes(), 0o600); err != nil {
					t.Fatal(err)
				}
				return colourTags(t, ffprobePath, out)
			}

			plain := stream(ffmpeg.StreamSpec{Source: path, MaxHeight: 180})
			if !strings.Contains(plain, "color_transfer="+src.transfer) {
				t.Errorf("control: an untone-mapped re-encode should still carry the source's transfer, got\n%s", plain)
			}
			mapped := stream(ffmpeg.StreamSpec{Source: path, MaxHeight: 180, ToneMap: src.toneMap})
			want := []string{
				"codec_name=h264", "pix_fmt=yuv420p", "height=180",
				"color_space=bt709", "color_transfer=bt709", "color_primaries=bt709",
			}
			for _, want := range want {
				if !strings.Contains(mapped, want) {
					t.Errorf("tone-mapped stream lacks %s:\n%s", want, mapped)
				}
			}
		})
	}
}
