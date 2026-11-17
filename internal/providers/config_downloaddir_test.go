package providers

import "testing"

// download_dir (#882): empty is the session default; set, it must be an
// absolute path on a Transmission provider and is refused everywhere else.
func TestResolveDownloadDir(t *testing.T) {
	for raw, want := range map[string]string{"": "", "  ": "", "/srv/media/downloads": "/srv/media/downloads", " /dl ": "/dl"} {
		got, err := resolveDownloadDir("tr", KindTransmission, raw)
		if err != nil || got != want {
			t.Errorf("resolveDownloadDir(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := resolveDownloadDir("tr", KindTransmission, "downloads/heyarr"); err == nil {
		t.Error("a relative download_dir was accepted")
	}
	if _, err := resolveDownloadDir("qb", KindQBittorrent, "/dl"); err == nil {
		t.Error("download_dir on a qbittorrent provider was accepted: it would be silently ignored")
	}
}
