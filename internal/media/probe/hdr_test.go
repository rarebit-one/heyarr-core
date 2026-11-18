package probe

import (
	"encoding/json"
	"testing"
)

// The fixtures below are ffprobe -show_streams output trimmed to the fields
// the parser reads, in ffprobe's own shapes: colour names as strings, Dolby
// Vision side data as integers. A side_data_list entry other than the DOVI
// record sits beside it in the HDR10 cases because a real HDR10 stream carries
// mastering-display side data, and the parser has to skip it.

const (
	masteringDisplay = `{"side_data_type":"Mastering display metadata","red_x":"34000/50000","max_luminance":"10000000/10000"}`
	fixtureHDR10     = `{"format":{"format_name":"matroska,webm"},"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","profile":"Main 10","height":2160,` +
		`"color_space":"bt2020nc","color_transfer":"smpte2084","color_primaries":"bt2020","side_data_list":[` + masteringDisplay + `]}]}`
	fixtureHLG = `{"format":{"format_name":"matroska,webm"},"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","profile":"Main 10","height":2160,` +
		`"color_space":"bt2020nc","color_transfer":"arib-std-b67","color_primaries":"bt2020"}]}`
	// Profile 5 declares no colour at all: its samples are IPTPQc2, which no
	// VUI value describes.
	fixtureDV5 = `{"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2"},"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","profile":"Main 10","height":2160,"codec_tag_string":"dvh1",` +
		`"side_data_list":[{"side_data_type":"DOVI configuration record","dv_version_major":1,"dv_version_minor":0,"dv_profile":5,"dv_level":6,` +
		`"rpu_present_flag":1,"el_present_flag":0,"bl_present_flag":1,"dv_bl_signal_compatibility_id":0}]}]}`
	fixtureDV81 = `{"format":{"format_name":"matroska,webm"},"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","profile":"Main 10","height":2160,` +
		`"color_space":"bt2020nc","color_transfer":"smpte2084","color_primaries":"bt2020","side_data_list":[` +
		`{"side_data_type":"DOVI configuration record","dv_version_major":1,"dv_version_minor":0,"dv_profile":8,"dv_level":6,` +
		`"rpu_present_flag":1,"el_present_flag":0,"bl_present_flag":1,"dv_bl_signal_compatibility_id":1},` + masteringDisplay + `]}]}`
	// Profile 8.4 rides an HLG base.
	fixtureDV84 = `{"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2"},"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","profile":"Main 10","height":2160,` +
		`"color_space":"bt2020nc","color_transfer":"arib-std-b67","color_primaries":"bt2020","side_data_list":[` +
		`{"side_data_type":"DOVI configuration record","dv_profile":8,"dv_level":5,"rpu_present_flag":1,"el_present_flag":0,"bl_present_flag":1,"dv_bl_signal_compatibility_id":4}]}]}`
	// The Blu-ray dual layer: an HDR10 base with an enhancement layer.
	fixtureDV7 = `{"format":{"format_name":"matroska,webm"},"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","profile":"Main 10","height":2160,` +
		`"color_space":"bt2020nc","color_transfer":"smpte2084","color_primaries":"bt2020","side_data_list":[` +
		`{"side_data_type":"DOVI configuration record","dv_profile":7,"dv_level":6,"rpu_present_flag":1,"el_present_flag":1,"bl_present_flag":1,"dv_bl_signal_compatibility_id":6}]}]}`
	fixtureSDR = `{"format":{"format_name":"matroska,webm"},"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","profile":"Main 10","height":1080,` +
		`"color_space":"bt709","color_transfer":"bt709","color_primaries":"bt709"}]}`
	fixtureUntagged = `{"format":{"format_name":"mov,mp4"},"streams":[{"index":0,"codec_type":"video","codec_name":"h264","profile":"High","height":1080}]}`
)

