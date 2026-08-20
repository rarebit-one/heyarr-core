package playback

import (
	"fmt"
	"strings"
)

// The playback planner (§68).
//
// It takes an Asset's media profile, a device's declared capabilities and what
// replicas exist, and chooses DIRECT, REMUX or TRANSCODE.
//
// # A pure function, and nothing else
//
// No database, no filesystem, no subprocess. That is what makes the
// interesting behaviour — the combinatorics of codec × container × profile ×
// locality — exhaustively table-testable, which matters more here than almost
// anywhere else in the codebase: any design that needed a running system to
// test one cell of that table would leave most cells untested.
//
// # The decision carries its reasons
//
// Not just TRANSCODE, but *why not DIRECT*. "Why is my television transcoding
// this" is the single most common question this class of software gets asked,
// and a planner that answers only with a verdict cannot answer it. The reasons
// are part of the returned value and part of the API response, not a log line
// somebody has to go and find.

// Decision is what a client should do with an asset (§68).
type Decision string

const (
	// DecisionDirect means the bytes play as they are.
	DecisionDirect Decision = "direct"
	// DecisionRemux means the streams are fine and the container is not.
	DecisionRemux Decision = "remux"
	// DecisionTranscode means at least one stream must be re-encoded.
	DecisionTranscode Decision = "transcode"
	// DecisionUnplayable means no plan exists — there is nothing to play from.
	//
	// It is a decision rather than an error because it is an ANSWER: the
	// client asked what to do and the honest reply is "nothing, and here is
	// why". Returning an error would push a normal, explicable outcome into
	// the same channel as "the planner broke".
	DecisionUnplayable Decision = "unplayable"
)

// Reason codes. They are stable strings because clients branch on them, and a
// client branching on prose is a client that breaks when the prose improves.
const (
	ReasonNoReplica             = "no_replica"
	ReasonContainerUnsupported  = "container_unsupported"
	ReasonVideoCodecUnsupported = "video_codec_unsupported"
	ReasonAudioCodecUnsupported = "audio_codec_unsupported"
	ReasonResolutionTooHigh     = "resolution_too_high"
	ReasonBitrateTooHigh        = "bitrate_too_high"
	ReasonHDRUnsupported        = "hdr_unsupported"
	ReasonNoProbe               = "no_probe"
	ReasonRemoteReplicaOnly     = "remote_replica_only"
	ReasonDeviceDeclaresNothing = "device_declares_nothing"
)

// Reason is one contribution to a decision.
type Reason struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// MediaProfile is what the planner needs to know about the bytes.
//
// It is a domain value rather than a probe.Result, so that this package stays
// free of anything that shells out or touches the network. The mapping lives at
// the edge, and the planner needs a handful of facts rather than ffprobe's
// whole output.
type MediaProfile struct {
	// Known is false when nothing has probed these bytes — a node with no
	// ffprobe (ADR-0023), or a probe still pending. It is not the same as an
	// empty profile, and the difference decides the plan.
	Known bool
	// Container is ffprobe's format_name: a comma-separated list of every name
	// the demuxer answers to. Matching is by membership, because which of
	// those a file "is" is a question with no answer.
	Container  string
	VideoCodec string
	Width      int
	Height     int
	HDR        bool
	AudioCodec string
	Channels   int
	BitrateBPS int64
}

// DeviceProfile is what a device declared it can play (§68, M2-05).
//
// A zero maximum means "no limit stated", which is deliberately different from
// a limit of zero: a client that omits a maximum bitrate is not claiming it can
// play nothing.
type DeviceProfile struct {
	Containers    []string
	VideoCodecs   []string
	AudioCodecs   []string
	MaxWidth      int64
	MaxHeight     int64
	MaxBitrateBPS int64
	SupportsHDR   bool
}

// Declares reports whether the device said anything at all about what it can
// play. A device that declares nothing is a legitimate thing to be (M2-05) and
// the planner has to reach an answer about it.
func (d DeviceProfile) Declares() bool {
	return len(d.Containers) > 0 || len(d.VideoCodecs) > 0 || len(d.AudioCodecs) > 0
}

// Replica is one place the bytes are.
type Replica struct {
	PeerID string
	// Local means the same site as the client (§31). Cross-site streaming is
	// fallback behaviour, not the norm.
	Local bool
}

