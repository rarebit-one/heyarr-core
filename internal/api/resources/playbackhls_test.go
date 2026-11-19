//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"context"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/media/ffmpeg"
	"github.com/rarebit-one/heyarr-core/internal/media/hls"
)

// fakeTiming answers a fixed timing and records what was warmed, so the route
// can be tested without ffprobe. The cutting itself is exercised against a
// real ffmpeg in internal/media/hls.
type fakeTiming struct {
	t      hls.Timing
	mu     sync.Mutex
	warmed []string
}

func (f *fakeTiming) Timing(context.Context, string, string, bool) (hls.Timing, error) {
	return f.t, nil
}

func (f *fakeTiming) Warm(blob, path string, keyframes bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.warmed = append(f.warmed, blob+"|"+filepath.Base(path)+"|"+map[bool]string{true: "keyframes", false: "timing"}[keyframes])
}

func everySeconds(step, until float64) []float64 {
	var out []float64
	for t := 0.0; t < until; t += step {
		out = append(out, t)
	}
	return out
}

func newPresenter(t *testing.T, runner hls.Runner, timing hls.TimingSource) *hls.Manager {
	t.Helper()
	m, err := hls.New(hls.Options{Root: filepath.Join(t.TempDir(), "hls"), Runner: runner, Index: timing})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

// A stream plan offers the same repackage as HLS, and the presentation is
// fetched with no credential at all — on a node whose API demands one.
func TestAStreamPlanOffersASeekablePresentationFetchedWithoutACredential(t *testing.T) {
	t.Parallel()
	timing := &fakeTiming{t: hls.Timing{Duration: 2700, Keyframes: everySeconds(2, 2700)}}
	h := newHarness(t, withAuth, withStreamLeg(&fakeStreamer{}, newFakeBlobs(t), nil),
		withHLS(newPresenter(t, &fakeStreamer{}, timing))).seed()
	h.seedProbe(blob1Hash, "matroska,webm", "h264", "ac3", 1080)
	phone := h.mint("phone", auth.ScopeRead)

	resp, p := h.planForClient(t, phone.Secret, asset1ID, phoneNoAC3)
	if resp.StatusCode != http.StatusOK || p.Mode != "stream" {
		t.Fatalf("status = %d mode = %q: %s", resp.StatusCode, p.Mode, h.body(resp))
	}
	if !strings.HasPrefix(p.HLSURL, "/render/hls/") || !strings.HasSuffix(p.HLSURL, "/index.m3u8") {
		t.Fatalf("hls_url = %q, want /render/hls/{capability}/index.m3u8", p.HLSURL)
	}
	if p.HLSMIME != "application/vnd.apple.mpegurl" {
		t.Errorf("hls_mime = %q", p.HLSMIME)
	}
	// The live stream is still what url names, for every client that does
	// not know the new field.
	if !strings.HasPrefix(p.URL, "/api/v1/playback/stream/") {
		t.Errorf("url = %q; the stream route must stay", p.URL)
	}
	// Planning started the keyframe read the playlist will need.
	if want := blob1Hash + "|" + strings.TrimPrefix(blob1Hash, "blake3:") + "|keyframes"; len(timing.warmed) != 1 || timing.warmed[0] != want {
		t.Errorf("warmed = %v, want [%s]", timing.warmed, want)
	}

	pl := h.do(http.MethodGet, p.HLSURL, "", nil)
	if pl.StatusCode != http.StatusOK {
		t.Fatalf("playlist status = %d: %s", pl.StatusCode, h.body(pl))
	}
	for k, want := range map[string]string{
		"Content-Type": "application/vnd.apple.mpegurl", "Access-Control-Allow-Origin": "*",
	} {
		if got := pl.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	body := string(h.body(pl))
	for _, want := range []string{"#EXT-X-PLAYLIST-TYPE:VOD", "#EXT-X-MAP:URI=\"init.mp4\"", "\n449.m4s\n", "#EXT-X-ENDLIST"} {
		if !strings.Contains(body, want) {
			t.Errorf("the playlist lacks %q:\n%.300s", want, body)
		}
	}
	if n := strings.Count(body, "#EXTINF:"); n != 450 {
		t.Errorf("a 2700 s film was listed as %d segments, want 450", n)
	}

	// A Cast receiver's preflight is answered too.
	pre := h.do(http.MethodOptions, strings.TrimSuffix(p.HLSURL, "index.m3u8")+"0.m4s", "", nil)
	if pre.StatusCode != http.StatusNoContent || pre.Header.Get("Access-Control-Allow-Origin") != "*" ||
		!strings.Contains(pre.Header.Get("Access-Control-Allow-Methods"), "GET") {
		t.Errorf("preflight = %d %v", pre.StatusCode, pre.Header)
	}
}

// Every refusal is one opaque 404, and the two kinds of token are not
// interchangeable.
func TestAnHLSCapabilityIsRefusedOpaquely(t *testing.T) {
	t.Parallel()
	blobs := newFakeBlobs(t)
	h := newHarness(t, withAuth, withStreamLeg(&fakeStreamer{}, blobs, nil),
		withHLS(newPresenter(t, &fakeStreamer{}, &fakeTiming{t: hls.Timing{Duration: 60, Keyframes: everySeconds(2, 60)}}))).seed()
	h.seedProbe(blob1Hash, "matroska,webm", "h264", "ac3", 1080)
	phone := h.mint("phone", auth.ScopeRead)
	_, p := h.planForClient(t, phone.Secret, asset1ID, phoneNoAC3)
	if p.HLSURL == "" {
		t.Fatal("the plan offered no hls_url")
	}
	capability := strings.TrimSuffix(strings.TrimPrefix(p.HLSURL, "/render/hls/"), "/index.m3u8")
	streamToken := strings.TrimPrefix(p.URL, "/api/v1/playback/stream/")

	cases := []struct {
		name, url string
	}{
		{"a tampered capability", "/render/hls/" + capability + "x/index.m3u8"},
		{"a made-up capability", "/render/hls/h1.not.a.capability/index.m3u8"},
		{"a stream token presented as a capability", "/render/hls/" + streamToken + "/index.m3u8"},
		{"a segment the playlist does not list", "/render/hls/" + capability + "/99999.m4s"},
		{"a segment name that is not a number", "/render/hls/" + capability + "/01.m4s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := h.do(http.MethodGet, tc.url, "", nil)
			body := string(h.body(r))
			if r.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404: %s", r.StatusCode, body)
			}
			// The refusal never echoes the secret it refused.
			if strings.Contains(body, capability) || strings.Contains(body, streamToken) {
				t.Errorf("the refusal echoes the capability: %s", body)
			}
		})
	}
	// Nor does the stream route take a capability, even from the credential
	// that planned it.
	if r := h.do(http.MethodGet, "/api/v1/playback/stream/"+capability, phone.Secret, nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("the stream route accepted an hls capability: %d", r.StatusCode)
	}

	// It lives for the film (45 minutes here) and six hours, not the stream token's hour.
	h.clock.t = fixedTime.Add(2 * time.Hour)
	if r := h.do(http.MethodGet, p.HLSURL, "", nil); r.StatusCode != http.StatusOK {
		t.Errorf("two hours in, the capability answered %d, want 200", r.StatusCode)
	}
	h.clock.t = fixedTime.Add(6*time.Hour + 46*time.Minute)
	if r := h.do(http.MethodGet, p.HLSURL, "", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("an expired capability answered %d, want 404", r.StatusCode)
	}

	// A capability for a blob this node no longer holds.
	h.clock.t = fixedTime
	delete(blobs.paths, blob1Hash)
	if r := h.do(http.MethodGet, p.HLSURL, "", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("a capability for a blob that is gone answered %d", r.StatusCode)
	}
}

