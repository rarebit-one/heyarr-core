package probe

import "testing"

// HDR is read from the transfer characteristic, not guessed from bit depth: a
// ten-bit HEVC encode is as often SDR as not, and choosing an SDR copy for a
// client that cannot show HDR turns on this flag being right (#885).
func TestHDRIsTheTransferCharacteristic(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "PQ, as HDR10 and Dolby Vision carry it",
			raw:  `{"format":{"format_name":"matroska"},"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","profile":"Main 10","height":2160,"color_transfer":"smpte2084"}]}`,
			want: true,
		},
		{
			name: "HLG",
			raw:  `{"format":{"format_name":"matroska"},"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","profile":"Main 10","height":2160,"color_transfer":"arib-std-b67"}]}`,
			want: true,
		},
		{
			name: "ten-bit SDR",
			raw:  `{"format":{"format_name":"matroska"},"streams":[{"index":0,"codec_type":"video","codec_name":"hevc","profile":"Main 10","height":1080,"color_transfer":"bt709"}]}`,
		},
		{
			name: "no transfer reported",
			raw:  `{"format":{"format_name":"mov,mp4"},"streams":[{"index":0,"codec_type":"video","codec_name":"h264","profile":"High","height":1080}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := parseAndCheck([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			video, ok := result.VideoStream()
			if !ok {
				t.Fatal("no video stream")
			}
			if got := video.HDR(); got != tc.want {
				t.Errorf("HDR() = %v, want %v (transfer %q)", got, tc.want, video.ColorTransfer)
			}
		})
	}
}
