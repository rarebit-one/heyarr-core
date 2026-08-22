package playback_test

import (
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/playback"
)

// A television that takes almost everything.
func tv() playback.DeviceProfile {
	return playback.DeviceProfile{
		Containers:    []string{"mp4", "mkv"},
		VideoCodecs:   []string{"h264", "hevc"},
		AudioCodecs:   []string{"aac", "eac3"},
		MaxWidth:      3840,
		MaxHeight:     2160,
		MaxBitrateBPS: 120_000_000,
		SupportsHDR:   true,
	}
}

// A deliberately limited device: MP4 only, H.264 only, 1080p, no HDR.
func limited() playback.DeviceProfile {
	return playback.DeviceProfile{
		Containers:  []string{"mp4"},
		VideoCodecs: []string{"h264"},
		AudioCodecs: []string{"aac"},
		MaxWidth:    1920,
		MaxHeight:   1080,
	}
}

func h264MP4() playback.MediaProfile {
	return playback.MediaProfile{
		Known: true, Container: "mov,mp4,m4a,3gp,3g2,mj2",
		VideoCodec: "h264", Width: 1920, Height: 1080,
		AudioCodec: "aac", Channels: 2, BitrateBPS: 8_000_000,
	}
}

var localOnly = []playback.Replica{{PeerID: "peer-local", Local: true}}

// The table §68 exists for. Every cell asserts the VERDICT and the REASON,
// because a verdict without its reason is half the deliverable: "why is my
// television transcoding this" is the question this planner is asked, and one
// that answers only with a verb cannot answer it.
func TestThePlannerTable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		media      playback.MediaProfile
		device     playback.DeviceProfile
		want       playback.Decision
		wantReason string
	}{
		{
			name:  "matching codecs and container",
			media: h264MP4(), device: tv(),
			want: playback.DecisionDirect,
		},
		{
			// The REMUX case: right streams, wrong wrapper.
			name: "the right streams in a container the device refuses",
			media: func() playback.MediaProfile {
				m := h264MP4()
				m.Container = "matroska,webm"
				return m
			}(), device: limited(),
			want: playback.DecisionRemux, wantReason: playback.ReasonContainerUnsupported,
		},
		{
			name: "matroska on a device that takes it",
			media: func() playback.MediaProfile {
				m := h264MP4()
				m.Container = "matroska,webm"
				return m
			}(), device: tv(),
			want: playback.DecisionDirect,
		},
		{
			name: "a video codec the device refuses",
			media: func() playback.MediaProfile {
				m := h264MP4()
				m.VideoCodec = "hevc"
				return m
			}(), device: limited(),
			want: playback.DecisionTranscode, wantReason: playback.ReasonVideoCodecUnsupported,
		},
		{
			name: "an audio codec the device refuses, with supported video",
			media: func() playback.MediaProfile {
				m := h264MP4()
				m.AudioCodec = "truehd"
				return m
			}(), device: limited(),
			want: playback.DecisionTranscode, wantReason: playback.ReasonAudioCodecUnsupported,
		},
		{
			name: "a resolution past the device's maximum",
			media: func() playback.MediaProfile {
				m := h264MP4()
				m.Width, m.Height = 3840, 2160
				return m
			}(), device: limited(),
			want: playback.DecisionTranscode, wantReason: playback.ReasonResolutionTooHigh,
		},
		{
			name: "a bitrate past the device's maximum",
			media: func() playback.MediaProfile {
				m := h264MP4()
				m.BitrateBPS = 200_000_000
				return m
			}(), device: tv(),
			want: playback.DecisionTranscode, wantReason: playback.ReasonBitrateTooHigh,
		},
		{
			name: "HDR on a device without it",
			media: func() playback.MediaProfile {
				m := h264MP4()
				m.HDR = true
				return m
			}(), device: limited(),
			want: playback.DecisionTranscode, wantReason: playback.ReasonHDRUnsupported,
		},
		{
			// A device that declares nothing is a legitimate thing to be
			// (M2-05), and the planner has to reach an answer about it.
			name:  "a device that declares nothing",
			media: h264MP4(), device: playback.DeviceProfile{},
			want: playback.DecisionDirect, wantReason: playback.ReasonDeviceDeclaresNothing,
		},
		{
			// The ADR-0023 case, and the one most likely to be got wrong.
			name:  "media nothing has probed",
			media: playback.MediaProfile{}, device: tv(),
			want: playback.DecisionDirect, wantReason: playback.ReasonNoProbe,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := playback.Choose(tc.media, tc.device, localOnly)
			if plan.Decision != tc.want {
				t.Errorf("decision = %q, want %q (reasons: %+v)", plan.Decision, tc.want, plan.Reasons)
			}
			if tc.wantReason == "" {
				if len(plan.Reasons) != 0 {
					t.Errorf("a clean %s carried reasons: %+v", tc.want, plan.Reasons)
				}
				return
			}
			r, ok := plan.Reason(tc.wantReason)
			if !ok {
				t.Fatalf("no %q reason in %+v", tc.wantReason, plan.Reasons)
			}
			if strings.TrimSpace(r.Detail) == "" {
				t.Errorf("the %q reason has no detail", tc.wantReason)
			}
		})
	}
}

