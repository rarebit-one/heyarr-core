package catalog

import (
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
)

// A real 1080p broadcast master is not 1920x1080. It is cropped vertically,
// and before #231 the resolution attribute was the frame's pixel height — so
// a `resolution >= 1080` gate rejected it, with a confident `fail` rather than
// an honest `undetermined`.
//
// The dimensions here are from a real probe taken on a deployed host: a
// 2.00:1 HDTV master, h264 High@4.1, that reported "resolution 960, which is
// not at least 1080".
func TestProbeAttributesClassifyWidescreenHDAsItsClass(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		streams string
		want    int64
		absent  bool
	}{
		{
			name:    "2.00:1 1080p master, the case from the field",
			streams: `[{"type":"video","codec":"h264","profile":"High","width":1920,"height":960}]`,
			want:    1080,
		},
		{
			name:    "2.39:1 1080p",
			streams: `[{"type":"video","codec":"h264","width":1920,"height":800}]`,
			want:    1080,
		},
		{
			name:    "2.39:1 2160p",
			streams: `[{"type":"video","codec":"hevc","width":3840,"height":1600}]`,
			want:    2160,
		},
		{
			name:    "16:9 1080p still classifies as 1080",
			streams: `[{"type":"video","codec":"h264","width":1920,"height":1080}]`,
			want:    1080,
		},
		{
			name:    "PAL SD keeps its height, where width cannot discriminate",
			streams: `[{"type":"video","codec":"mpeg2video","width":720,"height":576}]`,
			want:    576,
		},
		{
			name:    "a video stream with no dimensions leaves the attribute absent",
			streams: `[{"type":"video","codec":"h264"}]`,
			absent:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			attrs := acquisition.Attributes{}
			applyProbeAttributes(attrs, tc.streams)

			got, ok := attrs[policy.AttrResolution]
			if tc.absent {
				if ok {
					t.Fatalf("resolution = %v, want absent so it reports undetermined", got)
				}
				return
			}
			if !ok {
				t.Fatal("resolution attribute is absent, want it determined")
			}
			if got.Num != tc.want {
				t.Errorf("resolution = %d, want %d", got.Num, tc.want)
			}
		})
	}
}
