package playback_test

import (
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/playback"
)

// The preference's limits (#887), one row per quality and per way a cap can
// meet a screen. Zero is "no limit" on both sides, as everywhere here.
func TestPreferenceLimits(t *testing.T) {
	cases := []struct {
		name        string
		pref        playback.Preference
		screen      int64
		bitrate     int64
		wantHeight  int64
		wantBitrate int64
	}{
		{name: "absent is fit: the screen", pref: playback.Preference{}, screen: 1080, wantHeight: 1080},
		{name: "fit: the screen", pref: playback.Preference{Quality: playback.QualityFit}, screen: 2160, wantHeight: 2160},
		{
			name: "fit under a cap: the tighter of the two", pref: playback.Preference{Quality: playback.QualityFit, MaxHeight: 1080},
			screen: 2160, wantHeight: 1080,
		},
		{
			name: "fit, a cap over the screen: the screen", pref: playback.Preference{MaxHeight: 2160},
			screen: 1080, wantHeight: 1080,
		},
		{name: "fit, no screen stated: the cap", pref: playback.Preference{MaxHeight: 720}, wantHeight: 720},
		{name: "best: the screen is no limit", pref: playback.Preference{Quality: playback.QualityBest}, screen: 1080, wantHeight: 0},
		{
			name: "best under a cap: the cap, past the screen", pref: playback.Preference{Quality: playback.QualityBest, MaxHeight: 1440},
			screen: 1080, wantHeight: 1440,
		},
		{name: "data saver: 720 under a taller screen", pref: playback.Preference{Quality: playback.QualityDataSaver}, screen: 2160, wantHeight: 720},
		{name: "data saver: a shorter screen holds", pref: playback.Preference{Quality: playback.QualityDataSaver}, screen: 480, wantHeight: 480},
		{name: "data saver, no screen stated: 720", pref: playback.Preference{Quality: playback.QualityDataSaver}, wantHeight: 720},
		{
			name: "data saver under a lower cap: the cap", pref: playback.Preference{Quality: playback.QualityDataSaver, MaxHeight: 576},
			screen: 1080, wantHeight: 576,
		},
		{
			name: "a bitrate cap under the client's: the cap", pref: playback.Preference{MaxBitrateBPS: 4_000_000},
			bitrate: 20_000_000, wantBitrate: 4_000_000,
		},
		{
			name: "a bitrate cap, the client states none: the cap", pref: playback.Preference{Quality: playback.QualityBest, MaxBitrateBPS: 8_000_000},
			wantBitrate: 8_000_000,
		},
		{
			name: "the client's bitrate under the cap: the client's", pref: playback.Preference{MaxBitrateBPS: 20_000_000},
			bitrate: 4_000_000, wantBitrate: 4_000_000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fit := tc.pref.Apply(playback.Fit{MaxHeight: tc.screen, MaxBitrateBPS: tc.bitrate, SupportsHDR: true, VideoCodecs: []string{"h264"}})
			if fit.MaxHeight != tc.wantHeight || fit.MaxBitrateBPS != tc.wantBitrate {
				t.Errorf("limits = %d lines, %d bit/s; want %d, %d", fit.MaxHeight, fit.MaxBitrateBPS, tc.wantHeight, tc.wantBitrate)
			}
			if got := tc.pref.Height(tc.screen); got != tc.wantHeight {
				t.Errorf("Height(%d) = %d, want %d: selection and the planner must read one limit", tc.screen, got, tc.wantHeight)
			}
			// The device's facts are never the preference's to change.
			if !fit.SupportsHDR || len(fit.VideoCodecs) != 1 {
				t.Errorf("Apply touched the device's facts: %+v", fit)
			}
		})
	}
}

func TestQualityValid(t *testing.T) {
	for _, q := range []playback.Quality{"", "fit", "best", "data_saver"} {
		if !q.Valid() {
			t.Errorf("%q is a quality", q)
		}
	}
	for _, q := range []playback.Quality{"Best", "fit_screen", "high", " "} {
		if q.Valid() {
			t.Errorf("%q is not a quality", q)
		}
	}
}

