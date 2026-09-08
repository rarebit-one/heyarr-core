package identification

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	// S02E05, s02.e05, S02E05E06, S02E05-E06.
	reSxxExx = regexp.MustCompile(`(?i)(^|[^a-z0-9])s(\d{1,3})[ ._-]*e(\d{1,4})((?:[ ._-]*e\d{1,4})+)?`)
	// 2x05. The leading boundary keeps "1920x1080" out.
	reNxNN = regexp.MustCompile(`(?i)(^|[^a-z0-9])(\d{1,2})x(\d{1,3})([^0-9]|$)`)
	// A season directory: "Season 02", "S02", "Staffel 2", "Temporada 2".
	reSeasonDir = regexp.MustCompile(`(?i)^(?:season|series|staffel|saison|stagione|temporada|seizoen|s)[ ._-]*(\d{1,3})(?:[^0-9].*)?$`)
	// A specials directory is season zero, not an extras directory.
	reSpecialsDir = regexp.MustCompile(`(?i)^specials?$|^season[ ._-]*0+$`)
	// A name that carries the series and a season together, as a download
	// folder does: "Show Season 4 Mp4 1080p", "Show (2018) Staffel 2",
	// "Show.S04.Complete.1080p". The title before the season word is what names
	// the series; everything after the number is release noise. The character
	// after the number must not be a letter, so "S04E04" is left to reSxxExx.
	reTitleSeason = regexp.MustCompile(`(?i)^(.+?)[ ._-]+(?:season|series|staffel|saison|stagione|temporada|seizoen|s)[ ._-]*(\d{1,3})(?:[^0-9a-z].*)?$`)
	// "Episode 5", "Ep 5", "E05" at the start of a filename.
	reEpisodeStem = regexp.MustCompile(`(?i)^(?:episode|epis|ep|e)[ ._-]*(\d{1,4})`)
	// A bare leading episode number, only trusted inside a season directory.
	reLeadingNum = regexp.MustCompile(`^(\d{1,3})[ ._-]+`)
	reTrailingE  = regexp.MustCompile(`(?i)e(\d{1,4})`)
)

func seriesRules() []Rule {
	return []Rule{
		{Name: "series/sxxexx", ContentType: Series, Match: matchSeriesSxxExx},
		{Name: "series/nxnn", ContentType: Series, Match: matchSeriesNxNN},
		{Name: "series/season-dir", ContentType: Series, Match: matchSeriesSeasonDir},
		{Name: "series/season-artwork", ContentType: Series, Match: matchSeriesSeasonArtwork},
	}
}

// matchSeriesSeasonArtwork claims a companion file that roleView has already
// resolved to a season directory ("Series/Season 02/season02-poster.jpg"). It
// is safe ahead of the movie rules because no movie directory is called
// "Season 02".
func matchSeriesSeasonArtwork(p Path) (Candidate, bool) {
	if !isAuxExt(p.Ext) || len(p.Dirs) == 0 {
		return Candidate{}, false
	}
	stem := strings.TrimSpace(p.Stem)
	var season int
	switch {
	case reSpecialsDir.MatchString(stem):
		season = 0
	case reSeasonDir.MatchString(stem):
		season = atoi(reSeasonDir.FindStringSubmatch(stem)[1])
	default:
		return Candidate{}, false
	}
	titleSrc, _ := seriesTitleSource(Path{Dirs: p.Dirs}, "")
	return seriesCandidate(titleSrc, season, nil, "", p)
}

