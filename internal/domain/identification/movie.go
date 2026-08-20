package identification

var videoExts = newSet(
	".mkv", ".mp4", ".m4v", ".avi", ".mov", ".wmv", ".mpg", ".mpeg", ".m2ts",
	".ts", ".webm", ".flv", ".divx", ".iso", ".vob", ".ogv", ".3gp", ".rmvb",
)

// isVideoPath reports whether a rule for video content should look at this
// path. Companion extensions are accepted because roleView has already
// rewritten the path to point at the sibling primary file.
func isVideoPath(p Path) bool { return videoExts[p.Ext] || isAuxExt(p.Ext) }

func movieRules() []Rule {
	return []Rule{
		{Name: "movie/title-year-dir", ContentType: Movie, Match: matchMovieDir},
		{Name: "movie/title-year", ContentType: Movie, Match: matchMovieFile},
	}
}

// fallbackRules recognise a bare title with nothing else to go on. They are
// registered after every specific rule, so they only ever see what nothing else
// claimed. Their relative order is what types a companion file in a library
// whose content type was not given.
func fallbackRules() []Rule {
	return []Rule{
		{Name: "movie/title-only", ContentType: Movie, Match: matchMovieTitleOnly},
		{Name: "series/show", ContentType: Series, Match: matchSeriesShow},
	}
}

// matchMovieDir is the Plex/Jellyfin shape: "Movie Title (2019)/anything.mkv".
// The directory is the more reliable of the two names — it is the one a human
// curated — so it wins whenever it carries a year.
func matchMovieDir(p Path) (Candidate, bool) {
	if !isVideoPath(p) || len(p.Dirs) == 0 {
		return Candidate{}, false
	}
	dir := parseName(p.Dir())
	if dir.Empty() || dir.Year == 0 {
		return Candidate{}, false
	}
	return movieCandidate(dir, parseName(p.Stem).Meta), true
}

// matchMovieFile is the scene shape: "Movie.Title.2019.2160p.WEB-DL.x265-GRP".
func matchMovieFile(p Path) (Candidate, bool) {
	if !isVideoPath(p) {
		return Candidate{}, false
	}
	name := parseName(p.Stem)
	if name.Empty() || name.Year == 0 {
		return Candidate{}, false
	}
	return movieCandidate(name, name.Meta), true
}

// matchMovieTitleOnly is the last resort for video: a title with no year at
// all. It still produces a stable key, so a rescan converges.
func matchMovieTitleOnly(p Path) (Candidate, bool) {
	if !isVideoPath(p) {
		return Candidate{}, false
	}
	name := parseName(p.Stem)
	meta := name.Meta
	if name.Empty() {
		return Candidate{}, false
	}
	// A bare "movie.mkv" inside "Movie Title/" means the directory names it.
	if len(p.Dirs) > 0 {
		if dir := parseName(p.Dir()); !dir.Empty() && normKey(dir.Title) != "" {
			if len(name.Title) <= 1 || genericArtworkStems[normKey(name.Title)] {
				return movieCandidate(dir, meta), true
			}
		}
	}
	return movieCandidate(name, meta), true
}

func movieCandidate(name nameParts, meta []string) Candidate {
	key := normKey(name.Title)
	if key == "" {
		return Candidate{}
	}
	q := parseQuality(meta)
	editionKey := q.Key()
	if editionKey == "" {
		editionKey = "default"
	}
	return Candidate{
		ContentType:       Movie,
		WorkKey:           workKey(Movie, key, yearKey(name.Year)),
		Title:             displayTitle(name.Title),
		SortTitle:         key,
		Year:              name.Year,
		WorkAttributes:    map[string]any{},
		EditionAttributes: q.Attributes(),
		EditionKey:        editionKey,
		EditionLabel:      q.Label(),
		EditionType:       q.Source,
	}
}
