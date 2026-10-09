//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/media/ffmpeg"
	"github.com/rarebit-one/heyarr-core/internal/media/probe"
)

// The preferred track's codec, not the first track's codec, decides the leg.
func TestPreferredAudioChangesCodecNegotiation(t *testing.T) {
	h := newHarness(t, withStreamLeg(&fakeStreamer{}, newFakeBlobs(t), nil)).seed()
	h.seedProbe(blob1Hash, "mov,mp4,m4a,3gp,3g2,mj2", "h264", "aac", 1080)
	h.exec(`UPDATE blob_probes SET streams = ? WHERE blob_hash = ?`, `[{"index":0,"type":"video","codec":"h264","height":1080},{"index":1,"type":"audio","codec":"aac","language":"ita","channels":2},{"index":2,"type":"audio","codec":"ac3","language":"eng","channels":6}]`, blob1Hash)
	resp, p := h.planForClient(t, "", asset1ID, `{"containers":["mp4"],"video":["h264"],"audio":["aac"],"audio_languages":["en"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	if p.Mode != "stream" || p.Source == nil || p.Source.Audio != "ac3" {
		t.Fatalf("preferred English AC-3 must stream, got mode=%q source=%+v", p.Mode, p.Source)
	}
}

func TestPreferredAudioIsInTheProducedStreamAfterASeek(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	blobs := newFakeBlobs(t)
	source := blobs.paths[blob1Hash]
	args := []string{"-v", "error", "-nostdin", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x120:rate=10", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-f", "lavfi", "-i", "sine=frequency=880:sample_rate=48000", "-map", "0:v", "-map", "1:a", "-map", "2:a", "-t", "2", "-c:v", "libx264", "-preset", "ultrafast", "-c:a:0", "aac", "-c:a:1", "ac3", "-metadata:s:a:0", "language=ita", "-metadata:s:a:1", "language=eng", "-f", "matroska", source}
	if out, err := exec.CommandContext(t.Context(), ffmpegPath, args...).CombinedOutput(); err != nil {
		t.Fatalf("generate: %v %s", err, out)
	}
	streamer, err := ffmpeg.NewStreamer(ffmpeg.StreamerOptions{FFmpegPath: ffmpegPath})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, withStreamLeg(streamer, blobs, nil)).seed()
	h.seedProbe(blob1Hash, "matroska,webm", "h264", "aac", 120)
	h.exec(`UPDATE blob_probes SET streams=? WHERE blob_hash=?`, `[{"index":0,"type":"video","codec":"h264","height":120},{"index":1,"type":"audio","codec":"aac","language":"ita"},{"index":2,"type":"audio","codec":"ac3","language":"eng"}]`, blob1Hash)
	_, p := h.planForClient(t, "", asset1ID, `{"containers":["mp4"],"video":["h264"],"audio":["aac"],"audio_languages":["eng"]}`)
	if p.Mode != "stream" {
		t.Fatalf("mode=%q", p.Mode)
	}
	for _, start := range []string{"0", "1"} {
		resp := h.do(http.MethodGet, p.URL+"?start="+start, "", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream status=%d %s", resp.StatusCode, h.body(resp))
		}
		path := filepath.Join(t.TempDir(), "stream.mp4")
		if err := os.WriteFile(path, h.body(resp), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.CommandContext(t.Context(), ffprobePath, "-v", "error", "-select_streams", "a", "-show_entries", "stream=codec_name:stream_tags=language", "-of", "json", path).Output()
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		var parsed struct {
			Streams []struct {
				Codec string            `json:"codec_name"`
				Tags  map[string]string `json:"tags"`
			} `json:"streams"`
		}
		if err := json.NewDecoder(bytes.NewReader(out)).Decode(&parsed); err != nil {
			t.Fatal(err)
		}
		if len(parsed.Streams) != 1 || parsed.Streams[0].Codec != "aac" || parsed.Streams[0].Tags["language"] != "eng" {
			t.Fatalf("start=%s output=%s", start, out)
		}
	}
}

func TestNodeAudioPreferenceCanBeOverriddenAndPreservesOriginals(t *testing.T) {
	h := newHarness(t, withStreamLeg(&fakeStreamer{}, newFakeBlobs(t), nil), func(hc *harnessConfig) { hc.cfg.Media.AudioLanguages = []string{"en"} }).seed()
	h.seedProbe(blob1Hash, "mp4", "h264", "aac", 1080)
	h.exec(`UPDATE blob_probes SET streams=?WHERE blob_hash=?`, `[{"index":0,"type":"video","codec":"h264"},{"index":1,"type":"audio","codec":"aac","language":"ita"},{"index":2,"type":"audio","codec":"ac3","language":"eng"}]`, blob1Hash)
	for _, tc := range []struct{ suffix, want string }{
		{"", "ac3"}, {`,"audio_languages":[]`, "aac"}, {`,"audio_languages":["it"]`, "aac"}, {`,"audio_languages":["zh"]`, "aac"},
	} {
		resp, p := h.planForClient(t, "", asset1ID, `{"containers":["mp4"],"video":["h264"],"audio":["aac"]`+tc.suffix+`}`)
		if resp.StatusCode != 200 || p.Source == nil || p.Source.Audio != tc.want {
			t.Fatalf("%s status=%d source=%+v", tc.suffix, resp.StatusCode, p.Source)
		}
	}
	resp, _ := h.planForClient(t, "", asset1ID, `{"audio_languages":["--invalid--"]}`)
	if resp.StatusCode != 400 {
		t.Fatalf("invalid language status=%d", resp.StatusCode)
	}
}

func TestOnDemandProbeUsesTheSamePreferredAudioAsCachedProbe(t *testing.T) {
	prober := &fakeProber{result: probe.Result{Container: "mp4", Streams: []probe.Stream{{Index: 0, Type: "video", Codec: "h264"}, {Index: 1, Type: "audio", Codec: "aac", Language: "ita"}, {Index: 2, Type: "audio", Codec: "ac3", Language: "eng"}}}}
	h := newHarness(t, withStreamLeg(&fakeStreamer{}, newFakeBlobs(t), prober)).seed()
	for range 2 {
		resp, p := h.planForClient(t, "", asset1ID, `{"containers":["mp4"],"video":["h264"],"audio":["aac"],"audio_languages":["en"]}`)
		if resp.StatusCode != 200 || p.Source == nil || p.Source.Audio != "ac3" || p.Mode != "stream" {
			t.Fatalf("status=%d plan=%+v", resp.StatusCode, p)
		}
	}
	if prober.calls != 1 {
		t.Fatalf("probe calls=%d, want one then cached selection", prober.calls)
	}
}