// What a preference does to the choice (#887), over the copies #885's tests
// use: a 2160p HDR remux, a 1080p SDR encode and two 720p encodes.
func TestSelectUnderAPreference(t *testing.T) {
	uhdHDR := playback.Copy{AssetID: "a-uhd", Known: true, Height: 2160, HDR: true, VideoCodec: "hevc", BitrateBPS: 60_000_000}
	fhdSDR := playback.Copy{AssetID: "b-fhd", Known: true, Height: 1080, VideoCodec: "h264", BitrateBPS: 8_000_000}
	hdRich := playback.Copy{AssetID: "c-hd-rich", Known: true, Height: 720, VideoCodec: "h264", BitrateBPS: 5_000_000}
	hdLean := playback.Copy{AssetID: "d-hd-lean", Known: true, Height: 720, VideoCodec: "h264", BitrateBPS: 2_000_000}
	hdrPhone := playback.Fit{MaxHeight: 1080, SupportsHDR: true, VideoCodecs: []string{"h264", "hevc"}}
	sdrPhone := playback.Fit{MaxHeight: 1080, VideoCodecs: []string{"h264", "hevc"}}

	cases := []struct {
		name       string
		copies     []playback.Copy
		fit        playback.Fit
		pref       playback.Preference
		want       string
		code       string
		detailHas  string
		passedCode map[string]string
	}{
		{
			name:   "fit on an HDR phone: the copy at the screen",
			copies: []playback.Copy{uhdHDR, fhdSDR}, fit: hdrPhone,
			pref: playback.Preference{Quality: playback.QualityFit},
			want: "b-fhd", code: playback.SelectedBestFit,
			passedCode: map[string]string{"a-uhd": playback.ReasonResolutionTooHigh},
		},
		{
			name:   "best on an HDR phone: the tallest copy, past the screen",
			copies: []playback.Copy{uhdHDR, fhdSDR}, fit: hdrPhone,
			pref: playback.Preference{Quality: playback.QualityBest},
			want: "a-uhd", code: playback.SelectedBestFit, detailHas: "(best quality)",
			passedCode: map[string]string{"b-fhd": playback.LostSmaller},
		},
		{
			name:   "best on an SDR phone: still SDR first, because nothing tone-maps",
			copies: []playback.Copy{uhdHDR, fhdSDR}, fit: sdrPhone,
			pref: playback.Preference{Quality: playback.QualityBest},
			want: "b-fhd", code: playback.SelectedBestFit, detailHas: "(best quality)",
			passedCode: map[string]string{"a-uhd": playback.LostHDRNotDeclared},
		},
		{
			name:   "best under the person's 1080 cap: the cap holds",
			copies: []playback.Copy{uhdHDR, fhdSDR}, fit: playback.Fit{MaxHeight: 2160, SupportsHDR: true},
			pref: playback.Preference{Quality: playback.QualityBest, MaxHeight: 1080},
			want: "b-fhd", code: playback.SelectedBestFit, detailHas: "max height 1080 (best quality)",
			passedCode: map[string]string{"a-uhd": playback.ReasonResolutionTooHigh},
		},
		{
			name:   "data saver: the 720p copy, and the leaner of two",
			copies: []playback.Copy{uhdHDR, fhdSDR, hdRich, hdLean}, fit: sdrPhone,
			pref: playback.Preference{Quality: playback.QualityDataSaver},
			want: "d-hd-lean", code: playback.SelectedBestFit, detailHas: "(data saver)",
			passedCode: map[string]string{
				"c-hd-rich": playback.LostEqual, "b-fhd": playback.ReasonResolutionTooHigh, "a-uhd": playback.ReasonResolutionTooHigh,
			},
		},
		{
			name:   "fit at equal height: the richer of two, as before",
			copies: []playback.Copy{hdRich, hdLean}, fit: sdrPhone,
			pref: playback.Preference{},
			want: "c-hd-rich", code: playback.SelectedBestFit,
			passedCode: map[string]string{"d-hd-lean": playback.LostEqual},
		},
		{
			name:   "data saver, nothing at 720: the smallest copy over it, scaled down",
			copies: []playback.Copy{uhdHDR, fhdSDR}, fit: sdrPhone,
			pref: playback.Preference{Quality: playback.QualityDataSaver},
			want: "b-fhd", code: playback.SelectedSmallestOverLimit, detailHas: "max height 720 (data saver)",
			passedCode: map[string]string{"a-uhd": playback.LostLarger},
		},
		{
			name:   "a bitrate cap passes over the richer 720p copy",
			copies: []playback.Copy{hdRich, hdLean}, fit: sdrPhone,
			pref: playback.Preference{MaxBitrateBPS: 3_000_000},
			want: "d-hd-lean", code: playback.SelectedBestFit,
			passedCode: map[string]string{"c-hd-rich": playback.ReasonBitrateTooHigh},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sel, ok := playback.Select(tc.copies, tc.pref.Apply(tc.fit))
			if !ok {
				t.Fatal("no selection")
			}
			if sel.Chosen.AssetID != tc.want || sel.Chosen.Reason.Code != tc.code {
				t.Fatalf("chose %s (%s: %s), want %s (%s)",
					sel.Chosen.AssetID, sel.Chosen.Reason.Code, sel.Chosen.Reason.Detail, tc.want, tc.code)
			}
			if tc.detailHas != "" && !strings.Contains(sel.Chosen.Reason.Detail, tc.detailHas) {
				t.Errorf("detail %q does not name the preference (%q)", sel.Chosen.Reason.Detail, tc.detailHas)
			}
			if q := tc.pref.Quality; (q == "" || q == playback.QualityFit) &&
				(strings.Contains(sel.Chosen.Reason.Detail, "(best quality)") || strings.Contains(sel.Chosen.Reason.Detail, "(data saver)")) {
				t.Errorf("detail %q names a preference the default should leave unsaid", sel.Chosen.Reason.Detail)
			}
			for _, p := range sel.Passed {
				if want, ok := tc.passedCode[p.AssetID]; ok && p.Reason.Code != want {
					t.Errorf("%s lost with %s (%s), want %s", p.AssetID, p.Reason.Code, p.Reason.Detail, want)
				}
			}
		})
	}
}