// Plan is the planner's answer.
type Plan struct {
	Decision Decision `json:"decision"`
	// Reasons is why the decision is not DIRECT, or why it is unplayable.
	// Empty for a clean DIRECT.
	Reasons []Reason `json:"reasons"`
	// PeerID is the replica chosen to serve from. Empty when unplayable.
	PeerID string `json:"peer_id,omitempty"`
	// Remote reports that the chosen replica is at another site.
	//
	// ## UNPROVEN
	//
	// Nothing has ever run against a second peer. The peer model exists with
	// exactly one peer by design (ADR-0010), so the local-vs-remote
	// distinction that §31 and §68 both turn on has never been exercised
	// against reality. The preference below is unit-tested against synthetic
	// replicas and nothing more; treat this field as untested until Milestone
	// 4 stands a second peer up.
	Remote bool `json:"remote"`
}

// Direct reports whether the plan needs no processing.
func (p Plan) Direct() bool { return p.Decision == DecisionDirect }

// Reason returns the first reason with the given code, if any.
func (p Plan) Reason(code string) (Reason, bool) {
	for _, r := range p.Reasons {
		if r.Code == code {
			return r, true
		}
	}
	return Reason{}, false
}

// Choose plans a playback (§68).
func Choose(media MediaProfile, device DeviceProfile, replicas []Replica) Plan {
	source, ok := chooseReplica(replicas)
	if !ok {
		return Plan{
			Decision: DecisionUnplayable,
			Reasons: []Reason{{
				Code:   ReasonNoReplica,
				Detail: "no peer holds these bytes",
			}},
		}
	}

	plan := Plan{PeerID: source.PeerID, Remote: !source.Local}
	if plan.Remote {
		// Not a reason to avoid DIRECT — a remote replica plays perfectly
		// well. It is recorded because §31 says cross-site streaming should be
		// the exception, and an instance where every playback is remote is one
		// where replication is not working.
		plan.Reasons = append(plan.Reasons, Reason{
			Code:   ReasonRemoteReplicaOnly,
			Detail: "the only replica is at another site; §31 prefers a local one",
		})
	}

	// Nothing has looked at these bytes.
	//
	// The conservative-looking answer is TRANSCODE, and it is wrong. A node
	// with no probes is precisely a node with no ffprobe (ADR-0023) — and a
	// node with no ffprobe cannot transcode either, so planning TRANSCODE
	// there makes the entire library unplayable. Planning DIRECT makes most of
	// it work, and the failure mode when it does not is a client reporting
	// that it cannot play the file: recoverable, visible, and far better than
	// a library that refuses everything.
	//
	// The reason is attached either way, so a client that wants to be careful
	// can see that the plan is a guess rather than a finding.
	if !media.Known {
		plan.Decision = DecisionDirect
		plan.Reasons = append(plan.Reasons, Reason{
			Code: ReasonNoProbe,
			Detail: "nothing has probed these bytes, so this plan assumes the device can play them; " +
				"a probe would confirm it",
		})
		return plan
	}

	// A device that declares nothing cannot be matched against, so the same
	// argument applies: assume it can play what it asked for, and say so.
	if !device.Declares() {
		plan.Decision = DecisionDirect
		plan.Reasons = append(plan.Reasons, Reason{
			Code:   ReasonDeviceDeclaresNothing,
			Detail: "the device declares no codecs or containers, so nothing can be checked against it",
		})
		return plan
	}

	streamReasons := incompatibleStreams(media, device)
	containerOK := supportsContainer(device.Containers, media.Container)

	switch {
	case len(streamReasons) > 0:
		// At least one stream must be re-encoded. The container is not the
		// deciding factor here — a remux cannot fix a codec the device
		// refuses — but an unsupported container is still recorded, because
		// the transcode will have to write a different one and the operator
		// asking "why" deserves the whole answer.
		plan.Decision = DecisionTranscode
		plan.Reasons = append(plan.Reasons, streamReasons...)
		if !containerOK {
			plan.Reasons = append(plan.Reasons, containerReason(media.Container))
		}
	case !containerOK:
		// Right streams, wrong wrapper.
		plan.Decision = DecisionRemux
		plan.Reasons = append(plan.Reasons, containerReason(media.Container))
	default:
		plan.Decision = DecisionDirect
	}
	return plan
}

// chooseReplica prefers a local replica (§31).
//
// Cross-site streaming is fallback behaviour, not the norm — so a local
// replica wins even when a remote one is listed first, and a remote one is
// used rather than refusing when it is all there is.
func chooseReplica(replicas []Replica) (Replica, bool) {
	var remote Replica
	haveRemote := false
	for _, r := range replicas {
		if r.Local {
			return r, true
		}
		if !haveRemote {
			remote, haveRemote = r, true
		}
	}
	return remote, haveRemote
}

