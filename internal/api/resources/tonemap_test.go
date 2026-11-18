//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/media/ffmpeg"
)

// A re-encode of HDR for a client that does not declare it is tone-mapped to
// SDR, end to end: the plan says so, the signed token carries it, and the
// stream route hands it to the repackager (#886).

// seedPicture records a probe of blob1 as a 2160p HEVC picture with the given
// colour fields, and E-AC-3 audio the phones below decode.
func (h *harness) seedPicture(colour string) {
	h.exec(`INSERT INTO blob_probes (blob_hash, container, format_long, duration_seconds, bitrate_bps, streams, bytes_read, materialised, probed_at)
		VALUES (?, 'matroska,webm', '', 6960, 60000000, ?, 0, 0, ?)`, blob1Hash,
		`[{"index":0,"type":"video","codec":"hevc","profile":"Main 10","width":3840,"height":2160`+colour+`},`+
			`{"index":1,"type":"audio","codec":"eac3","channels":6}]`, seedTime)
}

const (
	pqColour  = `,"color_transfer":"smpte2084","color_primaries":"bt2020","color_space":"bt2020nc"`
	dv5Colour = `,"dolby_vision":{"profile":5,"level":6,"bl_compatibility_id":0,"rpu":true,"bl":true}`
	// A 1080p phone: the 2160p source is scaled, so its video is re-encoded.
	sdrPhone = `{"containers":["mp4","mkv"],"video":["h264","hevc"],"audio":["aac","eac3"],"max_height":1080}`
	hdrPhone = `{"containers":["mp4","mkv"],"video":["h264","hevc"],"audio":["aac","eac3"],"max_height":1080,"supports_hdr":true}`
)

type toneMapPlan struct {
	clientPlan
	Source *struct {
		DynamicRange string `json:"dynamic_range"`
	} `json:"source"`
}

func (h *harness) planRaw(t *testing.T, body string) toneMapPlan {
	t.Helper()
	resp := h.do(http.MethodPost, "/api/v1/playback/plan", "", strings.NewReader(body))
	raw := h.body(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", resp.StatusCode, raw)
	}
	var p toneMapPlan
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &p.clientPlan); err != nil {
		t.Fatal(err)
	}
	return p
}

// streamSpec fetches a planned stream and returns what the repackager was asked.
func (h *harness) streamSpec(t *testing.T, streamer *fakeStreamer, p toneMapPlan) ffmpeg.StreamSpec {
	t.Helper()
	if p.Mode != "stream" {
		t.Fatalf("mode = %q, want stream (reason %q)", p.Mode, p.Reason)
	}
	if r := h.get(p.URL); r.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d: %s", r.StatusCode, h.body(r))
	}
	return streamer.last(t)
}

func TestAnHDRReencodeForAnSDRClientIsToneMapped(t *testing.T) {
	t.Parallel()
	streamer := &fakeStreamer{}
	h := newHarness(t, withStreamLeg(streamer, newFakeBlobs(t), nil)).seed()
	h.seedPicture(pqColour)

	p := h.planRaw(t, `{"asset_id":"`+asset1ID+`","client":`+sdrPhone+`}`)
	if !strings.Contains(p.Reason, "HDR (pq) tone-mapped to SDR because the client does not declare HDR") {
		t.Errorf("reason = %q, want it to say the stream is tone-mapped", p.Reason)
	}
	if p.Source == nil || p.Source.DynamicRange != "pq" {
		t.Errorf("source = %+v, want dynamic_range pq", p.Source)
	}
	spec := h.streamSpec(t, streamer, p)
	if spec.CopyVideo || spec.MaxHeight != 1080 || spec.ToneMap != ffmpeg.ToneMapPQ {
		t.Errorf("the streamer was asked for %+v; want a 1080p re-encode tone-mapped from PQ", spec)
	}
}

