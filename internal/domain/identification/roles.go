package identification

import "strings"

// Extensions that carry no content of their own and therefore say nothing about
// the content type. They are accepted by every rule, because a poster or a
// subtitle must resolve to the same Work as the file it sits next to.
var (
	subtitleExts = newSet(".srt", ".ass", ".ssa", ".sub", ".idx", ".vtt", ".sup", ".smi", ".pgs")
	imageExts    = newSet(".jpg", ".jpeg", ".png", ".webp", ".tbn", ".bmp", ".gif")
)

// Directory names that hold bonus material. "Specials" is deliberately absent:
// for a series it is season zero, not an extra.
var extrasDirs = newSet(
	"featurettes", "featurette", "extras", "extra", "bonus", "behind the scenes",
	"deleted scenes", "interviews", "scenes", "shorts", "trailers", "other",
	"sample", "samples",
)

// Directories that hold companion files for the work beside them. They are
// stepped over without changing the asset's role.
var transparentDirs = newSet("subs", "subtitles", "sub", "artwork", "images", "covers", "fanart")

// Filenames that name a role rather than a work.
var genericArtworkStems = newSet(
	"poster", "cover", "folder", "fanart", "banner", "backdrop", "background",
	"thumb", "thumbnail", "disc", "discart", "clearlogo", "logo", "landscape",
	"art", "albumart", "front", "movie", "show", "season", "default",
)

// Suffixes Kodi/Jellyfin append to an artwork file that otherwise names its work.
var artworkSuffixes = []string{
	"-poster", "-fanart", "-banner", "-thumb", "-clearlogo", "-logo", "-disc",
	"-discart", "-landscape", "-backdrop", "-background", "-cover",
}

func isSubtitleExt(ext string) bool { return subtitleExts[ext] }
func isImageExt(ext string) bool    { return imageExts[ext] }

// isAuxExt reports whether the extension belongs to a companion file rather
// than to content.
func isAuxExt(ext string) bool { return isSubtitleExt(ext) || isImageExt(ext) }

// roleView determines the asset role, any language the filename declares, and
// the view of the path that the rules should match against.
//
// For a non-primary asset the view is rewritten to point at the sibling primary
// file — the extras directory is dropped and a filename that names a role
// rather than a work ("poster.jpg") is replaced by the directory above it. That
// rewrite is what makes a poster attach to its movie instead of creating a Work
// called "Poster".
func roleView(p Path) (role, lang string, view Path) {
	role = RolePrimary
	dirs := make([]string, 0, len(p.Dirs))
	inExtras := false
	for _, d := range p.Dirs {
		n := normalizeName(d)
		if extrasDirs[n] {
			inExtras = true
			continue
		}
		if transparentDirs[n] {
			continue
		}
		dirs = append(dirs, d)
	}

	stem := p.Stem
	promote := false

	switch {
	case isSubtitleExt(p.Ext):
		role = RoleSubtitle
		stem, lang = splitSubtitleLang(stem)
	case isImageExt(p.Ext):
		role = RoleArtwork
		stem = trimArtworkSuffix(stem)
		if stem == "" || genericArtworkStems[normalizeName(stem)] || isSeasonPoster(stem) {
			promote = true
		}
	}
	if inExtras {
		// A featurette's filename describes the featurette, never the work.
		role = RoleExtra
		promote = true
	}

	if promote && len(dirs) > 0 {
		stem = dirs[len(dirs)-1]
		dirs = dirs[:len(dirs)-1]
	}

	view = Path{Rel: p.Rel, Dirs: dirs, Base: stem + p.Ext, Stem: stem, Ext: p.Ext}
	return role, lang, view
}

// isSeasonPoster matches "season01-poster", "season-specials" and friends after
// the artwork suffix has already been trimmed.
func isSeasonPoster(stem string) bool {
	n := normalizeName(stem)
	return strings.HasPrefix(n, "season") || strings.HasPrefix(n, "specials")
}

