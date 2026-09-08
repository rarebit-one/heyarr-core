package resources

import (
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/render"
)

// TestVideoURLWithCaptionCarriesTheSidecar checks the re-mint that lets the
// serve side advertise a subtitle in the CaptionInfo.sec header: it appends the
// sidecar to the video capability, leaves the video blob, type and expiry it
// already had untouched, preserves the cosmetic trailing name, and refuses a
// URL that is not this peer's (returning "" so the caller keeps the plain URL).
func TestVideoURLWithCaptionCarriesTheSidecar(t *testing.T) {
	t.Parallel()

	secret := []byte("0123456789abcdef0123456789abcdef")
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	a := &API{renderSecret: secret, renderBaseURL: "https://heyarr.example.com", now: func() time.Time { return now }}

	videoBlob := "blake3:" + strings.Repeat("a", 64)
	videoTok, err := render.Capability{BlobHash: videoBlob, ExpiresAt: now.Add(time.Hour), MIME: "video/mp4"}.Sign(secret)
	if err != nil {
		t.Fatal(err)
	}
	videoURL := a.renderBaseURL + render.Path(videoTok) + "/stream.mp4"

	capBlob := "blake3:" + strings.Repeat("e", 64)
	got := a.videoURLWithCaption(videoURL, capBlob, "application/x-subrip")
	if got == "" {
		t.Fatal("videoURLWithCaption returned empty for a URL of ours")
	}

	tok, name := renderTokenFromURL(a.renderBaseURL, got)
	if name != "stream.mp4" {
		t.Errorf("trailing name = %q, want stream.mp4 (preserved)", name)
	}
	c, err := render.Verify(secret, tok, now)
	if err != nil {
		t.Fatalf("re-minted token does not verify: %v", err)
	}
	if c.BlobHash != videoBlob {
		t.Errorf("video blob = %q, want unchanged %q", c.BlobHash, videoBlob)
	}
	if c.MIME != "video/mp4" {
		t.Errorf("video MIME = %q, want unchanged video/mp4", c.MIME)
	}
	if !c.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Errorf("expiry = %v, want unchanged %v", c.ExpiresAt, now.Add(time.Hour))
	}
	if c.CaptionBlob != capBlob {
		t.Errorf("caption blob = %q, want %q", c.CaptionBlob, capBlob)
	}
	if c.CaptionMIME != "application/x-subrip" {
		t.Errorf("caption MIME = %q, want application/x-subrip", c.CaptionMIME)
	}

	// A URL that is not one we minted re-mints nothing, so the cast keeps its
	// original (still-valid) video URL rather than a forgery.
	if foreign := a.videoURLWithCaption("https://evil.example/render/xxx/stream.mp4", capBlob, "application/x-subrip"); foreign != "" {
		t.Errorf("a foreign URL should not re-mint, got %q", foreign)
	}
}

// TestRenderTokenFromURL pins the small parser both halves rely on.
func TestRenderTokenFromURL(t *testing.T) {
	t.Parallel()

	const base = "https://heyarr.example.com"
	tests := []struct {
		name, url, wantTok, wantName string
	}{
		{"token and name", base + "/render/abc.def/stream.mp4", "abc.def", "stream.mp4"},
		{"token only", base + "/render/abc.def", "abc.def", ""},
		{"not ours", "https://other.example/render/abc/stream.mp4", "", ""},
		{"empty", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tok, name := renderTokenFromURL(base, tc.url)
			if tok != tc.wantTok || name != tc.wantName {
				t.Errorf("renderTokenFromURL(%q) = (%q, %q), want (%q, %q)", tc.url, tok, name, tc.wantTok, tc.wantName)
			}
		})
	}
}
