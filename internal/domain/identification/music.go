package identification

import (
	"regexp"
	"strings"
)

var (
	losslessExts = newSet(".flac", ".alac", ".wav", ".aiff", ".aif", ".ape", ".wv", ".dsf", ".dff", ".tta")
	lossyExts    = newSet(".mp3", ".m4a", ".aac", ".ogg", ".oga", ".opus", ".wma", ".mpc", ".spx")
)

var (
	// "CD1", "Disc 2", "Disk 03".
	reDiscDir = regexp.MustCompile(`(?i)^(?:cd|disc|disk|vol|volume)[ ._-]*(\d{1,2})$`)
	// "2001 - Album Name" and "[2001] Album Name".
	reYearFirst = regexp.MustCompile(`^[\[(]?((?:18|19|20)\d{2})[\])]?\s*[-–.]?\s+(.+)$`)
	// "1-03 Track", "1.03 Track".
	reDiscTrack = regexp.MustCompile(`^(\d{1,2})[-.](\d{1,3})[ ._-]+(.*)$`)
	// "03 - Track", "03. Track", "03 Track".
	reTrackNum = regexp.MustCompile(`^(\d{1,3})[ ._-]+(.*)$`)
)

func isAudioPath(p Path) bool { return losslessExts[p.Ext] || lossyExts[p.Ext] || isAuxExt(p.Ext) }

func musicRules() []Rule {
	return []Rule{
		{Name: "music/companion", ContentType: Music, Match: matchMusicCompanion},
		{Name: "music/artist-dir-album-dir", ContentType: Music, Match: matchMusicArtistAlbumDir},
		{Name: "music/artist-album-dir", ContentType: Music, Match: matchMusicCombinedDir},
		{Name: "music/album-dir", ContentType: Music, Match: matchMusicAlbumDir},
		{Name: "music/flat", ContentType: Music, Match: matchMusicFlat},
	}
}

// musicDirs strips a trailing disc directory, returning the remaining
// directories and the disc number (0 when there is none).
func musicDirs(p Path) (dirs []string, disc int) {
	dirs = p.Dirs
	if len(dirs) > 0 {
		if m := reDiscDir.FindStringSubmatch(strings.TrimSpace(dirs[len(dirs)-1])); m != nil {
			disc = atoi(m[1])
			dirs = dirs[:len(dirs)-1]
		}
	}
	return dirs, disc
}

// matchMusicCompanion claims a companion file — "Artist/Album (2001)/cover.jpg"
// — which roleView has already resolved to the album directory. It must run
// before the album rules, because by then the album name is the stem and not a
// directory at all.
func matchMusicCompanion(p Path) (Candidate, bool) {
	if !isAuxExt(p.Ext) {
		return Candidate{}, false
	}
	album, year := albumFromDir(p.Stem)
	if album.Empty() {
		return Candidate{}, false
	}
	var artist nameParts
	dirs, disc := musicDirs(p)
	if len(dirs) > 0 {
		artist = parseName(dirs[len(dirs)-1])
	} else if left, right, ok := splitPair(p.Stem); ok {
		artist = parseName(left)
		album, year = albumFromDir(right)
	}
	return musicCandidate(artist, album, year, disc, "", p.Ext), true
}

// matchMusicArtistAlbumDir is the Beets/Picard shape:
// "Artist/Album (2001)/03 - Track.flac", including "Artist/2001 - Album/…".
func matchMusicArtistAlbumDir(p Path) (Candidate, bool) {
	if !isAudioPath(p) {
		return Candidate{}, false
	}
	dirs, disc := musicDirs(p)
	if len(dirs) < 2 {
		return Candidate{}, false
	}
	album, year := albumFromDir(dirs[len(dirs)-1])
	artist := parseName(dirs[len(dirs)-2])
	if album.Empty() || artist.Empty() {
		return Candidate{}, false
	}
	return musicCandidate(artist, album, year, disc, p.Stem, p.Ext), true
}

// matchMusicCombinedDir is the "Artist - Album/01 Track.mp3" shape.
func matchMusicCombinedDir(p Path) (Candidate, bool) {
	if !isAudioPath(p) {
		return Candidate{}, false
	}
	dirs, disc := musicDirs(p)
	if len(dirs) == 0 {
		return Candidate{}, false
	}
	left, right, ok := splitPair(dirs[len(dirs)-1])
	if !ok {
		return Candidate{}, false
	}
	album, year := albumFromDir(right)
	artist := parseName(left)
	if album.Empty() || artist.Empty() {
		return Candidate{}, false
	}
	return musicCandidate(artist, album, year, disc, p.Stem, p.Ext), true
}

// matchMusicAlbumDir is an album directory with no artist above it.
func matchMusicAlbumDir(p Path) (Candidate, bool) {
	if !isAudioPath(p) {
		return Candidate{}, false
	}
	dirs, disc := musicDirs(p)
	if len(dirs) == 0 {
		return Candidate{}, false
	}
	album, year := albumFromDir(dirs[len(dirs)-1])
	if album.Empty() {
		return Candidate{}, false
	}
	return musicCandidate(nameParts{}, album, year, disc, p.Stem, p.Ext), true
}

