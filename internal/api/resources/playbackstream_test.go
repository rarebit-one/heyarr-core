//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/media/ffmpeg"
	"github.com/rarebit-one/heyarr-core/internal/media/probe"
)

// fakeStreamer stands in for ffmpeg: it writes a recognisable body and
// records the spec it was handed and whether the request context ended. The
// real ffmpeg is exercised in internal/media/ffmpeg; what is under test here
// is the route — the token, the headers, the cap, and that a client going
// away reaches the streamer.
type fakeStreamer struct {
	mu        sync.Mutex
	specs     []ffmpeg.StreamSpec
	busy      bool
	block     bool
	cancelled chan struct{}
	active    int
}

const fakeBody = "ftypisom....moov....moof....mdat"

func (f *fakeStreamer) Stream(ctx context.Context, spec ffmpeg.StreamSpec, w io.Writer) error {
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	busy, block := f.busy, f.block
	f.active++
	f.mu.Unlock()
	defer func() { f.mu.Lock(); f.active--; f.mu.Unlock() }()
	if busy {
		return ffmpeg.ErrStreamBusy
	}
	if _, err := w.Write([]byte(fakeBody)); err != nil {
		return err
	}
	if block {
		<-ctx.Done()
		close(f.cancelled)
		return ctx.Err()
	}
	return nil
}

func (f *fakeStreamer) Active() int { f.mu.Lock(); defer f.mu.Unlock(); return f.active }

func (f *fakeStreamer) last(t *testing.T) ffmpeg.StreamSpec {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.specs) == 0 {
		t.Fatal("the streamer was never asked")
	}
	return f.specs[len(f.specs)-1]
}

// fakeBlobs locates the seeded blobs at files in a temp dir.
type fakeBlobs struct{ paths map[string]string }

func (b fakeBlobs) SourcePath(_ context.Context, hash string) (string, error) {
	p, ok := b.paths[hash]
	if !ok {
		return "", errors.New("not held here")
	}
	return p, nil
}

// fakeProber answers a canned result and counts calls.
type fakeProber struct {
	result probe.Result
	calls  int
}

func (p *fakeProber) ProbePath(context.Context, string) (probe.Result, probe.Stats, error) {
	p.calls++
	return p.result, probe.Stats{}, nil
}

