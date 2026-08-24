package playback

import (
	"slices"
	"testing"
)

func TestProfileFromProtocolInfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		sink          string
		wantContainer []string
		wantVideo     []string
		wantAudio     []string
	}{
		{
			name:          "a profiled entry gives a container and both codecs",
			sink:          "http-get:*:video/mp4:DLNA.ORG_PN=AVC_MP4_MP_HD_AAC_MULT5;DLNA.ORG_OP=01",
			wantContainer: []string{"mp4"},
			wantVideo:     []string{"h264"},
			wantAudio:     []string{"aac"},
		},
		{
			// The Samsung case. Without this the television's only statement
			// of HEVC and Matroska support is discarded.
			name:          "an unprofiled wildcard entry still counts",
			sink:          "http-get:*:video/x-mkv:*,http-get:*:video/hevc:*",
			wantContainer: []string{"mkv"},
			wantVideo:     []string{"hevc"},
		},
		{
			name:          "MPEG1_L3 is mp3, not a video codec",
			sink:          "http-get:*:video/vnd.dlna.mpeg-tts:DLNA.ORG_PN=AVC_TS_MP_SD_MPEG1_L3",
			wantContainer: []string{"ts"},
			wantVideo:     []string{"h264"},
			wantAudio:     []string{"mp3"},
		},
		{
			name:          "HEAAC and BSAC are decoded as aac",
			sink:          "http-get:*:video/mp4:DLNA.ORG_PN=AVC_MP4_BL_CIF15_HEAAC,http-get:*:video/mp4:DLNA.ORG_PN=AVC_MP4_BL_CIF30_BSAC",
			wantContainer: []string{"mp4"},
			wantVideo:     []string{"h264"},
			wantAudio:     []string{"aac"},
		},
		{
			// Heyarr serves blobs over HTTP and nothing else, so a renderer's
			// RTP support describes a conversation that will never happen.
			name:          "non-http-get protocols are ignored",
			sink:          "rtsp-rtp-udp:*:video/mp4:DLNA.ORG_PN=AVC_MP4_MP_HD_AAC_MULT5,http-get:*:audio/mpeg:*",
			wantContainer: []string{"mp3"},
			wantAudio:     []string{"mp3"},
		},
		{
			name:          "a container appears once however often it is declared",
			sink:          "http-get:*:video/mp4:*,http-get:*:video/mp4:*,http-get:*:video/mp4:*",
			wantContainer: []string{"mp4"},
		},
		{name: "an empty sink yields an empty profile", sink: ""},
		{name: "malformed entries are skipped rather than fatal", sink: "nonsense,,http-get:*"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ProfileFromProtocolInfo(tc.sink)
			assertExactly(t, "containers", got.Containers, tc.wantContainer)
			assertExactly(t, "video codecs", got.VideoCodecs, tc.wantVideo)
			assertExactly(t, "audio codecs", got.AudioCodecs, tc.wantAudio)
		})
	}
}

// TestProfileFromProtocolInfoStatesNoLimits pins the decision not to invent a
// resolution ceiling from a DLNA profile name. A wrong ceiling makes the
// planner refuse content the device would have played, which is worse than no
// ceiling at all.
func TestProfileFromProtocolInfoStatesNoLimits(t *testing.T) {
	t.Parallel()

	got := ProfileFromProtocolInfo("http-get:*:video/mp4:DLNA.ORG_PN=AVC_MP4_MP_HD_AAC_MULT5")
	if got.MaxWidth != 0 || got.MaxHeight != 0 || got.MaxBitrateBPS != 0 || got.SupportsHDR {
		t.Errorf("limits were derived from a profile name: %+v", got)
	}
	if !got.Declares() {
		t.Error("a profile with codecs must report that it declares something")
	}
}

func assertExactly(t *testing.T, kind string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", kind, got, want)
		return
	}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("%s = %v, want %v", kind, got, want)
			return
		}
	}
}