// matchMusicFlat is a loose file: "Artist - Album - 01 - Track.mp3", or
// "Artist - Track.mp3", which is a single and so is its own Work.
func matchMusicFlat(p Path) (Candidate, bool) {
	if !losslessExts[p.Ext] && !lossyExts[p.Ext] {
		return Candidate{}, false
	}
	parts := splitAll(p.Stem)
	var artist, album nameParts
	year := 0
	trackSrc := p.Stem
	switch len(parts) {
	case 0:
		return Candidate{}, false
	case 1:
		album, year = albumFromDir(parts[0])
	case 2:
		// "Artist - Track": a loose single is its own Work.
		artist = parseName(parts[0])
		album, year = albumFromDir(parts[1])
		trackSrc = parts[1]
	default:
		artist = parseName(parts[0])
		album, year = albumFromDir(parts[1])
		trackSrc = strings.Join(parts[2:], " - ")
	}
	if album.Empty() {
		return Candidate{}, false
	}
	return musicCandidate(artist, album, year, 0, trackSrc, p.Ext), true
}

// albumFromDir parses an album directory name, accepting both "Album (2001)"
// and "2001 - Album".
func albumFromDir(s string) (nameParts, int) {
	s = strings.TrimSpace(s)
	if m := reYearFirst.FindStringSubmatch(s); m != nil {
		album := parseName(m[2])
		return album, atoi(m[1])
	}
	album := parseName(s)
	return album, album.Year
}

// splitPair splits "Artist - Album" on the first " - " separator.
func splitPair(s string) (left, right string, ok bool) {
	for _, sep := range []string{" - ", " – ", " — ", "_-_"} {
		if i := strings.Index(s, sep); i > 0 && i+len(sep) < len(s) {
			return s[:i], s[i+len(sep):], true
		}
	}
	return "", "", false
}

func splitAll(s string) []string {
	parts := strings.Split(s, " - ")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// trackNumber peels a leading track (and optional disc) number off a filename.
func trackNumber(stem string) (track int, rest string) {
	if m := reDiscTrack.FindStringSubmatch(stem); m != nil {
		return atoi(m[2]), m[3]
	}
	if m := reTrackNum.FindStringSubmatch(stem); m != nil {
		return atoi(m[1]), m[2]
	}
	return 0, stem
}

// discNumber reads the disc from a "1-03 Track" filename.
func discNumber(stem string) int {
	if m := reDiscTrack.FindStringSubmatch(stem); m != nil {
		return atoi(m[1])
	}
	return 0
}

func musicCandidate(artist, album nameParts, year, disc int, trackSrc, ext string) Candidate {
	albumKey := normKey(album.Title)
	if albumKey == "" {
		return Candidate{}
	}
	artistKey := normKey(artist.Title)

	work := map[string]any{}
	if artistKey != "" {
		work["artist"] = displayTitle(artist.Title)
	}

	fileDisc, track, trackName := trackFrom(trackSrc, artist, album)
	if disc == 0 {
		disc = fileDisc
	}
	edition := map[string]any{}
	if disc > 0 {
		edition["disc"] = disc
	}
	if track > 0 {
		edition["track"] = track
	}
	if trackName != "" {
		edition["track_title"] = trackName
	}

	editionKey, editionLabel, editionType := audioEdition(ext)

	return Candidate{
		ContentType:       Music,
		WorkKey:           workKey(Music, artistKey, albumKey, yearKey(year)),
		Title:             displayTitle(album.Title),
		SortTitle:         albumKey,
		Year:              year,
		WorkAttributes:    work,
		EditionAttributes: edition,
		EditionKey:        editionKey,
		EditionLabel:      editionLabel,
		EditionType:       editionType,
	}
}

// trackFrom reads the disc, track and track title out of a filename, first
// peeling off any artist or album name the tagger repeated into it:
// "Artist - Album - 03 - Track" is a track number of 3, not a title.
func trackFrom(src string, artist, album nameParts) (disc, track int, title string) {
	src = stripRepeatedPrefix(src, artist, album)
	disc = discNumber(src)
	track, rest := trackNumber(src)
	rest = stripRepeatedPrefix(rest, artist, album)
	name := parseName(rest)
	if name.Empty() {
		return disc, track, ""
	}
	return disc, track, displayTitle(name.Title)
}

// stripRepeatedPrefix removes leading " - "-separated components that merely
// repeat the artist or the album.
func stripRepeatedPrefix(src string, artist, album nameParts) string {
	keys := []string{normKey(artist.Title), normKey(album.Title)}
	for range 2 {
		left, right, ok := splitPair(src)
		if !ok {
			return src
		}
		matched := false
		for _, k := range keys {
			if k != "" && normalizeName(left) == k {
				src = right
				matched = true
				break
			}
		}
		if !matched {
			return src
		}
	}
	return src
}

// audioEdition maps a container onto the Edition it implies. Lossless and lossy
// copies of the same album are different editions of the same Work.
func audioEdition(ext string) (key, label, editionType string) {
	format := strings.TrimPrefix(ext, ".")
	if format == "" {
		return "default", "", ""
	}
	if losslessExts[ext] {
		return format, strings.ToUpper(format), "lossless"
	}
	if lossyExts[ext] {
		return format, strings.ToUpper(format), "lossy"
	}
	return "default", "", ""
}