// HDR is read from the transfer characteristic and the Dolby Vision record,
// not guessed from bit depth: a ten-bit HEVC encode is as often SDR as not,
// and choosing an SDR copy for a client that cannot show HDR turns on this
// flag being right (#885, #886). The dynamic range is what a re-encode would
// have to tone-map from.
func TestHDRIsTheTransferOrDolbyVision(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		hdr       bool
		rng       DynamicRange
		primaries string
		matrix    string
		dv        *DolbyVision
	}{
		{name: "HDR10", raw: fixtureHDR10, hdr: true, rng: RangePQ, primaries: "bt2020", matrix: "bt2020nc"},
		{name: "HLG", raw: fixtureHLG, hdr: true, rng: RangeHLG, primaries: "bt2020", matrix: "bt2020nc"},
		{
			name: "Dolby Vision profile 5, no compatible base", raw: fixtureDV5, hdr: true, rng: RangeDolbyVisionOnly,
			dv: &DolbyVision{Profile: 5, Level: 6, BaseCompatibility: 0, RPU: true, BaseLayer: true},
		},
		{
			name: "Dolby Vision profile 8.1, an HDR10 base", raw: fixtureDV81, hdr: true, rng: RangePQ, primaries: "bt2020", matrix: "bt2020nc",
			dv: &DolbyVision{Profile: 8, Level: 6, BaseCompatibility: 1, RPU: true, BaseLayer: true},
		},
		{
			name: "Dolby Vision profile 8.4, an HLG base", raw: fixtureDV84, hdr: true, rng: RangeHLG, primaries: "bt2020", matrix: "bt2020nc",
			dv: &DolbyVision{Profile: 8, Level: 5, BaseCompatibility: 4, RPU: true, BaseLayer: true},
		},
		{
			name: "Dolby Vision profile 7, the Blu-ray dual layer", raw: fixtureDV7, hdr: true, rng: RangePQ, primaries: "bt2020", matrix: "bt2020nc",
			dv: &DolbyVision{Profile: 7, Level: 6, BaseCompatibility: 6, RPU: true, EnhancementLayer: true, BaseLayer: true},
		},
		{name: "ten-bit SDR", raw: fixtureSDR, rng: RangeSDR, primaries: "bt709", matrix: "bt709"},
		{name: "no colour reported", raw: fixtureUntagged, rng: RangeSDR},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := parseAndCheck([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			video, ok := result.VideoStream()
			if !ok {
				t.Fatal("no video stream")
			}
			if got := video.HDR(); got != tc.hdr {
				t.Errorf("HDR() = %v, want %v (transfer %q)", got, tc.hdr, video.ColorTransfer)
			}
			if got := video.DynamicRange(); got != tc.rng {
				t.Errorf("DynamicRange() = %q, want %q", got, tc.rng)
			}
			if video.ColorPrimaries != tc.primaries || video.ColorSpace != tc.matrix {
				t.Errorf("primaries/matrix = %q/%q, want %q/%q", video.ColorPrimaries, video.ColorSpace, tc.primaries, tc.matrix)
			}
			switch {
			case tc.dv == nil && video.DolbyVision != nil:
				t.Errorf("DolbyVision = %+v, want none", *video.DolbyVision)
			case tc.dv != nil && (video.DolbyVision == nil || *video.DolbyVision != *tc.dv):
				t.Errorf("DolbyVision = %+v, want %+v", video.DolbyVision, *tc.dv)
			}
		})
	}
}

// A profile 5 record wins over a PQ tag on the same stream. Some muxers write
// a VUI onto profile 5; trusting it would tone-map IPTPQc2 as if it were
// YCbCr, which is the green-and-purple picture refusing exists to prevent.
func TestProfileFiveIsDolbyVisionOnlyWhateverTheTransferSays(t *testing.T) {
	s := Stream{Type: "video", ColorTransfer: "smpte2084", DolbyVision: &DolbyVision{Profile: 5, BaseCompatibility: 1}}
	if got := s.DynamicRange(); got != RangeDolbyVisionOnly {
		t.Errorf("DynamicRange() = %q, want %q", got, RangeDolbyVisionOnly)
	}
}

// The facts survive the round trip through blob_probes.streams, which is a
// JSON column: a probe recorded with them must read back with them, and one
// recorded before them must read back without inventing any.
func TestColourFactsRoundTripThroughTheStoredJSON(t *testing.T) {
	result, err := parseAndCheck([]byte(fixtureDV81))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(result.Streams)
	if err != nil {
		t.Fatal(err)
	}
	var back []Stream
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if got := back[0]; got.DynamicRange() != RangePQ || got.DolbyVision == nil || got.DolbyVision.Profile != 8 || got.ColorPrimaries != "bt2020" {
		t.Errorf("round trip lost the colour facts: %+v", got)
	}

	var old []Stream
	if err := json.Unmarshal([]byte(`[{"index":0,"type":"video","codec":"hevc","height":2160}]`), &old); err != nil {
		t.Fatal(err)
	}
	if old[0].HDR() || old[0].DynamicRange() != RangeSDR || old[0].DolbyVision != nil {
		t.Errorf("a probe stored before colour was recorded reads as %+v", old[0])
	}
}