// incompatibleStreams lists every reason a stream cannot be played as it is.
//
// It collects ALL of them rather than returning the first. A device that
// refuses both the video codec and the resolution has two problems, and
// telling the operator about one of them produces a second question.
func incompatibleStreams(media MediaProfile, device DeviceProfile) []Reason {
	var out []Reason

	if media.VideoCodec != "" && len(device.VideoCodecs) > 0 &&
		!contains(device.VideoCodecs, media.VideoCodec) {
		out = append(out, Reason{
			Code:   ReasonVideoCodecUnsupported,
			Detail: fmt.Sprintf("the device does not declare %s video", media.VideoCodec),
		})
	}
	if media.AudioCodec != "" && len(device.AudioCodecs) > 0 &&
		!contains(device.AudioCodecs, media.AudioCodec) {
		out = append(out, Reason{
			Code:   ReasonAudioCodecUnsupported,
			Detail: fmt.Sprintf("the device does not declare %s audio", media.AudioCodec),
		})
	}
	// A zero maximum is "no limit stated", not a limit of zero.
	if device.MaxWidth > 0 && device.MaxHeight > 0 &&
		(int64(media.Width) > device.MaxWidth || int64(media.Height) > device.MaxHeight) {
		out = append(out, Reason{
			Code: ReasonResolutionTooHigh,
			Detail: fmt.Sprintf("%dx%d is past the device's %dx%d",
				media.Width, media.Height, device.MaxWidth, device.MaxHeight),
		})
	}
	if device.MaxBitrateBPS > 0 && media.BitrateBPS > device.MaxBitrateBPS {
		out = append(out, Reason{
			Code: ReasonBitrateTooHigh,
			Detail: fmt.Sprintf("%d bit/s is past the device's %d",
				media.BitrateBPS, device.MaxBitrateBPS),
		})
	}
	if media.HDR && !device.SupportsHDR {
		out = append(out, Reason{
			Code:   ReasonHDRUnsupported,
			Detail: "the device does not declare HDR",
		})
	}
	return out
}

func containerReason(container string) Reason {
	return Reason{
		Code:   ReasonContainerUnsupported,
		Detail: fmt.Sprintf("the device does not declare the %s container", container),
	}
}

// containerAliases maps a demuxer name to the names a device is likely to use
// for the same container.
//
// The two vocabularies genuinely differ, and this is not cosmetic. ffprobe
// reports Matroska as "matroska,webm"; every device on earth calls it "mkv".
// Without this, a television declaring "mkv" would be told to REMUX every
// Matroska file in the library — the exact "why is my TV transcoding
// everything" complaint this planner exists to be able to answer, caused by
// the planner itself.
//
// It is deliberately small and one-directional: demuxer name to the aliases a
// client might declare. A general normalisation of container names is a much
// larger and vaguer job, and every entry here is one somebody can check.
var containerAliases = map[string][]string{
	"matroska": {"mkv"},
	"webm":     {"webm"},
	"mov":      {"mov", "qt"},
	"mp4":      {"mp4", "m4v", "m4a"},
	"mpegts":   {"ts", "m2ts", "mts"},
	"mpeg":     {"mpg", "mpeg"},
	"asf":      {"wmv", "asf"},
	"ogg":      {"ogg", "ogv", "oga"},
	"mp3":      {"mp3"},
	"flac":     {"flac"},
	"wav":      {"wav"},
	"aiff":     {"aiff", "aif"},
}

// supportsContainer matches by membership, because ffprobe's format_name is a
// comma-separated list of every name a demuxer answers to — "mov,mp4,m4a" is
// one file, and a device declaring "mp4" can play it.
//
// Each name is also checked against its aliases, because the demuxer's
// vocabulary and a device's are not the same. See containerAliases.
func supportsContainer(declared []string, container string) bool {
	if len(declared) == 0 {
		return false
	}
	for _, name := range strings.Split(container, ",") {
		name = strings.TrimSpace(strings.ToLower(name))
		if name == "" {
			continue
		}
		if contains(declared, name) {
			return true
		}
		for _, alias := range containerAliases[name] {
			if contains(declared, alias) {
				return true
			}
		}
	}
	return false
}

func contains(haystack []string, needle string) bool {
	needle = strings.ToLower(strings.TrimSpace(needle))
	for _, h := range haystack {
		if strings.ToLower(strings.TrimSpace(h)) == needle {
			return true
		}
	}
	return false
}
