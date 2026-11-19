package hls_test

import (
	"math"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/media/hls"
)

func starts(segs []hls.Segment) []float64 {
	out := make([]float64, len(segs))
	for i, s := range segs {
		out[i] = s.Start
	}
	return out
}

func sum(segs []hls.Segment) float64 {
	total := 0.0
	for _, s := range segs {
		total += s.Duration
	}
	return total
}

func sameFloats(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(a[i]-b[i]) > 1e-9 {
			return false
		}
	}
	return true
}

func TestPlanKeyframesCutsOnlyAtKeyframes(t *testing.T) {
	cases := []struct {
		name      string
		keyframes []float64
		end       float64
		want      []float64
	}{
		{
			name:      "regular two-second GOPs make six-second segments",
			keyframes: []float64{0, 2, 4, 6, 8, 10, 12, 14, 16, 18},
			end:       20,
			want:      []float64{0, 6, 12, 18},
		},
		{
			name:      "irregular keyframes: each boundary is the first keyframe a target after the last",
			keyframes: []float64{0, 2.5, 4, 9.5, 11, 13.2, 19, 20, 26, 33, 37},
			end:       40,
			want:      []float64{0, 9.5, 19, 26, 33},
		},
		{
			name:      "a GOP longer than the target is one segment",
			keyframes: []float64{0, 10, 20},
			end:       30,
			want:      []float64{0, 10, 20},
		},
		{
			name:      "a sliver of a last segment folds into the one before",
			keyframes: []float64{0, 6, 12, 18},
			end:       18.5,
			want:      []float64{0, 6, 12},
		},
		{
			name:      "a source that starts late keeps its own clock",
			keyframes: []float64{1.4, 3.4, 7.4, 9.4},
			end:       11.4,
			want:      []float64{1.4, 7.4},
		},
		{
			name:      "keyframes at or past the end are not boundaries",
			keyframes: []float64{0, 6, 12, 18},
			end:       12,
			want:      []float64{0, 6},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			segs := hls.PlanKeyframes(tc.keyframes, tc.end, hls.SegmentSeconds)
			if got := starts(segs); !sameFloats(got, tc.want) {
				t.Fatalf("boundaries = %v, want %v", got, tc.want)
			}
			if got, want := sum(segs), tc.end-tc.keyframes[0]; math.Abs(got-want) > 1e-9 {
				t.Errorf("durations sum to %v, want %v", got, want)
			}
		})
	}
	if segs := hls.PlanKeyframes(nil, 10, hls.SegmentSeconds); segs != nil {
		t.Errorf("no keyframes planned %v, want nothing", segs)
	}
}

func TestPlanUniformCoversTheFilm(t *testing.T) {
	cases := []struct {
		duration float64
		want     []float64
		last     float64
	}{
		{duration: 18, want: []float64{0, 6, 12}, last: 6},
		{duration: 20, want: []float64{0, 6, 12, 18}, last: 2},
		{duration: 18.4, want: []float64{0, 6, 12}, last: 6.4},
		{duration: 4, want: []float64{0}, last: 4},
		{duration: 7262.5, last: 2.5},
	}
	for _, tc := range cases {
		segs := hls.PlanUniform(tc.duration, hls.SegmentSeconds)
		if tc.want != nil && !sameFloats(starts(segs), tc.want) {
			t.Errorf("PlanUniform(%v) boundaries = %v, want %v", tc.duration, starts(segs), tc.want)
		}
		if math.Abs(sum(segs)-tc.duration) > 1e-6 {
			t.Errorf("PlanUniform(%v) sums to %v", tc.duration, sum(segs))
		}
		if got := segs[len(segs)-1].Duration; math.Abs(got-tc.last) > 1e-6 {
			t.Errorf("PlanUniform(%v) last segment = %v, want %v", tc.duration, got, tc.last)
		}
	}
}

func TestLocate(t *testing.T) {
	segs := hls.PlanKeyframes([]float64{0, 9.5, 19, 26}, 30, hls.SegmentSeconds)
	cases := []struct {
		t    float64
		want int
	}{
		{-0.5, -1}, {0, 0}, {9.4, 0}, {9.5, 1}, {9.4999, 1}, {18.9, 1}, {19, 2}, {29.9, 3},
	}
	for _, tc := range cases {
		if got := hls.Locate(segs, tc.t); got != tc.want {
			t.Errorf("Locate(%v) = %d, want %d", tc.t, got, tc.want)
		}
	}
}

func TestPlaylistIsAVODPresentationOfTheWholeFilm(t *testing.T) {
	got := hls.Playlist(hls.PlanKeyframes([]float64{0, 9.5, 19, 26}, 30.25, hls.SegmentSeconds))
	want := `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:10
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-MAP:URI="init.mp4"
#EXTINF:9.500000,
0.m4s
#EXTINF:9.500000,
1.m4s
#EXTINF:7.000000,
2.m4s
#EXTINF:4.250000,
3.m4s
#EXT-X-ENDLIST
`
	if got != want {
		t.Errorf("playlist:\n%s\nwant:\n%s", got, want)
	}
}

func TestParseIndex(t *testing.T) {
	out := strings.Join([]string{
		"packet|pts_time=0.000000|flags=K__",
		"packet|pts_time=0.083000|flags=___",
		"packet|pts_time=N/A|flags=K__",
		"packet|pts_time=4.000000|flags=K__",
		"packet|pts_time=2.500000|flags=K_D",
		"packet|pts_time=2.500000|flags=K__",
		"format|start_time=-0.005000|duration=40.000000",
	}, "\n")
	got, err := hls.ParseIndex(strings.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if !sameFloats(got.Keyframes, []float64{0, 2.5, 4}) || got.Start != -0.005 || got.Duration != 40 {
		t.Errorf("ParseIndex = %+v", got)
	}
	if _, err := hls.ParseIndex(strings.NewReader("packet|pts_time=0|flags=K__\n")); err == nil {
		t.Error("an index with no duration parsed")
	}
}
