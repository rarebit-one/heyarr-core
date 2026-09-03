package playback

import (
	"fmt"
	"strings"
)

// The device-aware streaming leg (§33, §68, ADR-0069).
//
// A CLIENT — a phone, a browser, a set-top box — says what it can decode, and
// this decides whether it may play the bytes as they are or needs the node to
// repackage them on the fly. It is the same question the planner asks with a
// registered device (Choose), narrowed to the one answer a client has to act
// on: fetch the blob, or fetch a stream.
//
// # Why this is a second function rather than a fourth Decision
//
// Choose answers "what would it take"; it says REMUX or TRANSCODE and stops,
// because until now nothing in the tree produced either on demand (#202). This
// answers "what do I fetch", and carries what the repackager needs — copy the
// video or re-encode it, copy the audio or re-encode it, cap the height — so
// the API can hand ffmpeg a spec without re-deriving the decision. Both stay
// pure: no database, no subprocess, exhaustively table-testable.
//
// # The stream is always fragmented MP4
//
// One output shape, chosen for what a Media3 or browser player takes without
// a plugin. Video is COPIED whenever the client can decode it — a repackage
// that re-encodes h264 to h264 is a transcode wearing a remux's name, and it
// costs a core per stream for nothing. Audio is transcoded to stereo AAC
// whenever the client cannot decode the source, which is the whole of the
// AC-3 and MP2 cases that opened #432.

// ClientProfile is what a client declared it can decode, in the vocabulary a
// client uses: container names ("mp4", "mkv"), codec names as ffprobe reports
// them ("h264", "ac3"), and a height ceiling.
type ClientProfile struct {
	Containers  []string
	VideoCodecs []string
	AudioCodecs []string
	// MaxHeight is the tallest picture the client will take. Zero means no
	// limit stated.
	MaxHeight int
}

// Declares reports whether the client said anything at all. A client that
// declares nothing cannot be matched against, and is handed the bytes.
func (c ClientProfile) Declares() bool {
	return len(c.Containers) > 0 || len(c.VideoCodecs) > 0 || len(c.AudioCodecs) > 0
}

// DeviceProfile renders the client's declaration in the planner's shape, so
// Choose can decide and explain it exactly as it would for a registered
// device. One vocabulary, one explanation.
func (c ClientProfile) DeviceProfile() DeviceProfile {
	return DeviceProfile{
		Containers:  c.Containers,
		VideoCodecs: c.VideoCodecs,
		AudioCodecs: c.AudioCodecs,
		MaxHeight:   int64(c.MaxHeight),
	}
}

// LegMode is what the client fetches.
type LegMode string

const (
	// LegDirect means the ordinary blob endpoint (ADR-0013): the bytes play
	// as they are.
	LegDirect LegMode = "direct"
	// LegStream means the on-the-fly repackage: fragmented MP4 produced by
	// ffmpeg from the blob, one process per client.
	LegStream LegMode = "stream"
)

// Leg is the answer: what to fetch, why, and — for a stream — what the
// repackager must do to the streams.
type Leg struct {
	Mode LegMode
	// Reason is one sentence for a human or a log line, empty for a clean
	// direct. Reasons carries the same facts as stable codes.
	Reason  string
	Reasons []Reason
	// CopyVideo and CopyAudio say whether the stream carries the source
	// stream unchanged or a re-encode. Meaningful only when Mode is stream.
	CopyVideo bool
	CopyAudio bool
	// TargetHeight is the height to scale down to, when the source is taller
	// than the client takes. Zero means keep the source height.
	TargetHeight int
}

// Direct reports whether the client should fetch the blob itself.
func (l Leg) Direct() bool { return l.Mode == LegDirect }

// fmp4Video is every video codec that can be carried in fragmented MP4 without
// re-encoding. A codec outside this set is re-encoded even when the client
// could decode it, because the container could not carry it.
var fmp4Video = []string{"h264", "hevc", "av1", "vp9", "mpeg4"}

// fmp4Audio is the same set for audio. AC-3 and E-AC-3 ARE muxable — a client
// that declares them gets the original track copied; only a client that does
// not declare them gets AAC.
var fmp4Audio = []string{"aac", "mp3", "ac3", "eac3", "opus", "flac", "alac"}

