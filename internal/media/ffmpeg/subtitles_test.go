package ffmpeg

import (
	"slices"
	"testing"
)

func TestExtractArgsMapsExactlyOneStreamAsSubRip(t *testing.T) {
	t.Parallel()

	got := extractArgs("/blobs/movie.mkv", "/work/out.srt", 3)
	want := []string{
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-i", "/blobs/movie.mkv",
		"-map", "0:3",
		"-c:s", "srt",
		"-f", "srt",
		"/work/out.srt",
	}
	if !slices.Equal(got, want) {
		t.Errorf("extractArgs =\n  %v\nwant\n  %v", got, want)
	}
}

func TestProbeSubsArgsSelectsSubtitleStreamsAsJSON(t *testing.T) {
	t.Parallel()

	got := probeSubsArgs("/blobs/movie.mkv")
	// The two load-bearing choices: only subtitle streams, and JSON out.
	if !slices.Contains(got, "s") {
		t.Error("probeSubsArgs must select subtitle streams (-select_streams s)")
	}
	if i := slices.Index(got, "-of"); i < 0 || i+1 >= len(got) || got[i+1] != "json" {
		t.Errorf("probeSubsArgs must ask for JSON output, got %v", got)
	}
	if got[len(got)-1] != "/blobs/movie.mkv" {
		t.Errorf("probeSubsArgs must end with the source path, got %v", got)
	}
}

func TestIsTextSubtitleCodec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		codec string
		text  bool
	}{
		{"mov_text", true}, // the Yellowstone case: text inside .mp4
		{"subrip", true},
		{"SRT", true},   // case-insensitive
		{" ass ", true}, // trimmed
		{"webvtt", true},
		{"hdmv_pgs_subtitle", false}, // bitmap (Blu-ray) — needs OCR, skip
		{"dvd_subtitle", false},      // bitmap (DVD/VobSub) — skip
		{"dvb_subtitle", false},      // bitmap — skip
		{"", false},
		{"something_new", false}, // unknown → skip, not guess
	}
	for _, tc := range tests {
		if got := IsTextSubtitleCodec(tc.codec); got != tc.text {
			t.Errorf("IsTextSubtitleCodec(%q) = %v, want %v", tc.codec, got, tc.text)
		}
	}
}

func TestNormaliseLangFoldsUndeterminedToEmpty(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"eng":  "eng",
		"EN":   "en",
		" fr ": "fr",
		"und":  "", // ffmpeg's "undetermined" sentinel is not a language
		"":     "",
	}
	for in, want := range tests {
		if got := normaliseLang(in); got != want {
			t.Errorf("normaliseLang(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractDedupeKeyIsPerBlob(t *testing.T) {
	t.Parallel()

	a := ExtractDedupeKey("blake3:aaa")
	b := ExtractDedupeKey("blake3:bbb")
	if a == b {
		t.Error("two different blobs must get different dedupe keys")
	}
	if a != ExtractDedupeKey("blake3:aaa") {
		t.Error("the same blob must get a stable dedupe key")
	}
}