func newFakeBlobs(t *testing.T) fakeBlobs {
	t.Helper()
	dir := t.TempDir()
	paths := map[string]string{}
	for _, h := range []string{blob1Hash, blob2Hash} {
		p := filepath.Join(dir, strings.TrimPrefix(h, "blake3:"))
		if err := os.WriteFile(p, []byte("bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		paths[h] = p
	}
	return fakeBlobs{paths: paths}
}

// seedProbe records what "ffprobe found" for the seeded MKV: h264 with AC-3
// 5.1 — the #432 shape, in Matroska.
func (h *harness) seedProbe(blobHash, container, video, audio string, height int) {
	h.exec(`INSERT INTO blob_probes (blob_hash, container, format_long, duration_seconds, bitrate_bps, streams, bytes_read, materialised, probed_at)
		VALUES (?, ?, '', 2700, 6000000, ?, 0, 0, ?)`,
		blobHash, container,
		fmt.Sprintf(`[{"index":0,"type":"video","codec":%q,"width":%d,"height":%d},{"index":1,"type":"audio","codec":%q,"channels":6}]`,
			video, height*16/9, height, audio),
		seedTime)
}

type clientPlan struct {
	plan
	DeviceID string `json:"device_id"`
	Mode     string `json:"mode"`
	URL      string `json:"url"`
	MIME     string `json:"mime"`
	Reason   string `json:"reason"`
	Source   *struct {
		Container string `json:"container"`
		Video     string `json:"video"`
		Audio     string `json:"audio"`
		Height    int    `json:"height"`
	} `json:"source"`
}

func (h *harness) planForClient(t *testing.T, token, assetID, client string) (*http.Response, clientPlan) {
	t.Helper()
	resp := h.do(http.MethodPost, "/api/v1/playback/plan", token,
		strings.NewReader(fmt.Sprintf(`{"asset_id":%q,"client":%s}`, assetID, client)))
	var p clientPlan
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(h.body(resp), &p); err != nil {
			t.Fatal(err)
		}
	}
	return resp, p
}

const phoneNoAC3 = `{"containers":["mp4","mkv","webm"],"video":["h264","hevc","vp9","av1"],"audio":["aac","opus","mp3","eac3"],"max_height":1080}`

// The #432 case end to end at the API: an AC-3 file, a phone with no AC-3
// decoder, a stream planned, and the stream fetched.
func TestAClientThatCannotDecodeTheAudioIsServedAStream(t *testing.T) {
	streamer := &fakeStreamer{}
	h := newHarness(t, withStreamLeg(streamer, newFakeBlobs(t), nil)).seed()
	h.seedProbe(blob1Hash, "matroska,webm", "h264", "ac3", 1080)

	resp, p := h.planForClient(t, "", asset1ID, phoneNoAC3)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	if p.Mode != "stream" {
		t.Fatalf("mode = %q, want stream (reason %q)", p.Mode, p.Reason)
	}
	if !strings.HasPrefix(p.URL, "/api/v1/playback/stream/") {
		t.Errorf("url = %q, want the stream route", p.URL)
	}
	if p.MIME != "video/mp4" {
		t.Errorf("mime = %q", p.MIME)
	}
	if p.Reason != "audio ac3 not decodable by client" {
		t.Errorf("reason = %q", p.Reason)
	}
	if p.Source == nil || p.Source.Audio != "ac3" || p.Source.Video != "h264" || p.Source.Height != 1080 {
		t.Errorf("source = %+v", p.Source)
	}
	// The planner's own view rides along, so a client that already branches
	// on decision keeps working.
	if p.Decision != "transcode" || !p.has("audio_codec_unsupported") {
		t.Errorf("decision = %q reasons = %+v", p.Decision, p.Reasons)
	}
	if p.ContentURL != "" {
		t.Errorf("content_url = %q on a transcode plan; the original would be played", p.ContentURL)
	}

	// Fetch it.
	sresp := h.get(p.URL)
	if sresp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d: %s", sresp.StatusCode, h.body(sresp))
	}
	for k, want := range map[string]string{
		"Content-Type": "video/mp4", "Cache-Control": "no-store", "Accept-Ranges": "none",
		"X-Accel-Buffering": "no", "X-Content-Type-Options": "nosniff",
	} {
		if got := sresp.Header.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if sresp.Header.Get("Content-Length") != "" {
		t.Errorf("a stream declared a length: %s", sresp.Header.Get("Content-Length"))
	}
	if string(h.body(sresp)) != fakeBody {
		t.Errorf("body = %q", h.body(sresp))
	}
	spec := streamer.last(t)
	if !spec.CopyVideo || spec.CopyAudio || spec.MaxHeight != 0 || spec.Start != 0 {
		t.Errorf("the streamer was asked for %+v; want video copied, audio transcoded, no cap", spec)
	}
	if !strings.HasSuffix(spec.Source, strings.TrimPrefix(blob1Hash, "blake3:")) {
		t.Errorf("source = %q, want the blob's path", spec.Source)
	}

	// ?start restarts from an offset; a bad one is a 400 before anything runs.
	if r := h.get(p.URL + "?start=61.5"); r.StatusCode != http.StatusOK || streamer.last(t).Start != 61.5 {
		t.Errorf("start: status %d, spec %+v", r.StatusCode, streamer.last(t))
	}
	for _, bad := range []string{"abc", "-1", "NaN"} {
		if r := h.get(p.URL + "?start=" + bad); r.StatusCode != http.StatusBadRequest {
			t.Errorf("start=%s: status %d, want 400", bad, r.StatusCode)
		}
	}
}

// The same file for a client that declares AC-3 is the bytes, unchanged.
func TestAClientThatDecodesTheSourceIsHandedTheBlob(t *testing.T) {
	streamer := &fakeStreamer{}
	h := newHarness(t, withStreamLeg(streamer, newFakeBlobs(t), nil)).seed()
	h.seedProbe(blob1Hash, "matroska,webm", "h264", "ac3", 1080)

	resp, p := h.planForClient(t, "", asset1ID,
		`{"containers":["mp4","mkv"],"video":["h264"],"audio":["ac3","aac"],"max_height":1080}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	if p.Mode != "direct" || p.URL != "/api/v1/blobs/"+blob1Hash+"/content" || p.Reason != "" {
		t.Errorf("plan = mode %q url %q reason %q", p.Mode, p.URL, p.Reason)
	}
	if p.MIME != "video/x-matroska" {
		t.Errorf("mime = %q, want the asset's declared type", p.MIME)
	}
	if p.Decision != "direct" || p.ContentURL != p.URL {
		t.Errorf("decision = %q content_url = %q", p.Decision, p.ContentURL)
	}
	if len(streamer.specs) != 0 {
		t.Error("a direct plan touched the streamer")
	}
}

// Every refusal of the token is one opaque 404: another credential's token,
// a tampered one, nothing.
func TestAStreamTokenIsBoundToTheCredentialThatPlannedIt(t *testing.T) {
	streamer := &fakeStreamer{}
	h := newHarness(t, withAuth, withStreamLeg(streamer, newFakeBlobs(t), nil)).seed()
	h.seedProbe(blob1Hash, "matroska,webm", "h264", "ac3", 1080)
	phone := h.mint("phone", auth.ScopeRead)
	other := h.mint("other", auth.ScopeRead)

	resp, p := h.planForClient(t, phone.Secret, asset1ID, phoneNoAC3)
	if resp.StatusCode != http.StatusOK || p.Mode != "stream" {
		t.Fatalf("status = %d mode = %q: %s", resp.StatusCode, p.Mode, h.body(resp))
	}

	cases := []struct {
		name  string
		token string
		url   string
		want  int
	}{
		{"the credential that planned it", phone.Secret, p.URL, http.StatusOK},
		{"another read credential", other.Secret, p.URL, http.StatusNotFound},
		{"a tampered token", phone.Secret, p.URL + "x", http.StatusNotFound},
		{"a made-up token", phone.Secret, "/api/v1/playback/stream/not.a.token", http.StatusNotFound},
		{"no credential at all", "", p.URL, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := h.do(http.MethodGet, tc.url, tc.token, nil)
			if r.StatusCode != tc.want {
				t.Errorf("status = %d, want %d: %s", r.StatusCode, tc.want, h.body(r))
			}
		})
	}
	// Expiry: the clock moves past the TTL and the same token is refused.
	h.clock.t = fixedTime.Add(2 * time.Hour)
	if r := h.do(http.MethodGet, p.URL, phone.Secret, nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("an expired token answered %d", r.StatusCode)
	}
}

// A node with no ffmpeg says so and hands over the bytes — never a 5xx and
// never a black player with no explanation.
func TestWithoutFFmpegThePlanIsDirectAndSaysWhy(t *testing.T) {
	h := newHarness(t).seed()
	h.seedProbe(blob1Hash, "matroska,webm", "h264", "ac3", 1080)

	resp, p := h.planForClient(t, "", asset1ID, phoneNoAC3)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	if p.Mode != "direct" || p.URL != "/api/v1/blobs/"+blob1Hash+"/content" {
		t.Errorf("mode = %q url = %q", p.Mode, p.URL)
	}
	if !strings.Contains(p.Reason, "audio ac3 not decodable by client") || !strings.Contains(p.Reason, "no ffmpeg") {
		t.Errorf("reason = %q", p.Reason)
	}
	// And the stream route refuses everything, opaquely.
	if r := h.get("/api/v1/playback/stream/anything"); r.StatusCode != http.StatusNotFound {
		t.Errorf("stream route without ffmpeg answered %d", r.StatusCode)
	}
}

// Nothing probed and ffprobe available: the plan probes now, answers from the
// finding, and caches it where the worker would have.
func TestAnUnprobedBlobIsProbedOnDemandAndCached(t *testing.T) {
	prober := &fakeProber{result: probe.Result{
		Container: "avi",
		Streams: []probe.Stream{
			{Index: 0, Type: "video", Codec: "h264", Width: 1280, Height: 720},
			{Index: 1, Type: "audio", Codec: "mp2", Channels: 2},
		},
	}}
	streamer := &fakeStreamer{}
	h := newHarness(t, withStreamLeg(streamer, newFakeBlobs(t), prober)).seed()

	resp, p := h.planForClient(t, "", asset1ID, phoneNoAC3)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	if p.Mode != "stream" || p.Reason != "container avi not playable by client; audio mp2 not decodable by client" {
		t.Errorf("mode = %q reason = %q", p.Mode, p.Reason)
	}
	if p.has("no_probe") {
		t.Error("the plan still says nothing probed the bytes")
	}
	if prober.calls != 1 {
		t.Errorf("ffprobe ran %d times, want once", prober.calls)
	}
	if n := h.countRows(t, `SELECT COUNT(*) FROM blob_probes WHERE blob_hash = ? AND container = 'avi'`, blob1Hash); n != 1 {
		t.Errorf("blob_probes rows for the blob = %d, want the cached probe", n)
	}
	// The second plan reads the cache, not ffprobe.
	h.planForClient(t, "", asset1ID, phoneNoAC3)
	if prober.calls != 1 {
		t.Errorf("ffprobe ran %d times over two plans; the second should have hit blob_probes", prober.calls)
	}
	if r := h.get("/api/v1/blobs/" + blob1Hash + "/probe"); r.StatusCode != http.StatusOK {
		t.Errorf("the cached probe is not readable: %d", r.StatusCode)
	}
}

// Past the cap the client is told to retry, with a status and not a stall.
func TestPastTheCapAStreamIsRefusedWith429(t *testing.T) {
	streamer := &fakeStreamer{busy: true}
	h := newHarness(t, withStreamLeg(streamer, newFakeBlobs(t), nil)).seed()
	h.seedProbe(blob1Hash, "matroska,webm", "h264", "ac3", 1080)
	_, p := h.planForClient(t, "", asset1ID, phoneNoAC3)

	r := h.get(p.URL)
	if r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d: %s", r.StatusCode, h.body(r))
	}
	if r.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After on the refusal")
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Errorf("Content-Type = %q, want a problem document", ct)
	}
}

// The client goes away; the streamer's context ends. This is the whole of
// what "ffmpeg is killed on disconnect" needs from the route — the kill
// itself is the streamer's, proven against a real ffmpeg in its own package.
func TestAClientDisconnectReachesTheStreamer(t *testing.T) {
	streamer := &fakeStreamer{block: true, cancelled: make(chan struct{})}
	h := newHarness(t, withStreamLeg(streamer, newFakeBlobs(t), nil)).seed()
	h.seedProbe(blob1Hash, "matroska,webm", "h264", "ac3", 1080)
	_, p := h.planForClient(t, "", asset1ID, phoneNoAC3)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, h.http.URL+p.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// Read the first bytes — the stream is live — then hang up.
	buf := make([]byte, len(fakeBody))
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatal(err)
	}
	if streamer.Active() != 1 {
		t.Errorf("active = %d mid-stream", streamer.Active())
	}
	_ = resp.Body.Close()

	select {
	case <-streamer.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the streamer never saw the disconnect")
	}
	deadline := time.Now().Add(5 * time.Second)
	for streamer.Active() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if streamer.Active() != 0 {
		t.Errorf("active = %d after the disconnect", streamer.Active())
	}
}

// Backward compatibility: a plan without `client` still needs a device, and
// still answers exactly as before, with none of the leg fields.
func TestAPlanWithoutClientIsUnchanged(t *testing.T) {
	h := newHarness(t, withStreamLeg(&fakeStreamer{}, newFakeBlobs(t), nil)).seed()
	h.seedProbe(blob1Hash, "matroska,webm", "h264", "ac3", 1080)

	r := h.do(http.MethodPost, "/api/v1/playback/plan", "", strings.NewReader(`{"asset_id":"`+asset1ID+`"}`))
	if r.StatusCode != http.StatusBadRequest {
		t.Errorf("no device and no client: status %d, want 400", r.StatusCode)
	}
	resp, p := h.plan(t, asset1ID, device1ID)
	if resp.StatusCode != http.StatusOK || p.Decision != "transcode" {
		t.Fatalf("status = %d decision = %q", resp.StatusCode, p.Decision)
	}
	raw := string(h.body(h.do(http.MethodPost, "/api/v1/playback/plan", "",
		strings.NewReader(fmt.Sprintf(`{"asset_id":%q,"device_id":%q}`, asset1ID, device1ID)))))
	for _, leg := range []string{`"mode"`, `"url"`, `"mime"`, `"source"`} {
		if strings.Contains(raw, leg) {
			t.Errorf("a device plan grew the leg field %s: %s", leg, raw)
		}
	}
	// Both together: the device decides, the client gets the leg.
	r2 := h.do(http.MethodPost, "/api/v1/playback/plan", "",
		strings.NewReader(fmt.Sprintf(`{"asset_id":%q,"device_id":%q,"client":%s}`, asset1ID, device1ID, phoneNoAC3)))
	var both clientPlan
	if err := json.Unmarshal(h.body(r2), &both); err != nil {
		t.Fatal(err)
	}
	if both.DeviceID != device1ID || both.Mode != "stream" {
		t.Errorf("device+client plan = device %q mode %q", both.DeviceID, both.Mode)
	}
}
