package identification

import "strings"

// Release-name quality parsing, exposed for the indexer layer (ADR-0091).
//
// The scanner reads quality (resolution, source, codec, dynamic range) off an
// on-disk filename to label an edition; ADR-0091 lets an indexer read the same
// tokens off a release title, opt-in, when the indexer asserts none. Both go
// through parseQuality here, so an on-disk copy and a search result converge on
// one reading of the same string and a fix to the token table fixes both.
//
// This file exposes the parser WITHOUT importing internal/domain/policy: it
// returns the recognised tokens as plain strings, and the indexer package maps
// them into policy's typed vocabulary (where its torznab mapping already lives).
// Keeping identification policy-free is deliberate — the scanner does not depend
// on the grading vocabulary, and this entry point must not make it.

// ReleaseQuality is what a release name asserts about itself. An empty field is
// a token the name did not carry, never a default.
type ReleaseQuality struct {
	// Resolution is the vertical-line class as the name spells it: "2160p",
	// "1080p", "720p", …, or "" when the name says nothing.
	Resolution string
	// Source is where the release came from: "remux", "bluray", "web-dl",
	// "webrip", "hdtv", "dvd", or "".
	Source string
	// Codec is the video codec as the name spells it: "x265", "x264", "av1",
	// "vp9", "xvid", "divx", or "". The indexer mapping folds x264/x265 onto
	// policy's h264/hevc class.
	Codec string
	// DynamicRange is "HDR", "DV", "DV HDR", or "".
	DynamicRange string
}

// Empty reports whether the name carried no recognised quality token at all.
func (q ReleaseQuality) Empty() bool {
	return q.Resolution == "" && q.Source == "" && q.Codec == "" && q.DynamicRange == ""
}

// ParseReleaseQuality reads the quality tokens a release title asserts about
// itself. It is the scanner's filename parser (tokenize + parseQuality) applied
// to an indexer's release title — the same code that decides an on-disk file is
// "2160p HDR web-dl". It reads only what the name says; a token it does not
// recognise leaves the corresponding field empty (never guessed).
func ParseReleaseQuality(title string) ReleaseQuality {
	// parseQuality matches against a lowercase token table, and the scanner
	// lowercases its metadata run before calling it (parseName's Meta). A
	// release title arrives in mixed case ("WEB-DL", "BluRay", "HDR"), so the
	// tokens are lowercased here for the same reason and by the same rule.
	toks := tokenize(title)
	for i := range toks {
		toks[i] = strings.ToLower(toks[i])
	}
	q := parseQuality(toks)
	return ReleaseQuality{
		Resolution:   q.Resolution,
		Source:       q.Source,
		Codec:        q.Codec,
		DynamicRange: q.Dynamic,
	}
}
