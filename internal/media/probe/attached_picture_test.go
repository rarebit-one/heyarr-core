package probe

import "testing"

func TestAttachedPictureIsNotPlaybackVideo(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		codec string
	}{
		{
			name: "audio with album artwork",
			raw:  `{"format":{"format_name":"mp3"},"streams":[{"index":0,"codec_type":"audio","codec_name":"mp3"},{"index":1,"codec_type":"video","codec_name":"mjpeg","width":500,"height":500,"disposition":{"attached_pic":1}}]}`,
		},
		{
			name:  "artwork before real video",
			raw:   `{"format":{"format_name":"matroska"},"streams":[{"index":0,"codec_type":"video","codec_name":"mjpeg","disposition":{"attached_pic":1}},{"index":1,"codec_type":"video","codec_name":"h264","width":1920,"height":1080}]}`,
			codec: "h264",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := parseAndCheck([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			video, ok := result.VideoStream()
			if tc.codec == "" {
				if ok {
					t.Fatalf("album artwork was selected as playback video: %+v", video)
				}
				return
			}
			if !ok || video.Codec != tc.codec {
				t.Fatalf("playback video = %+v, present = %v; want codec %s", video, ok, tc.codec)
			}
		})
	}
}