// A device with two problems has two problems. Telling the operator about one
// of them produces a second question.
func TestEveryReasonIsReported(t *testing.T) {
	media := h264MP4()
	media.VideoCodec = "av1"
	media.Width, media.Height = 3840, 2160
	media.HDR = true
	media.Container = "matroska,webm"

	plan := playback.Choose(media, limited(), localOnly)
	if plan.Decision != playback.DecisionTranscode {
		t.Fatalf("decision = %q", plan.Decision)
	}
	for _, want := range []string{
		playback.ReasonVideoCodecUnsupported,
		playback.ReasonResolutionTooHigh,
		playback.ReasonHDRUnsupported,
		// The container cannot be fixed by a remux here — the transcode will
		// rewrite it anyway — but it is still true and the operator asking
		// "why" deserves the whole answer.
		playback.ReasonContainerUnsupported,
	} {
		if _, ok := plan.Reason(want); !ok {
			t.Errorf("missing reason %q; got %+v", want, plan.Reasons)
		}
	}
}

// §31: local replica use is strongly preferred; cross-site streaming is
// fallback behaviour, not the norm.
//
// The replicas here are SYNTHETIC, and that is now a division of labour rather
// than a caveat: this table owns the preference, and §32's routing exercises it
// against a real second peer (M4-14). See the note on Plan.Remote.
func TestLocalityPreference(t *testing.T) {
	local := playback.Replica{PeerID: "local", Local: true}
	remote := playback.Replica{PeerID: "remote"}

	// A local replica wins even when a remote one is listed first.
	plan := playback.Choose(h264MP4(), tv(), []playback.Replica{remote, local})
	if plan.PeerID != "local" || plan.Remote {
		t.Errorf("chose %+v, want the local replica", plan)
	}
	if _, ok := plan.Reason(playback.ReasonRemoteReplicaOnly); ok {
		t.Error("a local playback was flagged as remote-only")
	}

	// A remote replica is used rather than refused when it is all there is,
	// and the fact is recorded: an instance where every playback is remote is
	// one where replication is not working.
	plan = playback.Choose(h264MP4(), tv(), []playback.Replica{remote})
	if plan.PeerID != "remote" || !plan.Remote {
		t.Errorf("chose %+v, want the remote replica", plan)
	}
	if _, ok := plan.Reason(playback.ReasonRemoteReplicaOnly); !ok {
		t.Errorf("a remote-only playback was not flagged: %+v", plan.Reasons)
	}
	// Remote is not a reason to avoid DIRECT — a remote replica plays fine.
	if plan.Decision != playback.DecisionDirect {
		t.Errorf("decision = %q, want direct over a remote replica", plan.Decision)
	}
}

// No replica is an ANSWER, not an error: the client asked what to do and the
// honest reply is "nothing, and here is why".
func TestNoReplicaIsUnplayableWithAReason(t *testing.T) {
	plan := playback.Choose(h264MP4(), tv(), nil)
	if plan.Decision != playback.DecisionUnplayable {
		t.Fatalf("decision = %q, want unplayable", plan.Decision)
	}
	if _, ok := plan.Reason(playback.ReasonNoReplica); !ok {
		t.Errorf("no reason given: %+v", plan.Reasons)
	}
	if plan.PeerID != "" {
		t.Errorf("an unplayable plan named a peer: %q", plan.PeerID)
	}
}

