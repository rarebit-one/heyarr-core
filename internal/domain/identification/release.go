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

// ReleaseSeason is the season and episode a release title spells out (ADR-0093).
//
// It is the scanner's SxxEyy reading — series.go's reSxxExx / reNxNN /
// reTitleSeason — applied to an indexer's release title, so an on-disk episode
// and a search result converge on one reading of the same string and a fix to
// the season table fixes both. An episode want's containment gate reads this.
type ReleaseSeason struct {
	// Determined reports whether a season could be read from the name at all.
	// False means the name carried no season marker — never season zero, which
	// is Specials and is a real, distinct answer.
	Determined bool
	// Season is the season the name spells out, meaningful only when Determined.
	Season int
	// Episodes are the episode numbers the name spells out, in the order they
	// appear. Empty with Determined true is a whole-season pack ("Show S03");
	// non-empty is a specific episode or a multi-episode file ("Show S03E01",
	// "Show S03E01E02").
	Episodes []int
}

// Pack reports whether the name is a whole-season pack: a season was read but
// no episode.
func (r ReleaseSeason) Pack() bool { return r.Determined && len(r.Episodes) == 0 }

// ContainsEpisode reports whether a release with this reading could contain the
// given season/episode: it must be the SAME season, and then either the exact
// episode (a multi-episode file counts when the episode is in its range) or a
// whole-season pack for that season. A reading whose season could not be
// determined contains nothing — the safe direction for an episode want.
func (r ReleaseSeason) ContainsEpisode(season, episode int) bool {
	if !r.Determined || r.Season != season {
		return false
	}
	if len(r.Episodes) == 0 {
		// A pack for the wanted season contains every episode of it.
		return true
	}
	for _, e := range r.Episodes {
		if e == episode {
			return true
		}
	}
	return false
}

// ParseReleaseSeasonEpisode reads the season and episode an indexer's release
// title spells out. It is the scanner's series parser — the same regexes that
// decide an on-disk file is "S02E05" or a "Season 02" pack — applied to a
// release name (ADR-0093), and it reads only what the name says: a title with
// no season marker returns Determined:false rather than a guessed zero.
func ParseReleaseSeasonEpisode(title string) ReleaseSeason {
	// SxxEyy (and SxxEyyEzz) is the most specific spelling, so it is tried
	// first — exactly as matchSeriesSxxExx does before the season-dir rules.
	if loc := reSxxExx.FindStringSubmatchIndex(title); loc != nil {
		episodes := []int{atoi(title[loc[6]:loc[7]])}
		if loc[8] >= 0 {
			for _, m := range reTrailingE.FindAllStringSubmatch(title[loc[8]:loc[9]], -1) {
				episodes = append(episodes, atoi(m[1]))
			}
		}
		return ReleaseSeason{Determined: true, Season: atoi(title[loc[4]:loc[5]]), Episodes: episodes}
	}
	// The "2x05" spelling.
	if loc := reNxNN.FindStringSubmatchIndex(title); loc != nil {
		return ReleaseSeason{
			Determined: true, Season: atoi(title[loc[4]:loc[5]]),
			Episodes: []int{atoi(title[loc[6]:loc[7]])},
		}
	}
	// A whole-season name: "Show S03", "Show.S03.Complete.1080p", "Show Season
	// 3". splitSeasonFolder rechecks reSxxExx first, so it only reaches the
	// season-word form here, which is a pack (no episode).
	if _, season, ok := splitSeasonFolder(title); ok {
		return ReleaseSeason{Determined: true, Season: season}
	}
	return ReleaseSeason{}
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
