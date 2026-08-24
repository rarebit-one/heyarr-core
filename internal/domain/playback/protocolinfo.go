package playback

import "strings"

// Deriving a DeviceProfile from what a UPnP renderer says it can sink (§68).
//
// # Why this exists
//
// internal/api/resources/devices.go states the assumption this file removes:
// "there is no way to interrogate a television. So a client declares what it
// can play and Heyarr believes it." That is true of a television in general
// and false of a UPnP MediaRenderer specifically, which answers
// ConnectionManager::GetProtocolInfo with the list of formats it will accept.
//
// So for one class of device the declaration can be READ rather than typed in.
// A hand-written profile is a guess that ages badly — someone writes "hevc"
// because the box was sold as 4K, and the planner then reports
// video_codec_unsupported for the rest of the device's life. A profile derived
// from the device's own answer is wrong only when the device lies about
// itself, and it is re-derivable on every discovery.
//
// # This package cannot fetch anything
//
// Nothing here speaks SOAP, HTTP or SSDP: the domain must not import net or
// os (ADR-0006/0007). internal/renderer does the fetching and hands the raw
// Sink string here, which is also what makes the interesting part — the
// vocabulary mapping — table-testable without a television in the room.

// ProfileFromProtocolInfo derives a DeviceProfile from a renderer's Sink
// protocolInfo (the CSV that GetProtocolInfo returns).
//
// A protocolInfo entry is four colon-separated fields:
//
//	http-get:*:video/mp4:DLNA.ORG_PN=AVC_MP4_MP_HD_AAC_MULT5;DLNA.ORG_OP=01
//	└ protocol └ network └ content format  └ additional info
//
// Only `http-get` is considered. Heyarr serves blobs over HTTP and nothing
// else (ADR-0013), so a renderer's RTP or RTSP support describes a
// conversation this system will never have.
func ProfileFromProtocolInfo(sink string) DeviceProfile {
	var containers, video, audio []string

	for _, entry := range strings.Split(sink, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		fields := strings.SplitN(entry, ":", 4)
		if len(fields) < 3 || !strings.EqualFold(fields[0], "http-get") {
			continue
		}
		mime := strings.ToLower(strings.TrimSpace(fields[2]))
		if m, ok := mimeVocabulary[mime]; ok {
			containers = appendMissing(containers, m.containers...)
			video = appendMissing(video, m.video...)
			audio = appendMissing(audio, m.audio...)
		}
		if len(fields) == 4 {
			v, a := codecsFromProfileName(fields[3])
			video = appendMissing(video, v...)
			audio = appendMissing(audio, a...)
		}
	}

	// Resolution, bitrate and HDR are deliberately left at their zero values,
	// which the planner reads as "no limit stated" rather than "zero".
	//
	// protocolInfo does not carry any of the three. A DLNA profile name hints
	// at a level — AVC_MP4_MP_HD_AAC is 1080-ish, AVC_TS_HP_HD_AAC is not the
	// same claim — but the mapping from profile name to a pixel count is not
	// something this file could get right, and a wrong ceiling here is worse
	// than no ceiling: it makes the planner refuse content the device would
	// have played. A device that wants a limit enforced can still declare one
	// through POST /devices; discovery does not overwrite what it cannot see.
	return DeviceProfile{Containers: containers, VideoCodecs: video, AudioCodecs: audio}
}

// mimeEntry is what one content-format string tells us.
type mimeEntry struct {
	containers []string
	video      []string
	audio      []string
}

