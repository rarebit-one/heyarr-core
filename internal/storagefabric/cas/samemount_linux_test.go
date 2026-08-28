//go:build linux

package cas

import "testing"

// The #222 mountinfo: a store and a library on ONE device (8:17) but two
// mounts, which is what ProtectSystem=strict produces (a read-only library
// bind mount inside a read-write media mount). st_dev is identical across all
// three lines; the mount id is not.
const mountinfo222 = `23 30 8:17 / / rw,relatime shared:1 - ext4 /dev/sdb1 rw
531 23 8:17 /srv/media /srv/media ro,nosuid,relatime shared:9 - ext4 /dev/sdb1 rw
254 531 8:17 /srv/media/heyarr /srv/media/heyarr rw,nosuid,relatime shared:12 - ext4 /dev/sdb1 rw
`

func TestMountIDForPath222(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		wantID string
	}{
		// A blob in the store resolves to the read-write heyarr mount.
		{"store blob", "/srv/media/heyarr/cas/ab/cd", "254"},
		// A file in the library resolves to the read-only library mount —
		// a DIFFERENT mount from the store despite the same device. This is
		// the pair the old st_dev check called "same filesystem" and passed
		// in silence.
		{"library file", "/srv/media/Movies/x.mkv", "531"},
		// Longest-prefix wins: a path under heyarr is the heyarr mount, not
		// the library mount it also sits beneath.
		{"nested store dir", "/srv/media/heyarr/tmp/staging/x", "254"},
		// Anything else falls back to the root mount.
		{"elsewhere", "/var/lib/heyarr/cas", "23"},
		{"exact mount point", "/srv/media", "531"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := mountIDForPath([]byte(mountinfo222), tt.path)
			if !ok {
				t.Fatalf("mountIDForPath(%q) reported unknown, want id %q", tt.path, tt.wantID)
			}
			if id != tt.wantID {
				t.Errorf("mountIDForPath(%q) = %q, want %q", tt.path, id, tt.wantID)
			}
		})
	}

	// The property the whole fix exists for: store and library are the same
	// device but must read as DIFFERENT mounts, or the guard stays silent.
	store, _ := mountIDForPath([]byte(mountinfo222), "/srv/media/heyarr/cas/x")
	library, _ := mountIDForPath([]byte(mountinfo222), "/srv/media/Movies/y.mkv")
	if store == library {
		t.Fatalf("store mount %q and library mount %q compared EQUAL — the #222 blind spot", store, library)
	}
}

func TestPathHasPrefix(t *testing.T) {
	tests := []struct {
		path, dir string
		want      bool
	}{
		{"/srv/media/x", "/srv/media", true},
		{"/srv/media", "/srv/media", true},
		// The whole-component guard: /srv/mediafoo is NOT under /srv/media.
		{"/srv/mediafoo", "/srv/media", false},
		{"/anything", "/", true},
		{"/srv/media/heyarr/a", "/srv/media/heyarr", true},
		{"/srv/media/heyarrx", "/srv/media/heyarr", false},
	}
	for _, tt := range tests {
		if got := pathHasPrefix(tt.path, tt.dir); got != tt.want {
			t.Errorf("pathHasPrefix(%q, %q) = %v, want %v", tt.path, tt.dir, got, tt.want)
		}
	}
}

func TestUnescapeMountField(t *testing.T) {
	tests := map[string]string{
		"/srv/media":       "/srv/media",
		`/srv/my\040media`: "/srv/my media",  // space
		`/srv/tab\011here`: "/srv/tab\there", // tab
		`/back\134slash`:   `/back\slash`,    // backslash
		`/trailing\04`:     `/trailing\04`,   // too short to be an escape: left as-is
	}
	for in, want := range tests {
		if got := unescapeMountField(in); got != want {
			t.Errorf("unescapeMountField(%q) = %q, want %q", in, got, want)
		}
	}
}