func trimArtworkSuffix(stem string) string {
	lower := strings.ToLower(stem)
	for _, suf := range artworkSuffixes {
		if strings.HasSuffix(lower, suf) && len(stem) > len(suf) {
			return stem[:len(stem)-len(suf)]
		}
	}
	return stem
}

// Subtitle flag words that sit between the name and the language code.
var subtitleFlags = newSet("forced", "sdh", "cc", "hi", "default", "full")

// splitSubtitleLang peels a trailing language code off a subtitle stem:
// "Movie (2019).en" -> "Movie (2019)", "en". A trailing component is only
// consumed when it is a language we recognise, so a title ending in a short
// word survives intact.
func splitSubtitleLang(stem string) (rest, lang string) {
	rest = stem
	for range 3 {
		i := strings.LastIndexAny(rest, "._-")
		if i <= 0 {
			break
		}
		tail := strings.ToLower(strings.TrimSpace(rest[i+1:]))
		switch {
		case subtitleFlags[tail]:
			rest = rest[:i]
		case languageCodes[tail]:
			if lang == "" {
				lang = canonicalLanguage(tail)
			}
			rest = rest[:i]
		case languageNames[tail] != "":
			if lang == "" {
				lang = languageNames[tail]
			}
			rest = rest[:i]
		default:
			return rest, lang
		}
	}
	return rest, lang
}

var languageCodes = newSet(
	"en", "eng", "es", "spa", "fr", "fre", "fra", "de", "ger", "deu", "it", "ita",
	"pt", "por", "nl", "dut", "nld", "ru", "rus", "ja", "jpn", "ko", "kor",
	"zh", "chi", "zho", "cmn", "ar", "ara", "sv", "swe", "no", "nor", "da", "dan",
	"fi", "fin", "pl", "pol", "tr", "tur", "he", "heb", "hi", "hin", "th", "tha",
	"cs", "cze", "el", "ell", "hu", "hun", "ro", "ron", "uk", "ukr", "vi", "vie",
	"id", "ind", "ms", "may", "ca", "cat", "sr", "hr", "bg", "sk", "sl", "et",
	"lv", "lt", "fa", "per", "bn", "ta", "te", "ml",
)

// canonicalLanguage folds a three-letter code onto its ISO 639-1 equivalent so
// editions.language holds one spelling per language.
func canonicalLanguage(code string) string {
	if two, ok := languageAliases[code]; ok {
		return two
	}
	return code
}

var languageAliases = map[string]string{
	"eng": "en", "spa": "es", "fre": "fr", "fra": "fr", "ger": "de", "deu": "de",
	"ita": "it", "por": "pt", "dut": "nl", "nld": "nl", "rus": "ru", "jpn": "ja",
	"kor": "ko", "chi": "zh", "zho": "zh", "cmn": "zh", "ara": "ar", "swe": "sv",
	"nor": "no", "dan": "da", "fin": "fi", "pol": "pl", "tur": "tr", "heb": "he",
	"hin": "hi", "tha": "th", "cze": "cs", "ell": "el", "hun": "hu", "ron": "ro",
	"ukr": "uk", "vie": "vi", "ind": "id", "may": "ms", "cat": "ca", "per": "fa",
}

var languageNames = map[string]string{
	"english":    "en",
	"spanish":    "es",
	"french":     "fr",
	"german":     "de",
	"italian":    "it",
	"portuguese": "pt",
	"dutch":      "nl",
	"russian":    "ru",
	"japanese":   "ja",
	"korean":     "ko",
	"chinese":    "zh",
	"arabic":     "ar",
	"swedish":    "sv",
	"norwegian":  "no",
	"danish":     "da",
	"finnish":    "fi",
	"polish":     "pl",
	"turkish":    "tr",
	"hebrew":     "he",
	"greek":      "el",
	"czech":      "cs",
	"hungarian":  "hu",
	"romanian":   "ro",
	"ukrainian":  "uk",
	"thai":       "th",
	"vietnamese": "vi",
}
