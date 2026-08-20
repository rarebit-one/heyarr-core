package identification

import "strings"

// noiseTokens are the tokens that mark the start of the release-metadata run in
// a filename. Everything from the first one of these onwards describes the
// release, never the work.
//
// The list is deliberately technical. A word that could plausibly open a title
// is never consulted anyway (the scan starts at index 1), and a word that
// truncates a title mid-way truncates it identically in every spelling of that
// filename, so convergence survives even where accuracy does not.
var noiseTokens = newSet(
	// resolution
	"480p", "540p", "576p", "576i", "720p", "1080p", "1080i", "1440p", "2160p",
	"4320p", "4k", "8k", "uhd", "fhd", "hd", "sd",
	// source
	"bluray", "blura", "blu", "bdrip", "brrip", "bd", "bdremux", "remux",
	"webrip", "webdl", "web", "dl", "hdtv", "pdtv", "dsr", "dvdrip", "dvdscr",
	"dvd", "dvd5", "dvd9", "hddvd", "hdrip", "vhsrip", "vhs", "cam", "camrip",
	"telesync", "telecine", "screener", "scr", "r5", "workprint", "vodrip",
	"amzn", "nf", "dsnp", "hmax", "atvp", "hulu", "pcok", "stan", "crav", "itunes",
	// codec
	"x264", "x265", "h264", "h265", "avc", "hevc", "xvid", "divx", "av1", "vp9",
	"10bit", "8bit", "12bit", "hi10p", "10bits",
	// audio
	"aac", "aac2", "aac5", "ac3", "eac3", "dd", "dd2", "dd5", "ddp", "ddp2",
	"ddp5", "ddp7", "dts", "dtshd", "dtsx", "truehd", "atmos", "lpcm", "pcm",
	"mp3", "flac", "opus", "2ch", "6ch", "8ch", "dual", "multi",
	// dynamic range
	"hdr", "hdr10", "hdr10plus", "dv", "dovi", "dolbyvision", "hlg", "sdr", "imax",
	// release flags
	"proper", "repack", "rerip", "extended", "unrated", "uncut", "uncensored",
	"theatrical", "directors", "remastered", "restored", "criterion", "limited",
	"internal", "subbed", "dubbed", "subs", "readnfo", "nfo", "3d", "sbs", "hsbs",
	"hou", "60fps", "hybrid", "oar",
)

func isNoiseToken(t string) bool { return noiseTokens[t] }

// quality is the release description parsed out of the metadata run.
type quality struct {
	Resolution string // "2160p"
	Source     string // "remux", "web-dl", "bluray", "webrip", "hdtv", "dvd"
	Dynamic    string // "HDR", "DV", "DV HDR"
	Codec      string // "x265"
}

// Empty reports whether nothing at all was recognised.
func (q quality) Empty() bool {
	return q.Resolution == "" && q.Source == "" && q.Dynamic == "" && q.Codec == ""
}

// Key is the stable part of an edition key: same bytes for the same release
// shape, regardless of the order the tokens appeared in.
func (q quality) Key() string {
	parts := make([]string, 0, 4)
	for _, p := range []string{q.Resolution, q.Source, strings.ToLower(q.Dynamic), q.Codec} {
		if p != "" {
			parts = append(parts, strings.ReplaceAll(p, " ", "-"))
		}
	}
	return strings.Join(parts, "-")
}

// Label is the human edition label, e.g. "2160p HDR".
func (q quality) Label() string {
	parts := make([]string, 0, 2)
	if q.Resolution != "" {
		parts = append(parts, q.Resolution)
	}
	if q.Dynamic != "" {
		parts = append(parts, q.Dynamic)
	}
	if len(parts) == 0 && q.Source != "" {
		return strings.ToUpper(q.Source)
	}
	return strings.Join(parts, " ")
}

// Attributes renders the recognised tokens for editions.attributes.
func (q quality) Attributes() map[string]any {
	attrs := map[string]any{}
	if q.Resolution != "" {
		attrs["resolution"] = q.Resolution
	}
	if q.Source != "" {
		attrs["source"] = q.Source
	}
	if q.Dynamic != "" {
		attrs["dynamic_range"] = q.Dynamic
	}
	if q.Codec != "" {
		attrs["codec"] = q.Codec
	}
	return attrs
}

var resolutionOrder = []string{
	"4320p", "2160p", "1440p", "1080p", "1080i", "720p", "576p", "576i", "540p", "480p",
}

func parseQuality(meta []string) quality {
	m := newSet(meta...)
	var q quality

	for _, r := range resolutionOrder {
		if m[r] {
			q.Resolution = r
			break
		}
	}
	if q.Resolution == "" && (m["4k"] || m["uhd"]) {
		q.Resolution = "2160p"
	}

	switch {
	case m["remux"] || m["bdremux"]:
		q.Source = "remux"
	case m["webrip"]:
		q.Source = "webrip"
	case m["webdl"], m["web"] && m["dl"], m["web"]:
		q.Source = "web-dl"
	case m["bluray"] || m["blu"] || m["bdrip"] || m["brrip"] || m["bd"]:
		q.Source = "bluray"
	case m["hdtv"] || m["pdtv"]:
		q.Source = "hdtv"
	case m["dvdrip"] || m["dvd"] || m["dvd5"] || m["dvd9"]:
		q.Source = "dvd"
	case m["hdrip"]:
		q.Source = "hdrip"
	}

	dyn := make([]string, 0, 2)
	if m["dv"] || m["dovi"] || m["dolbyvision"] {
		dyn = append(dyn, "DV")
	}
	if m["hdr"] || m["hdr10"] || m["hdr10plus"] {
		dyn = append(dyn, "HDR")
	}
	q.Dynamic = strings.Join(dyn, " ")

	switch {
	case m["x265"] || m["h265"] || m["hevc"]:
		q.Codec = "x265"
	case m["x264"] || m["h264"] || m["avc"]:
		q.Codec = "x264"
	case m["av1"]:
		q.Codec = "av1"
	case m["xvid"]:
		q.Codec = "xvid"
	case m["divx"]:
		q.Codec = "divx"
	case m["vp9"]:
		q.Codec = "vp9"
	}

	return q
}