// matchSeriesShow is the fallback for a file that names a show and nothing
// else: a show-level poster, or a featurette whose extras directory has already
// been stripped. It claims anything with a usable title, so it is registered
// last and only ever runs ahead of the movie fallback when the library says it
// holds series.
//
// Here the stem names the show and the directories above it are, at most, a
// season directory and grouping folders ("TV/"): a promoted companion file's
// stem is its show folder, and a bare video named after its show is named by
// its stem. So the stem wins and the directories supply only the season —
// the reverse of the episode rules, where the filename repeats the show at
// best and a directory names it.
func matchSeriesShow(p Path) (Candidate, bool) {
	if !isVideoPath(p) {
		return Candidate{}, false
	}
	_, season := seriesTitleSource(p, "")
	titleSrc := strings.TrimSpace(p.Stem)
	if t, s, ok := splitSeasonFolder(titleSrc); ok && t != "" {
		titleSrc = t
		if season < 0 {
			season = s
		}
	}
	if titleSrc == "" {
		return Candidate{}, false
	}
	return seriesCandidate(titleSrc, season, nil, "", p)
}

// matchSeriesSxxExx handles every filename that spells the season and episode
// out: "S02E05", "s02.e05", "S02E05E06" and "S00E01" under Specials.
func matchSeriesSxxExx(p Path) (Candidate, bool) {
	if !isVideoPath(p) {
		return Candidate{}, false
	}
	loc := reSxxExx.FindStringSubmatchIndex(p.Stem)
	if loc == nil {
		return Candidate{}, false
	}
	season, _ := strconv.Atoi(p.Stem[loc[4]:loc[5]])
	episodes := []int{atoi(p.Stem[loc[6]:loc[7]])}
	if loc[8] >= 0 {
		for _, m := range reTrailingE.FindAllStringSubmatch(p.Stem[loc[8]:loc[9]], -1) {
			episodes = append(episodes, atoi(m[1]))
		}
	}
	titleSrc, _ := seriesTitleSource(p, p.Stem[:loc[0]])
	return seriesCandidate(titleSrc, season, episodes, episodeTitle(p.Stem[loc[1]:]), p)
}

// matchSeriesNxNN handles the "2x05" spelling.
func matchSeriesNxNN(p Path) (Candidate, bool) {
	if !isVideoPath(p) {
		return Candidate{}, false
	}
	loc := reNxNN.FindStringSubmatchIndex(p.Stem)
	if loc == nil {
		return Candidate{}, false
	}
	season := atoi(p.Stem[loc[4]:loc[5]])
	episode := atoi(p.Stem[loc[6]:loc[7]])
	titleSrc, _ := seriesTitleSource(p, p.Stem[:loc[0]])
	return seriesCandidate(titleSrc, season, []int{episode}, episodeTitle(p.Stem[loc[7]:]), p)
}

// matchSeriesSeasonDir handles "Series/Season 2/Episode 5 - Title.mkv" and
// "Series/Season 2/05 - Title.mkv", where only the directory knows the season.
func matchSeriesSeasonDir(p Path) (Candidate, bool) {
	if !isVideoPath(p) || len(p.Dirs) == 0 {
		return Candidate{}, false
	}
	titleSrc, dirSeason := seriesTitleSource(p, "")
	if dirSeason < 0 {
		return Candidate{}, false
	}
	m := reEpisodeStem.FindStringSubmatchIndex(p.Stem)
	if m == nil {
		m = reLeadingNum.FindStringSubmatchIndex(p.Stem)
	}
	if m == nil {
		return Candidate{}, false
	}
	episode := atoi(p.Stem[m[2]:m[3]])
	rest := p.Stem[m[1]:]
	return seriesCandidate(titleSrc, dirSeason, []int{episode}, episodeTitle(rest), p)
}