// ffprobe's format_name is a comma-separated list of every name the demuxer
// answers to. A device declaring "mp4" can play "mov,mp4,m4a,3gp,3g2,mj2", and
// an equality check would send every MP4 in the library to a remux.
func TestContainerMatchingIsByMembership(t *testing.T) {
	for _, tc := range []struct {
		container string
		declared  []string
		want      playback.Decision
	}{
		{"mov,mp4,m4a,3gp,3g2,mj2", []string{"mp4"}, playback.DecisionDirect},
		// mkv is an alias for the matroska demuxer, not a different container.
		// This case asserted DecisionRemux until the alias table existed, and
		// that expectation was the bug written down.
		{"matroska,webm", []string{"mkv"}, playback.DecisionDirect},
		{"matroska,webm", []string{"webm"}, playback.DecisionDirect},
		{"MOV,MP4", []string{"mp4"}, playback.DecisionDirect},
		{"mov,mp4", []string{" MP4 "}, playback.DecisionDirect},
		{"avi", []string{"mp4", "mkv"}, playback.DecisionRemux},
	} {
		t.Run(tc.container+"/"+strings.Join(tc.declared, "+"), func(t *testing.T) {
			media := h264MP4()
			media.Container = tc.container
			device := limited()
			device.Containers = tc.declared
			if got := playback.Choose(media, device, localOnly).Decision; got != tc.want {
				t.Errorf("decision = %q, want %q", got, tc.want)
			}
		})
	}
}

// A zero maximum means "no limit stated", not a limit of zero. A client that
// omits max_bitrate_bps is not claiming it can play nothing, and treating it as
// zero would transcode every file in the library.
func TestAZeroMaximumIsNoLimit(t *testing.T) {
	media := h264MP4()
	media.Width, media.Height = 7680, 4320
	media.BitrateBPS = 900_000_000

	device := playback.DeviceProfile{
		Containers:  []string{"mp4"},
		VideoCodecs: []string{"h264"},
		AudioCodecs: []string{"aac"},
		// No maxima declared at all.
	}
	plan := playback.Choose(media, device, localOnly)
	if plan.Decision != playback.DecisionDirect {
		t.Errorf("decision = %q, want direct — an undeclared maximum is not a maximum of zero (%+v)",
			plan.Decision, plan.Reasons)
	}
}

// Half a resolution limit cannot happen through the API (M2-05 refuses it),
// but the planner must not fall over if it ever sees one from another writer.
func TestHalfAResolutionLimitIsIgnoredRatherThanGuessed(t *testing.T) {
	media := h264MP4()
	media.Width, media.Height = 3840, 2160

	device := limited()
	device.MaxHeight = 0 // width only
	if plan := playback.Choose(media, device, localOnly); plan.Decision != playback.DecisionDirect {
		t.Errorf("decision = %q; half a limit should be ignored, not guessed at (%+v)",
			plan.Decision, plan.Reasons)
	}
}

// The demuxer's vocabulary and a device's are not the same, and pretending
// otherwise sends the whole library to a remux.
//
// ffprobe reports Matroska as "matroska,webm". Every device on earth calls it
// "mkv". The first version of this planner matched by literal membership and
// told a television declaring "mkv" to remux every Matroska file it had — the
// exact complaint the planner exists to be able to answer, caused by the
// planner. The table test caught it.
func TestDemuxerNamesAreMatchedAgainstTheNamesDevicesUse(t *testing.T) {
	for _, tc := range []struct {
		container string
		declares  string
		want      playback.Decision
	}{
		{"matroska,webm", "mkv", playback.DecisionDirect},
		{"mov,mp4,m4a,3gp,3g2,mj2", "m4v", playback.DecisionDirect},
		{"mpegts", "ts", playback.DecisionDirect},
		{"asf", "wmv", playback.DecisionDirect},
		// And it must not become a rule that matches everything: a device that
		// declares only mp4 still cannot take Matroska.
		{"matroska,webm", "mp4", playback.DecisionRemux},
		{"avi", "mkv", playback.DecisionRemux},
	} {
		t.Run(tc.container+"→"+tc.declares, func(t *testing.T) {
			media := h264MP4()
			media.Container = tc.container
			device := limited()
			device.Containers = []string{tc.declares}
			if got := playback.Choose(media, device, localOnly).Decision; got != tc.want {
				t.Errorf("decision = %q, want %q", got, tc.want)
			}
		})
	}
}
