package ffmpeg_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/media/ffmpeg"
)

// TestExtractRoundTripsAnEmbeddedTrack proves the extractor against the REAL
// pinned toolchain: mux a known SubRip track into a video, then List + Extract
// and get the same cues back. This is the end-to-end proof that the ffprobe
// enumeration and the ffmpeg `-map/-c:s srt` line actually work — a unit test
// of the arg builders cannot show that ffmpeg accepts them.
//
// Skipped when the toolchain is absent (ADR-0023): the mechanism is optional,
// so its test is too.
func TestExtractRoundTripsAnEmbeddedTrack(t *testing.T) {
	t.Parallel()

	ffmpegPath, ffprobePath := toolchain(t)
	dir := t.TempDir()

	// A tiny SubRip with a distinctive line so the extraction is unambiguous.
	srtBody := "1\n00:00:00,000 --> 00:00:01,000\nHello Yellowstone\n\n"
	srtPath := write(t, []byte(srtBody), "in.srt")

	// Mux a 1s generated video with that subtitle as an embedded SubRip track,
	// tagged English — the shape of the Yellowstone .mp4 (text subs in the
	// container). mpeg4 is chosen because every ffmpeg build has it.
	mkv := filepath.Join(dir, "movie.mkv")
	build := exec.CommandContext(t.Context(), ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=64x64:rate=5",
		"-i", srtPath,
		"-map", "0:v:0", "-map", "1:0",
		"-c:v", "mpeg4", "-c:s", "srt",
		"-metadata:s:s:0", "language=eng",
		mkv,
	)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the fixture video failed: %v\n%s", err, out)
	}

	ex, err := ffmpeg.NewExtractor(ffmpeg.ExtractorOptions{
		FFmpegPath: ffmpegPath, FFprobePath: ffprobePath, WorkDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	streams, err := ex.List(ctx, mkv)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(streams) != 1 {
		t.Fatalf("found %d text subtitle streams, want 1: %+v", len(streams), streams)
	}
	if streams[0].Language != "eng" {
		t.Errorf("language = %q, want eng", streams[0].Language)
	}
	if !ffmpeg.IsTextSubtitleCodec(streams[0].Codec) {
		t.Errorf("codec %q was listed but is not classified text", streams[0].Codec)
	}

	res, err := ex.Extract(ctx, mkv, streams[0])
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	defer func() { _ = os.Remove(res.Path) }()
	got, err := os.ReadFile(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "Hello Yellowstone") {
		t.Errorf("the extracted subtitle is missing its cue; got:\n%s", got)
	}
	if res.Size == 0 {
		t.Error("the extracted subtitle reported zero size")
	}
}