// mimeVocabulary maps a renderer's content-format strings to Heyarr's.
//
// The container names on the right are in the DEVICE vocabulary — "mkv", not
// "matroska" — because that is the side of containerAliases a DeviceProfile
// sits on. Getting this backwards would make every Matroska file plan as a
// REMUX, which is the specific complaint the planner exists to avoid.
//
// # A finding worth keeping: the strict entries are the misleading ones
//
// A 2022 Samsung QN85B answers with 292 entries. The ~270 that carry a
// DLNA.ORG_PN describe an SD-era MPEG-2/AVC-baseline device: the richest
// video profile among them is AVC_MP4_MP_HD. Its actual modern capability is
// in a short trailing block of bare wildcard entries — `video/x-mkv:*`,
// `video/hevc:*`, `video/webm:*` — which carry no profile name at all.
//
// So a reader that trusts DLNA.ORG_PN and ignores unprofiled entries, which
// is what the specification would suggest, concludes that a 4K HEVC
// television cannot play HEVC or Matroska. That is why MIME is the primary
// signal here and the profile name is only additive.
//
// Image and caption formats are absent on purpose. They are real entries in
// the list, and Heyarr's planner has nothing to say about them.
var mimeVocabulary = map[string]mimeEntry{
	// Video containers.
	"video/mp4":               {containers: []string{"mp4"}},
	"video/x-m4v":             {containers: []string{"m4v"}},
	"video/quicktime":         {containers: []string{"mov"}},
	"video/x-mkv":             {containers: []string{"mkv"}},
	"video/x-matroska":        {containers: []string{"mkv"}},
	"video/webm":              {containers: []string{"webm"}},
	"video/avi":               {containers: []string{"avi"}},
	"video/x-avi":             {containers: []string{"avi"}},
	"video/x-msvideo":         {containers: []string{"avi"}},
	"video/x-ms-asf":          {containers: []string{"asf"}},
	"video/x-ms-wmv":          {containers: []string{"wmv"}, video: []string{"vc1"}},
	"video/x-flv":             {containers: []string{"flv"}},
	"video/3gpp":              {containers: []string{"3gp"}},
	"video/mpeg":              {containers: []string{"mpeg", "ts"}, video: []string{"mpeg2video"}},
	"video/mpeg2":             {containers: []string{"mpeg"}, video: []string{"mpeg2video"}},
	"video/vnd.dlna.mpeg-tts": {containers: []string{"ts"}},
	"video/x-divx":            {containers: []string{"avi"}, video: []string{"mpeg4"}},

	// A codec announced as a content format. Samsung does this for HEVC and it
	// is the only place the television admits to supporting it.
	"video/hevc": {video: []string{"hevc"}},

	// Audio containers, and the codec each implies.
	"audio/mpeg":             {containers: []string{"mp3"}, audio: []string{"mp3"}},
	"audio/x-mpeg":           {containers: []string{"mp3"}, audio: []string{"mp3"}},
	"audio/mpeg3":            {containers: []string{"mp3"}, audio: []string{"mp3"}},
	"audio/x-mpeg3":          {containers: []string{"mp3"}, audio: []string{"mp3"}},
	"audio/mp4":              {containers: []string{"m4a", "mp4"}, audio: []string{"aac"}},
	"audio/x-m4a":            {containers: []string{"m4a"}, audio: []string{"aac"}},
	"audio/aac":              {audio: []string{"aac"}},
	"audio/x-aac":            {audio: []string{"aac"}},
	"audio/vnd.dlna.adts":    {audio: []string{"aac"}},
	"audio/flac":             {containers: []string{"flac"}, audio: []string{"flac"}},
	"audio/x-flac":           {containers: []string{"flac"}, audio: []string{"flac"}},
	"audio/ogg":              {containers: []string{"ogg"}, audio: []string{"vorbis"}},
	"audio/x-vorbis+ogg":     {containers: []string{"ogg"}, audio: []string{"vorbis"}},
	"audio/opus":             {containers: []string{"ogg"}, audio: []string{"opus"}},
	"audio/wav":              {containers: []string{"wav"}, audio: []string{"pcm_s16le"}},
	"audio/x-wav":            {containers: []string{"wav"}, audio: []string{"pcm_s16le"}},
	"audio/vnd.wave":         {containers: []string{"wav"}, audio: []string{"pcm_s16le"}},
	"audio/aiff":             {containers: []string{"aiff"}, audio: []string{"pcm_s16be"}},
	"audio/x-aiff":           {containers: []string{"aiff"}, audio: []string{"pcm_s16be"}},
	"audio/x-ms-wma":         {containers: []string{"asf"}, audio: []string{"wmav2"}},
	"audio/x-ac3":            {audio: []string{"ac3"}},
	"audio/vnd.dolby.dd-raw": {audio: []string{"ac3"}},
	"audio/x-monkeys-audio":  {containers: []string{"ape"}, audio: []string{"ape"}},
	"audio/3gpp":             {containers: []string{"3gp"}},
}