// seriesTitleSource finds the string that names the series, and the season the
// directory layout implies (-1 when there is no season directory).
//
// Directories win over the filename: "Series/Season 02/S02E05.mkv" names the
// series exactly once, in the directory, and the filename does not repeat it.
//
// A directory that names the series and a season in one breath — the shape a
// download client leaves behind, "Show Season 4 Mp4 1080p/Show S04E04.mp4" —
// names the series too: the part before the season word. Reading the whole
// folder name as the title minted a series called "Show Season 4 Mp4" beside
// the real one, and a later move into "Show/Season 04/" could never converge
// with it (ADR-0081).
func seriesTitleSource(p Path, stemPrefix string) (title string, dirSeason int) {
	dirSeason = -1
	for i := len(p.Dirs) - 1; i >= 0; i-- {
		d := strings.TrimSpace(p.Dirs[i])
		switch {
		case reSpecialsDir.MatchString(d):
			if dirSeason < 0 {
				dirSeason = 0
			}
		case reSeasonDir.MatchString(d):
			if dirSeason < 0 {
				dirSeason = atoi(reSeasonDir.FindStringSubmatch(d)[1])
			}
		default:
			t, season, ok := splitSeasonFolder(d)
			if !ok {
				return d, dirSeason
			}
			if dirSeason < 0 {
				dirSeason = season
			}
			if t != "" {
				return t, dirSeason
			}
		}
	}
	title = strings.TrimSpace(stemPrefix)
	if t, season, ok := splitSeasonFolder(title); ok && t != "" {
		if dirSeason < 0 {
			dirSeason = season
		}
		title = t
	}
	return title, dirSeason
}

// splitSeasonFolder reads a name that carries a series title and a season
// together, and returns the title (which may be empty when the name is only a
// season marker) and the season. A single-episode download folder,
// "Show.S04E04.1080p.WEB", is the same shape with the episode spelled out.
func splitSeasonFolder(name string) (title string, season int, ok bool) {
	if loc := reSxxExx.FindStringSubmatchIndex(name); loc != nil {
		return strings.TrimSpace(name[:loc[0]]), atoi(name[loc[4]:loc[5]]), true
	}
	if m := reTitleSeason.FindStringSubmatch(name); m != nil {
		return strings.TrimSpace(m[1]), atoi(m[2]), true
	}
	return "", -1, false
}

// episodeTitle cleans the remainder of a filename after the episode marker.
//
// Unlike a work title, an episode title has no protected first token: the
// remainder of a scene filename usually *starts* with the release metadata
// ("Show.S03E07.PROPER.1080p"), and "PROPER" is not the name of an episode.
func episodeTitle(rest string) string {
	toks := tokenize(rest)
	cut := len(toks)
	for i, t := range toks {
		if isNoiseToken(strings.ToLower(t)) {
			cut = i
			break
		}
		if _, isYear := parseYear(t); isYear {
			cut = i
			break
		}
	}
	if cut == 0 {
		return ""
	}
	return displayTitle(toks[:cut])
}

func seriesCandidate(titleSrc string, season int, episodes []int, epTitle string, p Path) (Candidate, bool) {
	name := parseName(titleSrc)
	key := normKey(name.Title)
	if key == "" {
		return Candidate{}, false
	}

	edition := map[string]any{}
	if season >= 0 {
		edition["season"] = season
	}
	if len(episodes) > 0 {
		edition["episode"] = episodes[0]
	}
	if len(episodes) > 1 {
		edition["episodes"] = episodes
	}
	if epTitle != "" {
		edition["episode_title"] = epTitle
	}

	editionKey, editionLabel := seasonEdition(season)
	q := parseQuality(parseName(p.Stem).Meta)

	return Candidate{
		ContentType:       Series,
		WorkKey:           workKey(Series, key, yearKey(name.Year)),
		Title:             displayTitle(name.Title),
		SortTitle:         key,
		Year:              name.Year,
		WorkAttributes:    map[string]any{},
		EditionAttributes: edition,
		EditionKey:        editionKey,
		EditionLabel:      editionLabel,
		EditionType:       q.Source,
	}, true
}

// seasonEdition maps a season number onto the Edition it groups into. The
// Edition of an episodic Work is its season: that is the unit a human adds,
// replaces and deletes.
func seasonEdition(season int) (key, label string) {
	switch {
	case season < 0:
		return "default", ""
	case season == 0:
		return "season-00", "Specials"
	default:
		return fmt.Sprintf("season-%02d", season), fmt.Sprintf("Season %02d", season)
	}
}

func atoi(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}
