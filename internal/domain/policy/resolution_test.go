package policy

import "testing"

func TestResolutionClass(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		width, height int64
		want          int64
		wantOK        bool
	}{
		// The case this function exists for: 1080p is 1080p at every aspect
		// ratio, and every one of these was rejected as sub-1080 when the
		// attribute was the pixel height (#231).
		{"1080p 16:9", 1920, 1080, 1080, true},
		{"1080p 1.85:1", 1920, 1038, 1080, true},
		{"1080p 2.00:1", 1920, 960, 1080, true},
		{"1080p 2.35:1", 1920, 816, 1080, true},
		{"1080p 2.39:1", 1920, 800, 1080, true},

		// 2160p crops the same way.
		{"2160p 16:9", 3840, 2160, 2160, true},
		{"2160p 2.39:1", 3840, 1600, 2160, true},
		{"4K DCI", 4096, 1716, 2160, true},

		{"1440p", 2560, 1440, 1440, true},

		{"720p 16:9", 1280, 720, 720, true},
		{"720p scope", 1280, 536, 720, true},

		// Anamorphic storage: the width understates the format, so the height
		// is the second opinion rather than the first.
		{"HDV 1440x1080 is 1080p", 1440, 1080, 1080, true},
		{"anamorphic 1920x1080 stored 1440", 1440, 1080, 1080, true},

		// Standard definition keeps the height, because no width threshold
		// separates 480-line from 576-line: both are 720 wide.
		{"NTSC DVD", 720, 480, 480, true},
		{"PAL DVD", 720, 576, 576, true},
		{"VCD", 352, 240, 240, true},

		// Absence stays absence, so the attribute is reported undetermined
		// rather than as a confident zero.
		{"nothing known", 0, 0, 0, false},
		{"width only, sub-HD", 700, 0, 0, false},

		// Height alone still classifies, for a probe that reported no width.
		{"height only 1080", 0, 1080, 1080, true},
		{"height only 2160", 0, 2160, 2160, true},

		// Negative or nonsense dimensions must not classify as something.
		{"negative", -1, -1, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := ResolutionClass(tc.width, tc.height)
			if ok != tc.wantOK {
				t.Fatalf("ResolutionClass(%d, %d) ok = %v, want %v", tc.width, tc.height, ok, tc.wantOK)
			}
			if got != tc.want {
				t.Errorf("ResolutionClass(%d, %d) = %d, want %d", tc.width, tc.height, got, tc.want)
			}
		})
	}
}
