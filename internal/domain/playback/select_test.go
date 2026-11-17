package playback_test

import (
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/playback"
)

// The ranking from #885, one row per rule. The first two rows are the issue's
// acceptance cases: a work holding a 2160p HDR copy and a 1080p SDR copy.
func TestSelectChoosesTheCopyTheClientCanShow(t *testing.T) {
	uhdHDR := playback.Copy{AssetID: "a-uhd", Known: true, Height: 2160, HDR: true, VideoCodec: "hevc", BitrateBPS: 60_000_000}
	fhdSDR := playback.Copy{AssetID: "b-fhd", Known: true, Height: 1080, VideoCodec: "h264", BitrateBPS: 8_000_000}
	uhdSDR := playback.Copy{AssetID: "c-uhd-sdr", Known: true, Height: 2160, VideoCodec: "hevc", BitrateBPS: 40_000_000}
	hdSDR := playback.Copy{AssetID: "d-hd", Known: true, Height: 720, VideoCodec: "h264", BitrateBPS: 3_000_000}
	fhdAV1 := playback.Copy{AssetID: "e-fhd-av1", Known: true, Height: 1080, VideoCodec: "av1", BitrateBPS: 4_000_000}
	unprobed := playback.Copy{AssetID: "0-unprobed"}
	phoneCodecs := []string{"h264", "hevc"}

	cases := []struct {
		name   string
		copies []playback.Copy
		fit    playback.Fit
		want   string
		code   string
		passed map[string]string // asset id -> the code it lost with
	}{
		{
			name:   "1080 cap, no HDR: the 1080p SDR copy",
			copies: []playback.Copy{uhdHDR, fhdSDR},
			fit:    playback.Fit{MaxHeight: 1080, VideoCodecs: phoneCodecs},
			want:   "b-fhd", code: playback.SelectedBestFit,
			passed: map[string]string{"a-uhd": playback.ReasonResolutionTooHigh},
		},
		{
			name:   "2160 cap, HDR declared: the 2160p HDR copy",
			copies: []playback.Copy{fhdSDR, uhdHDR},
			fit:    playback.Fit{MaxHeight: 2160, SupportsHDR: true, VideoCodecs: phoneCodecs},
			want:   "a-uhd", code: playback.SelectedBestFit,
			passed: map[string]string{"b-fhd": playback.LostSmaller},
		},
		{
			name:   "2160 cap, HDR not declared: SDR beats the taller HDR copy",
			copies: []playback.Copy{uhdHDR, fhdSDR},
			fit:    playback.Fit{MaxHeight: 2160, VideoCodecs: phoneCodecs},
			want:   "b-fhd", code: playback.SelectedBestFit,
			passed: map[string]string{"a-uhd": playback.LostHDRNotDeclared},
		},
		{
			name:   "HDR not declared, a 2160p SDR copy exists: the tallest SDR wins",
			copies: []playback.Copy{uhdHDR, fhdSDR, uhdSDR},
			fit:    playback.Fit{VideoCodecs: phoneCodecs},
			want:   "c-uhd-sdr", code: playback.SelectedBestFit,
			passed: map[string]string{"a-uhd": playback.LostHDRNotDeclared, "b-fhd": playback.LostSmaller},
		},
		{
			name:   "HDR declared, equal heights: HDR wins the tie over a higher bitrate",
			copies: []playback.Copy{uhdSDR, {AssetID: "f-uhd-hdr", Known: true, Height: 2160, HDR: true, VideoCodec: "hevc", BitrateBPS: 30_000_000}},
			fit:    playback.Fit{SupportsHDR: true, VideoCodecs: phoneCodecs},
			want:   "f-uhd-hdr", code: playback.SelectedBestFit,
			passed: map[string]string{"c-uhd-sdr": playback.LostEqual},
		},
		{
			name:   "a copy needing a video re-encode loses to a smaller one that does not",
			copies: []playback.Copy{fhdAV1, hdSDR},
			fit:    playback.Fit{MaxHeight: 1080, VideoCodecs: phoneCodecs},
			want:   "d-hd", code: playback.SelectedBestFit,
			passed: map[string]string{"e-fhd-av1": playback.LostNeedsVideoReencode},
		},
		{
			name:   "no codecs declared: nothing is held to need a re-encode",
			copies: []playback.Copy{fhdAV1, hdSDR},
			fit:    playback.Fit{MaxHeight: 1080},
			want:   "e-fhd-av1", code: playback.SelectedBestFit,
		},
		{
			name:   "nothing fits: the smallest copy over the cap",
			copies: []playback.Copy{uhdHDR, fhdSDR},
			fit:    playback.Fit{MaxHeight: 720, VideoCodecs: phoneCodecs},
			want:   "b-fhd", code: playback.SelectedSmallestOverLimit,
			passed: map[string]string{"a-uhd": playback.LostLarger},
		},
		{
			name:   "a bitrate ceiling excludes a copy that is short enough",
			copies: []playback.Copy{uhdSDR, fhdSDR},
			fit:    playback.Fit{MaxBitrateBPS: 20_000_000, VideoCodecs: phoneCodecs},
			want:   "b-fhd", code: playback.SelectedBestFit,
			passed: map[string]string{"c-uhd-sdr": playback.ReasonBitrateTooHigh},
		},
		{
			name:   "an unknown bitrate is not held against a copy",
			copies: []playback.Copy{{AssetID: "g", Known: true, Height: 1080, VideoCodec: "h264"}},
			fit:    playback.Fit{MaxBitrateBPS: 1_000_000},
			want:   "g", code: playback.SelectedBestFit,
		},
		{
			name:   "an unprobed copy ranks after a known one, even one over the cap",
			copies: []playback.Copy{unprobed, uhdHDR},
			fit:    playback.Fit{MaxHeight: 1080},
			want:   "a-uhd", code: playback.SelectedSmallestOverLimit,
			passed: map[string]string{"0-unprobed": playback.LostNotProbed},
		},
		{
			name:   "nothing probed: the lowest id, and the reason says it was a guess",
			copies: []playback.Copy{{AssetID: "z"}, {AssetID: "m"}},
			fit:    playback.Fit{MaxHeight: 1080},
			want:   "m", code: playback.SelectedNotProbed,
			passed: map[string]string{"z": playback.LostNotProbed},
		},
		{
			name:   "identical copies: the id decides, so the answer is stable",
			copies: []playback.Copy{{AssetID: "y", Known: true, Height: 1080}, {AssetID: "x", Known: true, Height: 1080}},
			want:   "x", code: playback.SelectedBestFit,
			passed: map[string]string{"y": playback.LostEqual},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := playback.Select(tc.copies, tc.fit)
			if !ok {
				t.Fatal("Select found nothing to choose")
			}
			if got.Chosen.AssetID != tc.want || got.Chosen.Reason.Code != tc.code {
				t.Fatalf("chose %s (%s: %s), want %s (%s)",
					got.Chosen.AssetID, got.Chosen.Reason.Code, got.Chosen.Reason.Detail, tc.want, tc.code)
			}
			if len(got.Passed) != len(tc.copies)-1 {
				t.Errorf("passed over %d copies, want every other one (%d)", len(got.Passed), len(tc.copies)-1)
			}
			for _, p := range got.Passed {
				if p.Reason.Detail == "" {
					t.Errorf("%s was passed over with no detail", p.AssetID)
				}
				if want, ok := tc.passed[p.AssetID]; ok && p.Reason.Code != want {
					t.Errorf("%s passed over as %s (%s), want %s", p.AssetID, p.Reason.Code, p.Reason.Detail, want)
				}
			}
		})
	}
}

