package playback_test

import (
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/playback"
)

// The matrix from #432: what a client that declares a decoder set is handed
// for each shape of source. The two live cases — AC-3 5.1 in MP4 with no AC-3
// decoder, and H.264+MP2 in AVI — are the first two rows.
func TestNegotiateDecidesTheLeg(t *testing.T) {
	phone := playback.ClientProfile{
		Containers:  []string{"mp4", "mkv", "webm"},
		VideoCodecs: []string{"h264", "hevc", "vp9", "av1"},
		AudioCodecs: []string{"aac", "opus", "mp3", "eac3"},
		MaxHeight:   1080,
	}
	withAC3 := phone
	withAC3.AudioCodecs = append([]string{"ac3"}, phone.AudioCodecs...)

	mp4 := func(video, audio string, height int) playback.MediaProfile {
		return playback.MediaProfile{
			Known: true, Container: "mov,mp4,m4a,3gp,3g2,mj2",
			VideoCodec: video, Width: height * 16 / 9, Height: height, AudioCodec: audio, Channels: 6,
		}
	}

	cases := []struct {
		name   string
		media  playback.MediaProfile
		client playback.ClientProfile
		want   playback.Leg
		reason string // a substring the reason sentence must carry
	}{
		{
			name:   "AC-3 in MP4, client without an AC-3 decoder: stream, video copied, audio transcoded",
			media:  mp4("h264", "ac3", 1080),
			client: phone,
			want:   playback.Leg{Mode: playback.LegStream, CopyVideo: true, CopyAudio: false},
			reason: "audio ac3 not decodable by client",
		},
		{
			name:   "the same file, client declaring ac3: direct",
			media:  mp4("h264", "ac3", 1080),
			client: withAC3,
			want:   playback.Leg{Mode: playback.LegDirect},
		},
		{
			name: "H.264 + MP2 in AVI: the container is the problem and the audio is too",
			media: playback.MediaProfile{
				Known: true, Container: "avi", VideoCodec: "h264", Width: 1280, Height: 720, AudioCodec: "mp2",
			},
			client: phone,
			want:   playback.Leg{Mode: playback.LegStream, CopyVideo: true, CopyAudio: false},
			reason: "container avi not playable by client; audio mp2 not decodable by client",
		},
		{
			name: "AVI with streams the client decodes: still a stream, both copied",
			media: playback.MediaProfile{
				Known: true, Container: "avi", VideoCodec: "h264", Height: 720, AudioCodec: "mp3",
			},
			client: phone,
			want:   playback.Leg{Mode: playback.LegStream, CopyVideo: true, CopyAudio: true},
			reason: "container avi not playable by client",
		},
		{
			name: "MKV with E-AC-3, client declaring eac3: direct",
			media: playback.MediaProfile{
				Known: true, Container: "matroska,webm", VideoCodec: "h264", Height: 1080, AudioCodec: "eac3",
			},
			client: phone,
			want:   playback.Leg{Mode: playback.LegDirect},
		},
		{
			name: "MKV with E-AC-3, client without it: stream with the audio transcoded",
			media: playback.MediaProfile{
				Known: true, Container: "matroska,webm", VideoCodec: "h264", Height: 1080, AudioCodec: "eac3",
			},
			client: playback.ClientProfile{Containers: []string{"mkv"}, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}},
			want:   playback.Leg{Mode: playback.LegStream, CopyVideo: true, CopyAudio: false},
			reason: "audio eac3 not decodable by client",
		},
		{
			name:   "H.264 + AAC in MP4: direct, and a clean direct carries no reasons",
			media:  mp4("h264", "aac", 1080),
			client: phone,
			want:   playback.Leg{Mode: playback.LegDirect},
		},
		{
			name:   "a video codec the client lacks: video is transcoded and the reason says so",
			media:  mp4("mpeg2video", "aac", 576),
			client: phone,
			want:   playback.Leg{Mode: playback.LegStream, CopyVideo: false, CopyAudio: true},
			reason: "video mpeg2video not decodable by client (transcoding video)",
		},
		{
			name:   "taller than max_height: video transcoded and capped, audio copied",
			media:  mp4("hevc", "aac", 2160),
			client: phone,
			want:   playback.Leg{Mode: playback.LegStream, CopyVideo: false, CopyAudio: true, TargetHeight: 1080},
			reason: "video 2160p exceeds client max_height 1080 (transcoding video)",
		},
		{
			name:   "no height limit stated: a 4K source is not capped",
			media:  mp4("hevc", "aac", 2160),
			client: playback.ClientProfile{Containers: []string{"mp4"}, VideoCodecs: []string{"hevc"}, AudioCodecs: []string{"aac"}},
			want:   playback.Leg{Mode: playback.LegDirect},
		},
		{
			name: "audio only, in a container the client takes: direct",
			media: playback.MediaProfile{
				Known: true, Container: "flac", AudioCodec: "flac",
			},
			client: playback.ClientProfile{Containers: []string{"flac"}, AudioCodecs: []string{"flac"}},
			want:   playback.Leg{Mode: playback.LegDirect},
		},
		{
			name:   "nothing probed: direct with the guess declared",
			media:  playback.MediaProfile{},
			client: phone,
			want: playback.Leg{Mode: playback.LegDirect, Reasons: []playback.Reason{{
				Code: playback.ReasonNoProbe,
				Detail: "nothing has probed these bytes, so the client is handed them as they are; " +
					"a probe would confirm it can decode them",
			}}},
		},
		{
			name:   "a client that declares nothing is handed the bytes",
			media:  mp4("h264", "ac3", 1080),
			client: playback.ClientProfile{MaxHeight: 720},
			want: playback.Leg{Mode: playback.LegDirect, Reasons: []playback.Reason{{
				Code:   playback.ReasonDeviceDeclaresNothing,
				Detail: "the client declares no codecs or containers, so nothing can be checked against it",
			}}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := playback.Negotiate(tc.media, tc.client)
			if got.Mode != tc.want.Mode {
				t.Fatalf("mode = %q, want %q (reason %q)", got.Mode, tc.want.Mode, got.Reason)
			}
			if got.CopyVideo != tc.want.CopyVideo || got.CopyAudio != tc.want.CopyAudio {
				t.Errorf("copy video/audio = %v/%v, want %v/%v",
					got.CopyVideo, got.CopyAudio, tc.want.CopyVideo, tc.want.CopyAudio)
			}
			if got.TargetHeight != tc.want.TargetHeight {
				t.Errorf("target height = %d, want %d", got.TargetHeight, tc.want.TargetHeight)
			}
			if tc.reason != "" && !strings.Contains(got.Reason, tc.reason) {
				t.Errorf("reason = %q, want it to carry %q", got.Reason, tc.reason)
			}
			if tc.reason == "" && got.Mode == playback.LegDirect && tc.want.Reasons == nil {
				if got.Reason != "" || len(got.Reasons) != 0 {
					t.Errorf("a clean direct carries reasons: %q %+v", got.Reason, got.Reasons)
				}
			}
			if tc.want.Reasons != nil {
				if len(got.Reasons) != len(tc.want.Reasons) {
					t.Fatalf("reasons = %+v, want %+v", got.Reasons, tc.want.Reasons)
				}
				for i := range tc.want.Reasons {
					if got.Reasons[i] != tc.want.Reasons[i] {
						t.Errorf("reason %d = %+v, want %+v", i, got.Reasons[i], tc.want.Reasons[i])
					}
				}
			}
			// A stream always says why, as codes AND as a sentence.
			if got.Mode == playback.LegStream && (got.Reason == "" || len(got.Reasons) == 0) {
				t.Errorf("a stream with no reason: %+v", got)
			}
		})
	}
}

// The client's declaration renders in the planner's shape unchanged, so Choose
// and Negotiate explain one file the same way.
func TestClientProfileRendersAsADeviceProfile(t *testing.T) {
	c := playback.ClientProfile{Containers: []string{"mp4"}, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}, MaxHeight: 720}
	d := c.DeviceProfile()
	if !d.Declares() || d.MaxHeight != 720 || len(d.AudioCodecs) != 1 {
		t.Errorf("device profile = %+v", d)
	}
	media := playback.MediaProfile{Known: true, Container: "mov,mp4", VideoCodec: "h264", AudioCodec: "ac3", Height: 720}
	plan := playback.Choose(media, d, []playback.Replica{{PeerID: "p", Local: true}})
	leg := playback.Negotiate(media, c)
	if plan.Decision != playback.DecisionTranscode || leg.Mode != playback.LegStream {
		t.Errorf("planner says %s, negotiation says %s — they disagree about one file", plan.Decision, leg.Mode)
	}
	if _, ok := plan.Reason(playback.ReasonAudioCodecUnsupported); !ok {
		t.Errorf("the planner's reasons lack the audio code: %+v", plan.Reasons)
	}
}