// Negotiate decides the leg for a client against a probed media profile.
//
// The direction of every default is "hand the client the bytes": media nothing
// has probed, or a client that declares nothing, both get direct, with the
// reason attached — the same stance Choose takes and for the same reason (a
// node with no probes is a node with no ffprobe, and it cannot repackage
// either). Only a KNOWN incompatibility earns a stream.
func Negotiate(media MediaProfile, client ClientProfile) Leg {
	if !media.Known {
		return Leg{Mode: LegDirect, Reasons: []Reason{{
			Code: ReasonNoProbe,
			Detail: "nothing has probed these bytes, so the client is handed them as they are; " +
				"a probe would confirm it can decode them",
		}}}
	}
	if !client.Declares() {
		return Leg{Mode: LegDirect, Reasons: []Reason{{
			Code:   ReasonDeviceDeclaresNothing,
			Detail: "the client declares no codecs or containers, so nothing can be checked against it",
		}}}
	}

	var (
		leg       = Leg{Mode: LegDirect, CopyVideo: true, CopyAudio: true}
		sentences []string
	)
	// Every incompatibility is collected, not just the first: the operator
	// asking "why is my phone being served a stream" deserves the whole
	// answer, and the reason string names each one.
	if media.Container != "" && !supportsContainer(client.Containers, media.Container) {
		leg.Mode = LegStream
		leg.Reasons = append(leg.Reasons, containerReason(media.Container))
		sentences = append(sentences, fmt.Sprintf("container %s not playable by client", firstName(media.Container)))
	}
	if media.VideoCodec != "" {
		switch {
		case !contains(client.VideoCodecs, media.VideoCodec):
			leg.Mode, leg.CopyVideo = LegStream, false
			leg.Reasons = append(leg.Reasons, Reason{
				Code:   ReasonVideoCodecUnsupported,
				Detail: fmt.Sprintf("the client does not declare %s video", media.VideoCodec),
			})
			sentences = append(sentences, fmt.Sprintf("video %s not decodable by client (transcoding video)", media.VideoCodec))
		case !contains(fmp4Video, media.VideoCodec):
			// Decodable, but fragmented MP4 cannot carry it as it is.
			leg.Mode, leg.CopyVideo = LegStream, false
			leg.Reasons = append(leg.Reasons, Reason{
				Code:   ReasonVideoCodecUnsupported,
				Detail: fmt.Sprintf("%s video cannot be carried in fragmented MP4 without re-encoding", media.VideoCodec),
			})
			sentences = append(sentences, fmt.Sprintf("video %s cannot ride fragmented MP4 (transcoding video)", media.VideoCodec))
		}
		if client.MaxHeight > 0 && media.Height > client.MaxHeight {
			leg.Mode, leg.CopyVideo = LegStream, false
			leg.TargetHeight = client.MaxHeight
			leg.Reasons = append(leg.Reasons, Reason{
				Code:   ReasonResolutionTooHigh,
				Detail: fmt.Sprintf("%dx%d is past the client's max height %d", media.Width, media.Height, client.MaxHeight),
			})
			sentences = append(sentences, fmt.Sprintf("video %dp exceeds client max_height %d (transcoding video)", media.Height, client.MaxHeight))
		}
	}
	if media.AudioCodec != "" {
		switch {
		case !contains(client.AudioCodecs, media.AudioCodec):
			leg.Mode, leg.CopyAudio = LegStream, false
			leg.Reasons = append(leg.Reasons, Reason{
				Code:   ReasonAudioCodecUnsupported,
				Detail: fmt.Sprintf("the client does not declare %s audio", media.AudioCodec),
			})
			sentences = append(sentences, fmt.Sprintf("audio %s not decodable by client", media.AudioCodec))
		case !contains(fmp4Audio, media.AudioCodec):
			leg.Mode, leg.CopyAudio = LegStream, false
			leg.Reasons = append(leg.Reasons, Reason{
				Code:   ReasonAudioCodecUnsupported,
				Detail: fmt.Sprintf("%s audio cannot be carried in fragmented MP4 without re-encoding", media.AudioCodec),
			})
			sentences = append(sentences, fmt.Sprintf("audio %s cannot ride fragmented MP4 (transcoding audio)", media.AudioCodec))
		}
	}

	if leg.Mode == LegDirect {
		// A clean direct copies nothing; the flags are about a stream.
		return Leg{Mode: LegDirect}
	}
	leg.Reason = strings.Join(sentences, "; ")
	return leg
}

// firstName is the first of ffprobe's comma-separated demuxer names —
// "mov,mp4,m4a,3gp,3g2,mj2" reads as "mov" in a sentence, and "avi" as "avi".
func firstName(container string) string {
	name, _, _ := strings.Cut(container, ",")
	return strings.TrimSpace(name)
}
