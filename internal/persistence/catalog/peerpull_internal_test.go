package catalog

import "testing"

// A hold names the path the origin's library root saw, never the host path,
// and falls back to the file name for an asset under no root (an acquisition's
// download path).
func TestRelativeTo(t *testing.T) {
	roots := []string{"/srv/media/roms/snes", "/srv/media/roms"}
	cases := []struct {
		name, source, file, want string
	}{
		{"under the root", "/srv/media/roms/gba/Metroid Fusion (Europe).gba", "", "gba/Metroid Fusion (Europe).gba"},
		{"the nested root wins", "/srv/media/roms/snes/Super Mario World (USA).sfc", "", "Super Mario World (USA).sfc"},
		{"under no root", "/downloads/complete/Chrono Trigger (USA).sfc", "Chrono Trigger (USA).sfc", "Chrono Trigger (USA).sfc"},
		{"under no root, no file name", "/downloads/complete/a.gba", "", "a.gba"},
		{"a sibling of the root is not under it", "/srv/media/roms-old/a.sfc", "", "a.sfc"},
		{"nothing to go on", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := relativeTo(roots, tc.source, tc.file); got != tc.want {
				t.Fatalf("relativeTo(%q, %q) = %q, want %q", tc.source, tc.file, got, tc.want)
			}
		})
	}
}