// A node that cannot present offers no playlist, and the route is still there
// to refuse one.
func TestWithoutAPresenterThePlanOffersNoHLS(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withStreamLeg(&fakeStreamer{}, newFakeBlobs(t), nil)).seed()
	h.seedProbe(blob1Hash, "matroska,webm", "h264", "ac3", 1080)
	_, p := h.planForClient(t, "", asset1ID, phoneNoAC3)
	if p.Mode != "stream" || p.HLSURL != "" || p.HLSMIME != "" {
		t.Errorf("mode = %q hls_url = %q hls_mime = %q; want a stream with no presentation", p.Mode, p.HLSURL, p.HLSMIME)
	}
	if r := h.get("/render/hls/h1.x.y/index.m3u8"); r.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", r.StatusCode)
	}
}

// A source with no picture still streams, but is offered no playlist: the
// segments are cut on video keyframes it does not have.
func TestAnAudioOnlyStreamPlanOffersNoHLS(t *testing.T) {
	t.Parallel()
	timing := &fakeTiming{t: hls.Timing{Duration: 240}}
	h := newHarness(t, withStreamLeg(&fakeStreamer{}, newFakeBlobs(t), nil),
		withHLS(newPresenter(t, &fakeStreamer{}, timing))).seed()
	h.exec(`INSERT INTO blob_probes (blob_hash, container, format_long, duration_seconds, bitrate_bps, streams, bytes_read, materialised, probed_at)
		VALUES (?, 'flac', '', 240, 900000, '[{"index":0,"type":"audio","codec":"flac","channels":2}]', 0, 0, ?)`,
		blob1Hash, seedTime)

	resp, p := h.planForClient(t, "", asset1ID, phoneNoAC3)
	if resp.StatusCode != http.StatusOK || p.Mode != "stream" {
		t.Fatalf("status = %d mode = %q, want a stream for FLAC to a client without it: %s", resp.StatusCode, p.Mode, h.body(resp))
	}
	if p.HLSURL != "" || p.HLSMIME != "" {
		t.Errorf("hls_url = %q hls_mime = %q; a source with no picture has no presentation", p.HLSURL, p.HLSMIME)
	}
	if len(timing.warmed) != 0 {
		t.Errorf("planning warmed %v for a presentation it did not offer", timing.warmed)
	}
}

