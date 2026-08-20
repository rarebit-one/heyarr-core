package ffmpeg_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/media/ffmpeg"
	"github.com/rarebit-one/heyarr-core/internal/testutil/fixtures"
)

func toolchain(t *testing.T) (ffmpegPath, ffprobePath string) {
	t.Helper()
	dir := os.Getenv("HEYARR_TEST_TOOLCHAIN_DIR")
	if dir == "" {
		dir = filepath.Join("..", "..", "..", ".toolchain", "bin")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(abs, "ffmpeg")); err != nil {
		if p, lookErr := exec.LookPath("ffmpeg"); lookErr == nil {
			return p, "ffprobe"
		}
		t.Skip("no ffmpeg available; run scripts/toolchain.sh")
	}
	return filepath.Join(abs, "ffmpeg"), filepath.Join(abs, "ffprobe")
}

func write(t *testing.T, body []byte, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// streamChecksums asks ffmpeg for a per-stream hash of the ENCODED data.
//
// This is the assertion that makes "-c copy" mean something. A remux that
// silently re-encoded would produce a perfectly playable file with the right
// codecs, the right duration and the right resolution — and different bytes in
// every packet. Nothing short of hashing the streams catches it.
func streamChecksums(t *testing.T, ffmpegPath, path string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-i", path, "-map", "0", "-c", "copy", "-f", "streamhash", "-hash", "sha256", "-").Output()
	if err != nil {
		t.Fatalf("hashing the streams of %s: %v", path, err)
	}
	return strings.TrimSpace(string(out))
}

func probeCodecs(t *testing.T, ffprobePath, path string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), ffprobePath,
		"-v", "error", "-show_entries", "stream=codec_name", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("probing %s: %v", path, err)
	}
	return strings.TrimSpace(string(out))
}

