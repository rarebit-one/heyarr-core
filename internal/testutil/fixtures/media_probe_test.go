package fixtures_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/testutil/fixtures"
)

// The assertion Milestone 1 could not make.
//
// M1's media fixtures were structurally valid containers no decoder had ever
// opened, because there was no ffprobe on any machine to check them against.
// Shipping a hand-built "decodable" MP4 nobody could verify would have
// surfaced in M2 looking like a probing bug rather than a fixture bug — so it
// was deliberately not done, and media.go said so.
//
// This is the payment. The committed bytes are handed to a real ffprobe and
// asserted to be what they claim.
//
// Skipped when the toolchain is absent, which is a supported configuration
// (ADR-0023). CI asserts on the Linux runners that these did not skip: a
// skipped test and a passing one are indistinguishable in the summary line,
// and this repository has already found six tests incapable of failing.

// probeStream is the fragment of ffprobe's JSON these tests read.
type probeStream struct {
	CodecName string `json:"codec_name"`
	CodecType string `json:"codec_type"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
}

type probeResult struct {
	Streams []probeStream `json:"streams"`
	Format  struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
	} `json:"format"`
}

// ffprobePath finds the pinned toolchain, or reports that there is none.
func ffprobePath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("HEYARR_TEST_FFPROBE"); p != "" {
		return p
	}
	local := filepath.Join("..", "..", "..", ".toolchain", "bin", "ffprobe")
	if _, err := os.Stat(local); err == nil {
		return local
	}
	if p, err := exec.LookPath("ffprobe"); err == nil {
		return p
	}
	t.Skip("no ffprobe available; run scripts/toolchain.sh")
	return ""
}

func probe(t *testing.T, body []byte, ext string) probeResult {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture"+ext)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	// #nosec G204 -- the binary is the pinned toolchain and the path is a
	// t.TempDir file this test just wrote.
	out, err := exec.CommandContext(t.Context(), ffprobePath(t),
		"-v", "error", "-print_format", "json",
		"-show_streams", "-show_format", path).Output()
	if err != nil {
		t.Fatalf("ffprobe rejected the fixture: %v", err)
	}
	var result probeResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// codecs returns the codec name for each stream type, so an assertion can read
// as "the video is h264" rather than as an index into a slice whose order is
// the muxer's business.
func codecs(r probeResult) map[string]string {
	out := map[string]string{}
	for _, s := range r.Streams {
		out[s.CodecType] = s.CodecName
	}
	return out
}

func TestTheCommittedFixturesAreRealMedia(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       []byte
		ext        string
		formatHas  string
		wantCodecs map[string]string
		wantW      int
		wantH      int
	}{
		{
			// DIRECT for almost any device.
			name: "h264+aac in mp4", body: fixtures.SampleMP4(1), ext: ".mp4",
			formatHas: "mp4", wantCodecs: map[string]string{"video": "h264", "audio": "aac"},
			wantW: 160, wantH: 120,
		},
		{
			name: "the second mp4", body: fixtures.SampleMP4(2), ext: ".mp4",
			formatHas: "mp4", wantCodecs: map[string]string{"video": "h264", "audio": "aac"},
			wantW: 160, wantH: 120,
		},
		{
			// REMUX: the same streams, a container an MP4-only device refuses.
			name: "h264+aac in matroska", body: fixtures.SampleMKV(1), ext: ".mkv",
			formatHas: "matroska", wantCodecs: map[string]string{"video": "h264", "audio": "aac"},
			wantW: 160, wantH: 120,
		},
		{
			// TRANSCODE: a codec an ordinary profile refuses.
			name: "hevc+aac in mp4", body: fixtures.SampleHEVCMP4(), ext: ".mp4",
			formatHas: "mp4", wantCodecs: map[string]string{"video": "hevc", "audio": "aac"},
			wantW: 160, wantH: 120,
		},
		{
			name: "flac", body: fixtures.SampleFLAC(), ext: ".flac",
			formatHas: "flac", wantCodecs: map[string]string{"audio": "flac"},
		},
		{
			name: "mp3", body: fixtures.SampleMP3(), ext: ".mp3",
			formatHas: "mp3", wantCodecs: map[string]string{"audio": "mp3"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := probe(t, tc.body, tc.ext)

			// The format name is a comma-separated list of everything the
			// demuxer answers to ("mov,mp4,m4a,3gp,3g2,mj2"), so this is a
			// membership check rather than equality — and the exact list
			// differs between the pinned 6.0 and 7.0.2 builds, which is
			// precisely the kind of thing an equality assertion would turn
			// into a platform-dependent failure.
			if !strings.Contains(got.Format.FormatName, tc.formatHas) {
				t.Errorf("format = %q, want it to include %q", got.Format.FormatName, tc.formatHas)
			}
			have := codecs(got)
			for kind, want := range tc.wantCodecs {
				if have[kind] != want {
					t.Errorf("%s codec = %q, want %q (all: %v)", kind, have[kind], want, have)
				}
			}
			if len(have) != len(tc.wantCodecs) {
				t.Errorf("streams = %v, want exactly %v", have, tc.wantCodecs)
			}
			if tc.wantW != 0 {
				for _, s := range got.Streams {
					if s.CodecType != "video" {
						continue
					}
					if s.Width != tc.wantW || s.Height != tc.wantH {
						t.Errorf("resolution = %dx%d, want %dx%d", s.Width, s.Height, tc.wantW, tc.wantH)
					}
				}
			}
			if got.Format.Duration == "" || strings.HasPrefix(got.Format.Duration, "0.0") {
				t.Errorf("duration = %q, which is not a file with content in it", got.Format.Duration)
			}
		})
	}
}

// The MP4 and MKV of a variant must be the same streams in different
// containers, or the REMUX case the planner is tested against is not a remux at
// all — it is two unrelated files that happen to be named similarly.
func TestTheMP4AndMKVFixturesAreTheSameStreams(t *testing.T) {
	mp4 := codecs(probe(t, fixtures.SampleMP4(1), ".mp4"))
	mkv := codecs(probe(t, fixtures.SampleMKV(1), ".mkv"))
	if fmt.Sprint(mp4) != fmt.Sprint(mkv) {
		t.Errorf("mp4 streams %v, mkv streams %v — these are supposed to be one remux apart", mp4, mkv)
	}
}

// Two variants that deduplicate would silently break the ingest fixtures, whose
// whole point is a pair that must NOT collapse to one blob and a third that
// must.
func TestTheTwoVariantsAreGenuinelyDifferentBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b []byte
	}{
		{"mp4", fixtures.SampleMP4(1), fixtures.SampleMP4(2)},
		{"mkv", fixtures.SampleMKV(1), fixtures.SampleMKV(2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if digest(tc.a) == digest(tc.b) {
				t.Error("the two variants are byte-identical; the dedup fixtures would stop testing dedup")
			}
		})
	}
}

// A caller that appends to a returned slice must not corrupt the embedded bytes
// for every later test in the process. Embedded data is shared and immutable;
// a slice over it is neither.
func TestSamplesAreCopiesNotViewsOfTheEmbeddedBytes(t *testing.T) {
	before := digest(fixtures.SampleMP4(1))
	scribble := fixtures.SampleMP4(1)
	for i := range scribble {
		scribble[i] = 0
	}
	if digest(fixtures.SampleMP4(1)) != before {
		t.Fatal("mutating one caller's slice changed the fixture for everyone else")
	}
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Regenerating twice on one machine must produce identical bytes.
//
// Without it, a fixture silently changes under every regeneration and
// invalidates every golden file downstream of it — and the first two attempts
// at this generator failed exactly that way: Matroska embeds a random
// SegmentUID, and `-fflags +bitexact` before `-i` configures the input demuxer
// rather than the muxer that writes it. The .mp4 files were stable and the
// .mkv files were not, which is a difference nobody notices by reading.
//
// It deliberately does NOT assert that regeneration reproduces the COMMITTED
// bytes on every platform. It cannot: the pinned toolchain is 7.0.2-static on
// linux and 6.0 on darwin (ADR-0023), so a cross-version comparison would fail
// for a reason that has nothing to do with determinism. It makes that stronger
// claim only where the local toolchain matches the recorded generator.
func TestRegeneratingTheFixturesIsDeterministic(t *testing.T) {
	ffprobePath(t) // skips when there is no toolchain
	script := filepath.Join("..", "..", "..", "scripts", "genmedia.sh")
	if _, err := os.Stat(script); err != nil {
		t.Skip("no generator script here")
	}

	runs := make([]map[string]string, 2)
	for i := range runs {
		dir := t.TempDir()
		// The script cd's to the repo root itself, so it is invoked by a path
		// relative to that root rather than to this package.
		cmd := exec.CommandContext(t.Context(), "bash", "scripts/genmedia.sh") // #nosec G204 -- a committed script in this repo
		cmd.Dir = filepath.Join("..", "..", "..")
		cmd.Env = append(os.Environ(), "GENMEDIA_OUT="+dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("regenerating failed: %v\n%s", err, out)
		}
		runs[i] = digestDir(t, dir)
	}

	if len(runs[0]) == 0 {
		t.Fatal("the generator produced nothing")
	}
	for name, first := range runs[0] {
		if second := runs[1][name]; second != first {
			t.Errorf("%s differs between two runs of the generator on one machine:\n  %s\n  %s",
				name, first, second)
		}
	}

	// The stronger claim, where it is available.
	generated := strings.TrimSpace(string(mustRead(t,
		filepath.Join("..", "..", "..", "internal", "testutil", "fixtures", "media", "GENERATED_BY"))))
	local := localFFmpegVersion(t)
	if local != generated {
		t.Logf("committed fixtures were generated by ffmpeg %s; this machine has %s — "+
			"skipping the committed-bytes comparison (ADR-0023's per-platform pin)", generated, local)
		return
	}
	committed := digestDir(t, filepath.Join("..", "..", "..", "internal", "testutil", "fixtures", "media"))
	for name, want := range runs[0] {
		if got := committed[name]; got != want {
			t.Errorf("%s: regeneration does not reproduce the committed bytes on the platform that made them\n"+
				"  committed   %s\n  regenerated %s", name, got, want)
		}
	}
}

func digestDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || e.Name() == "GENERATED_BY" {
			continue
		}
		out[e.Name()] = digest(mustRead(t, filepath.Join(dir, e.Name())))
	}
	return out
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- test-controlled paths inside this repo
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func localFFmpegVersion(t *testing.T) string {
	t.Helper()
	path := strings.TrimSuffix(ffprobePath(t), "ffprobe") + "ffmpeg"
	// #nosec G204 -- the pinned toolchain beside the ffprobe already resolved
	out, err := exec.CommandContext(t.Context(), path, "-version").Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(strings.SplitN(string(out), "\n", 2)[0])
	if len(fields) < 3 {
		return ""
	}
	return fields[2]
}