// The whole path with a real ffmpeg: plan, playlist, init, the first segment
// and one near the end, through the routes a Cast receiver uses.
func TestAnHLSPresentationIsServedEndToEnd(t *testing.T) {
	t.Parallel()
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	src := filepath.Join(t.TempDir(), "film.mkv")
	if out, err := exec.CommandContext(t.Context(), ffmpegPath, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=160x120:rate=24", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", "30", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "48",
		"-c:a", "ac3", src).CombinedOutput(); err != nil {
		t.Fatalf("generating the film: %v\n%s", err, out)
	}
	streamer, err := ffmpeg.NewStreamer(ffmpeg.StreamerOptions{FFmpegPath: ffmpegPath})
	if err != nil {
		t.Fatal(err)
	}
	index, err := hls.NewIndexer(ffprobePath, nil)
	if err != nil {
		t.Fatal(err)
	}
	blobs := fakeBlobs{paths: map[string]string{blob1Hash: src}}
	h := newHarness(t, withStreamLeg(streamer, blobs, nil), withHLS(newPresenter(t, streamer, index))).seed()
	h.seedProbe(blob1Hash, "matroska,webm", "h264", "ac3", 120)

	_, p := h.planForClient(t, "", asset1ID, phoneNoAC3)
	if p.HLSURL == "" {
		t.Fatalf("no hls_url on %+v", p)
	}
	base := strings.TrimSuffix(p.HLSURL, "index.m3u8")
	pl := h.get(p.HLSURL)
	body := string(h.body(pl))
	segments := strings.Count(body, "#EXTINF:")
	if pl.StatusCode != http.StatusOK || segments != 5 {
		t.Fatalf("playlist %d with %d segments, want 200 with 5 (a 30 s film in 6 s segments):\n%s", pl.StatusCode, segments, body)
	}
	init := h.get(base + "init.mp4")
	if b := h.body(init); init.StatusCode != http.StatusOK || len(b) < 8 || string(b[4:8]) != "ftyp" {
		t.Fatalf("init: %d, %d bytes", init.StatusCode, len(b))
	}
	for _, n := range []string{"0", "4"} {
		seg := h.get(base + n + ".m4s")
		b := h.body(seg)
		if seg.StatusCode != http.StatusOK || seg.Header.Get("Content-Type") != "video/mp4" || len(b) < 8 || string(b[4:8]) != "moof" {
			t.Errorf("segment %s: %d %q, %d bytes", n, seg.StatusCode, seg.Header.Get("Content-Type"), len(b))
		}
		if seg.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("segment %s has no CORS header", n)
		}
	}
}