// The REMUX case: the same streams, a different box.
func TestRemuxChangesTheContainerAndNothingElse(t *testing.T) {
	ffmpegPath, ffprobePath := toolchain(t)
	src := write(t, fixtures.SampleMKV(1), "in.mkv")

	r, err := ffmpeg.New(ffmpeg.Options{FFmpegPath: ffmpegPath, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Remux(t.Context(), src, ffmpeg.ContainerMP4)
	if err != nil {
		t.Fatalf("remux failed: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(res.Path) })

	if !strings.HasSuffix(res.Path, ".mp4") {
		t.Errorf("output = %q, want an .mp4", res.Path)
	}
	if res.Size == 0 {
		t.Error("the output is empty")
	}

	// The container changed.
	if got := probeContainer(t, ffprobePath, res.Path); !strings.Contains(got, "mp4") {
		t.Errorf("output container = %q, want mp4", got)
	}
	// The codecs did not.
	if before, after := probeCodecs(t, ffprobePath, src), probeCodecs(t, ffprobePath, res.Path); before != after {
		t.Errorf("codecs changed: %q then %q", before, after)
	}
	// And neither did a single encoded byte. This is the assertion that makes
	// "-c copy" mean something rather than being a flag nobody checked.
	if before, after := streamChecksums(t, ffmpegPath, src), streamChecksums(t, ffmpegPath, res.Path); before != after {
		t.Errorf("the encoded streams changed — this re-encoded rather than remuxing:\n  %s\n  %s",
			before, after)
	}
	t.Logf("remuxed %d bytes in %s", res.Size, res.Elapsed)
}

func probeContainer(t *testing.T, ffprobePath, path string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), ffprobePath,
		"-v", "error", "-show_entries", "format=format_name", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("probing %s: %v", path, err)
	}
	return strings.TrimSpace(string(out))
}

// Blobs are immutable (§14). A remux that wrote in place would corrupt the
// thing every other asset sharing those bytes points at.
func TestRemuxNeverTouchesTheInput(t *testing.T) {
	ffmpegPath, _ := toolchain(t)
	body := fixtures.SampleMKV(1)
	src := write(t, body, "in.mkv")

	r, err := ffmpeg.New(ffmpeg.Options{FFmpegPath: ffmpegPath, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Remux(t.Context(), src, ffmpeg.ContainerMP4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(res.Path) })

	after, err := os.ReadFile(src) //nolint:gosec // a path this test created
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(body) {
		t.Error("the input was modified")
	}
	if res.Path == src {
		t.Error("the output is the input")
	}
}

// A failed remux must leave nothing behind: a partial file is
// indistinguishable from a finished one to anything that only checks whether
// it exists.
func TestAFailedRemuxLeavesNoOutput(t *testing.T) {
	ffmpegPath, _ := toolchain(t)
	workDir := t.TempDir()
	src := write(t, []byte("this is not media at all"), "in.mkv")

	r, err := ffmpeg.New(ffmpeg.Options{FFmpegPath: ffmpegPath, WorkDir: workDir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Remux(t.Context(), src, ffmpeg.ContainerMP4); err == nil {
		t.Fatal("remuxing a text file succeeded")
	} else if !errors.Is(err, ffmpeg.ErrRemuxFailed) {
		t.Errorf("error = %v, want ErrRemuxFailed", err)
	}

	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("a failed remux left %v behind", names)
	}
}

// Every stream, not just the first of each kind.
//
// This needs a fixture with MORE THAN ONE audio track, and that is the whole
// point. The first version of this test used the ordinary two-stream fixture
// and passed with `-map 0` removed — ffmpeg's default mapping picks one video
// and one audio, which is exactly what that fixture has. A test that cannot
// distinguish the flag from its absence is not testing the flag.
//
// Dropping the second audio track or the subtitles would be a silent
// downgrade: the file plays, the codecs are right, and the commentary track is
// gone.
func TestRemuxKeepsEveryStream(t *testing.T) {
	ffmpegPath, ffprobePath := toolchain(t)
	src := multiTrack(t, ffmpegPath)

	if got := streamCount(t, ffprobePath, src); got != 3 {
		t.Fatalf("the fixture has %d streams, want 3 — this test cannot detect a dropped one otherwise", got)
	}

	r, err := ffmpeg.New(ffmpeg.Options{FFmpegPath: ffmpegPath, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Remux(t.Context(), src, ffmpeg.ContainerMatroska)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(res.Path) })

	if got := streamCount(t, ffprobePath, res.Path); got != 3 {
		t.Errorf("the remux kept %d of 3 streams — a dropped track is a silent downgrade", got)
	}
}

// multiTrack builds a file with one video and TWO audio tracks, which the
// committed fixtures deliberately do not have: they are small on purpose, and
// this is the one test that needs otherwise.
func multiTrack(t *testing.T, ffmpegPath string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "multi.mkv")
	src := write(t, fixtures.SampleMKV(1), "base.mkv")
	out, err := exec.CommandContext(t.Context(), ffmpegPath,
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-i", src,
		"-f", "lavfi", "-i", "sine=frequency=880:sample_rate=44100:duration=1",
		"-map", "0:v", "-map", "0:a", "-map", "1:a",
		"-c:v", "copy", "-c:a", "copy", "-c:a:1", "aac", "-b:a:1", "32k",
		path).CombinedOutput()
	if err != nil {
		t.Fatalf("building the multi-track fixture: %v\n%s", err, out)
	}
	return path
}

func streamCount(t *testing.T, ffprobePath, path string) int {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), ffprobePath,
		"-v", "error", "-show_entries", "stream=index", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("probing %s: %v", path, err)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

// A remux that can hang forever is a job slot lost forever.
func TestRemuxRespectsItsDeadline(t *testing.T) {
	ffmpegPath, _ := toolchain(t)
	src := write(t, fixtures.SampleMKV(1), "in.mkv")

	r, err := ffmpeg.New(ffmpeg.Options{
		FFmpegPath: ffmpegPath, WorkDir: t.TempDir(), Timeout: time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.Remux(t.Context(), src, ffmpeg.ContainerMP4)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a remux with a one-nanosecond deadline succeeded")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the remux did not return after its deadline; the subprocess leaked")
	}
}

func TestTargetFor(t *testing.T) {
	for _, tc := range []struct {
		declared []string
		want     ffmpeg.Container
	}{
		{[]string{"mp4"}, ffmpeg.ContainerMP4},
		{[]string{"mkv"}, ffmpeg.ContainerMatroska},
		// MP4 first when both are on offer: it is the more widely accepted and
		// the likelier to be hardware-decoded.
		{[]string{"mkv", "mp4"}, ffmpeg.ContainerMP4},
		{[]string{"MP4"}, ffmpeg.ContainerMP4},
		// A device declaring neither still gets something. Remuxing into a
		// container it did not ask for is a guess, and a better one than
		// leaving the file in a container it definitely refused.
		{[]string{"avi"}, ffmpeg.ContainerMP4},
		{nil, ffmpeg.ContainerMP4},
	} {
		if got := ffmpeg.TargetFor(tc.declared); got != tc.want {
			t.Errorf("TargetFor(%v) = %q, want %q", tc.declared, got, tc.want)
		}
	}
}

func TestParseContainerRejectsNonsense(t *testing.T) {
	if _, err := ffmpeg.ParseContainer("avi"); err == nil {
		t.Error("avi was accepted as a remux target")
	}
	for _, c := range ffmpeg.Containers() {
		if _, err := ffmpeg.ParseContainer(string(c)); err != nil {
			t.Errorf("%s is a container but does not parse: %v", c, err)
		}
	}
}