// The order the copies arrive in never changes the answer: a database that
// returns rows in a different order on the next request must not flip a phone
// between two files.
func TestSelectIgnoresInputOrder(t *testing.T) {
	copies := []playback.Copy{
		{AssetID: "a", Known: true, Height: 2160, HDR: true},
		{AssetID: "b", Known: true, Height: 1080},
		{AssetID: "c", Known: true, Height: 1080, BitrateBPS: 9},
		{AssetID: "d"},
	}
	fit := playback.Fit{MaxHeight: 1080}
	first, _ := playback.Select(copies, fit)
	for i := range copies {
		rotated := append(append([]playback.Copy{}, copies[i:]...), copies[:i]...)
		got, _ := playback.Select(rotated, fit)
		if got.Chosen.AssetID != first.Chosen.AssetID {
			t.Errorf("rotation %d chose %s, want %s", i, got.Chosen.AssetID, first.Chosen.AssetID)
		}
	}
	if first.Chosen.AssetID != "c" {
		t.Errorf("chose %s, want c (the higher bitrate at equal height)", first.Chosen.AssetID)
	}
}

func TestSelectWithNoCopiesFindsNothing(t *testing.T) {
	if _, ok := playback.Select(nil, playback.Fit{}); ok {
		t.Error("Select chose from nothing")
	}
}