// codecsFromProfileName reads the codec out of a DLNA.ORG_PN value.
//
// Profile names are structured — AVC_TS_MP_SD_AAC_MULT5 is AVC video in an
// MPEG-TS container with AAC audio — but only loosely, and there are hundreds
// of them. This reads the two facts the planner uses and ignores the rest:
// which video codec and which audio codec. Container comes from the MIME type,
// which is unambiguous where the profile name is not.
//
// Matching is by prefix and by segment rather than by an exhaustive table,
// because an exhaustive table of DLNA profile names would be a thousand lines
// that nobody could verify and that a new device would fall straight through.
func codecsFromProfileName(additional string) (video, audio []string) {
	pn := ""
	for _, part := range strings.Split(additional, ";") {
		if name, ok := strings.CutPrefix(strings.TrimSpace(part), "DLNA.ORG_PN="); ok {
			pn = strings.ToUpper(strings.TrimSpace(name))
			break
		}
	}
	if pn == "" {
		return nil, nil
	}

	switch {
	case strings.HasPrefix(pn, "AVC_"):
		video = []string{"h264"}
	case strings.HasPrefix(pn, "HEVC"), strings.HasPrefix(pn, "HEV1"):
		video = []string{"hevc"}
	case strings.HasPrefix(pn, "MPEG4_P2"):
		video = []string{"mpeg4"}
	case strings.HasPrefix(pn, "MPEG1"):
		video = []string{"mpeg1video"}
	case strings.HasPrefix(pn, "MPEG_"):
		video = []string{"mpeg2video"}
	case strings.HasPrefix(pn, "WMV"), strings.HasPrefix(pn, "VC1"):
		video = []string{"vc1"}
	case strings.HasPrefix(pn, "VP8"):
		video = []string{"vp8"}
	case strings.HasPrefix(pn, "VP9"):
		video = []string{"vp9"}
	}

	// Audio appears as a segment of the name rather than a prefix, because in
	// a video profile it is the trailing half: AVC_MP4_MP_HD_AAC_MULT5.
	for segment, codec := range profileAudioSegments {
		if pn == segment || strings.Contains(pn, "_"+segment) ||
			strings.HasPrefix(pn, segment+"_") {
			audio = appendMissing(audio, codec)
		}
	}
	return video, audio
}

// profileAudioSegments maps a segment of a DLNA profile name to a codec.
//
// MPEG1_L3 is MP3 — layer 3 of MPEG-1 audio — and reads like a video codec if
// you are skimming. HEAAC and BSAC are both decoded by an AAC decoder, so both
// map to aac rather than inventing names ffprobe never reports.
var profileAudioSegments = map[string]string{
	"AAC":      "aac",
	"HEAAC":    "aac",
	"BSAC":     "aac",
	"AC3":      "ac3",
	"XAC3":     "ac3",
	"EAC3":     "eac3",
	"MP3":      "mp3",
	"MP3X":     "mp3",
	"MPEG1_L3": "mp3",
	"LPCM":     "pcm_s16le",
	"WMA":      "wmav2",
	"AMR":      "amr_nb",
	"AMR_WB":   "amr_wb",
	"FLAC":     "flac",
	"VORBIS":   "vorbis",
	"OPUS":     "opus",
	"ATRAC3":   "atrac3",
}

// appendMissing adds values not already present, preserving the order in which
// they were first seen. Discovery reads hundreds of entries that repeat the
// same handful of codecs, and a profile listing "h264" 200 times is one no
// operator can read.
func appendMissing(dst []string, values ...string) []string {
	for _, v := range values {
		if v == "" || contains(dst, v) {
			continue
		}
		dst = append(dst, v)
	}
	return dst
}