func TestAnHDRReencodeForAnHDRClientIsNotToneMapped(t *testing.T) {
	t.Parallel()
	streamer := &fakeStreamer{}
	h := newHarness(t, withStreamLeg(streamer, newFakeBlobs(t), nil)).seed()
	h.seedPicture(pqColour)

	// Declared in the request.
	p := h.planRaw(t, `{"asset_id":"`+asset1ID+`","client":`+hdrPhone+`}`)
	if spec := h.streamSpec(t, streamer, p); spec.ToneMap != ffmpeg.ToneMapNone {
		t.Errorf("declared HDR: tone-map = %q, want none", spec.ToneMap)
	}
	if strings.Contains(p.Reason, "tone-mapped") {
		t.Errorf("reason = %q mentions a tone-map that is not happening", p.Reason)
	}

	// Unstated in the request, filled from the registered device, which is
	// an HDR television.
	h.exec(`UPDATE devices SET supports_hdr = 1 WHERE id = ?`, device1ID)
	p = h.planRaw(t, `{"asset_id":"`+asset1ID+`","device_id":"`+device1ID+`","client":`+sdrPhone+`}`)
	if spec := h.streamSpec(t, streamer, p); spec.ToneMap != ffmpeg.ToneMapNone {
		t.Errorf("HDR device: tone-map = %q, want none", spec.ToneMap)
	}

	// The request's own false wins over the device's true.
	p = h.planRaw(t, `{"asset_id":"`+asset1ID+`","device_id":"`+device1ID+`","client":`+
		strings.Replace(hdrPhone, `"supports_hdr":true`, `"supports_hdr":false`, 1)+`}`)
	if spec := h.streamSpec(t, streamer, p); spec.ToneMap != ffmpeg.ToneMapPQ {
		t.Errorf("declared false over an HDR device: tone-map = %q, want pq", spec.ToneMap)
	}
}

// An ffmpeg without zscale still streams, as before there was a tone-map, and
// says the picture will look washed out rather than failing to start.
func TestWithoutTheFiltersTheReencodeIsNotToneMappedAndSaysSo(t *testing.T) {
	t.Parallel()
	streamer := &fakeStreamer{noToneMap: true}
	h := newHarness(t, withStreamLeg(streamer, newFakeBlobs(t), nil)).seed()
	h.seedPicture(pqColour)

	p := h.planRaw(t, `{"asset_id":"`+asset1ID+`","client":`+sdrPhone+`}`)
	if !strings.Contains(p.Reason, "no zscale/tonemap filters") {
		t.Errorf("reason = %q, want it to say why there is no tone-map", p.Reason)
	}
	if spec := h.streamSpec(t, streamer, p); spec.ToneMap != ffmpeg.ToneMapNone {
		t.Errorf("tone-map = %q on an ffmpeg that cannot", spec.ToneMap)
	}
}

// Dolby Vision profile 5 has no base layer a re-encode renders correctly, so a
// client that needs one is refused with a code, not served green and purple.
// A client that decodes it as it is still gets the bytes.
func TestDolbyVisionWithNoBaseIsRefusedARencode(t *testing.T) {
	t.Parallel()
	streamer := &fakeStreamer{}
	h := newHarness(t, withStreamLeg(streamer, newFakeBlobs(t), nil)).seed()
	h.seedPicture(dv5Colour)

	for _, client := range []string{sdrPhone, hdrPhone} {
		p := h.planRaw(t, `{"asset_id":"`+asset1ID+`","client":`+client+`}`)
		if p.Mode != "unplayable" || p.URL != "" {
			t.Fatalf("mode = %q url = %q, want unplayable with nothing to fetch (reason %q)", p.Mode, p.URL, p.Reason)
		}
		if !p.has("dolby_vision_no_compatible_base") {
			t.Errorf("reasons = %+v, want dolby_vision_no_compatible_base", p.Reasons)
		}
		if p.Source == nil || p.Source.DynamicRange != "dolby_vision_only" {
			t.Errorf("source = %+v", p.Source)
		}
	}
	streamer.mu.Lock()
	asked := len(streamer.specs)
	streamer.mu.Unlock()
	if asked != 0 {
		t.Errorf("the streamer was asked %d time(s) for a refused plan", asked)
	}

	tv := `{"containers":["mkv"],"video":["hevc"],"audio":["eac3"],"max_height":2160}`
	if p := h.planRaw(t, `{"asset_id":"`+asset1ID+`","client":`+tv+`}`); p.Mode != "direct" {
		t.Errorf("a client decoding the source as it is: mode = %q, want direct (reason %q)", p.Mode, p.Reason)
	}
}
