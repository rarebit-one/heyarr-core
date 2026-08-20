package identification

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// A four-digit number is only read as a year inside this window. The upper
// bound is what keeps "Blade Runner 2049" a title and not a release year.
const (
	minPlausibleYear = 1888
	maxPlausibleYear = 2035
)

func newSet(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, i := range items {
		m[i] = true
	}
	return m
}

// reBracketed strips [release-group] and {edition} tags wholesale: their
// contents are never part of a title and frequently contain years.
var reBracketed = regexp.MustCompile(`[\[\{][^\]\}]*[\]\}]`)

var punctuationStripper = strings.NewReplacer("'", "", "’", "", "`", "", "´", "")

// isSeparator treats every filename separator alike, which is what makes
// "Movie.Title", "Movie_Title" and "Movie Title" the same three tokens.
// "&" is preserved as its own token and folded to "and" during key
// normalisation, so "Fast & Furious" and "Fast and Furious" converge.
func isSeparator(r rune) bool {
	switch r {
	case '.', '_', '-', '+', '(', ')', '[', ']', '{', '}', ',', ';', ':', '!',
		'?', '*', '"', '/', '\\', '~', '=', '#', '@', '|', '<', '>', '–', '—':
		return true
	}
	return unicode.IsSpace(r)
}

// tokenize splits a filename fragment into display tokens.
func tokenize(s string) []string {
	s = reBracketed.ReplaceAllString(s, " ")
	s = punctuationStripper.Replace(s)
	fields := strings.FieldsFunc(s, isSeparator)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func parseYear(tok string) (int, bool) {
	if len(tok) != 4 {
		return 0, false
	}
	n, err := strconv.Atoi(tok)
	if err != nil || n < minPlausibleYear || n > maxPlausibleYear {
		return 0, false
	}
	return n, true
}

// nameParts is a filename fragment split into the part that names the work and
// the part that describes the release.
type nameParts struct {
	Title []string // display tokens, original casing
	Year  int      // 0 when unknown
	Meta  []string // lowercased release-metadata tokens
}

// Empty reports whether nothing usable was found.
func (n nameParts) Empty() bool { return len(n.Title) == 0 }

// parseName splits a fragment at the first release-metadata token or at the
// year, whichever comes first. The cut is deliberately symmetric: even when it
// truncates a real title ("The Final Cut" -> "final"), every spelling of that
// filename truncates identically, so the WorkKey still converges.
func parseName(s string) nameParts {
	toks := tokenize(s)
	var np nameParts
	if len(toks) == 0 {
		return np
	}

	firstNoise := len(toks)
	for i := 1; i < len(toks); i++ {
		if isNoiseToken(strings.ToLower(toks[i])) {
			firstNoise = i
			break
		}
	}

	// The last plausible year before the metadata run is the release year:
	// scene names put it immediately in front of the quality tokens, which
	// keeps a number that is part of the title ("2001: A Space Odyssey") from
	// being mistaken for one.
	cut := firstNoise
	for i := 1; i < firstNoise; i++ {
		if y, ok := parseYear(toks[i]); ok {
			np.Year = y
			cut = i
		}
	}

	np.Title = toks[:cut]
	meta := make([]string, 0, len(toks)-cut)
	for _, t := range toks[cut:] {
		meta = append(meta, strings.ToLower(t))
	}
	// A bracketed tag never names the work, but it very often describes the
	// release: Jellyfin writes "Movie (2019) [Bluray-1080p]".
	for _, b := range reBracketed.FindAllString(s, -1) {
		for _, t := range tokenize(b[1 : len(b)-1]) {
			meta = append(meta, strings.ToLower(t))
		}
	}
	np.Meta = meta
	return np
}

var leadingArticles = newSet("the", "a", "an")

// normKey reduces display tokens to the aggressively normalised form the
// WorkKey is built from: lowercase, punctuation-free, leading article stripped.
func normKey(toks []string) string {
	out := make([]string, 0, len(toks))
	for _, t := range toks {
		t = strings.ToLower(t)
		if t == "&" {
			t = "and"
		}
		t = strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				return unicode.ToLower(r)
			}
			return -1
		}, t)
		if t != "" {
			out = append(out, t)
		}
	}
	if len(out) > 1 && leadingArticles[out[0]] {
		out = out[1:]
	}
	return strings.Join(out, " ")
}

// normalizeName is normKey over a raw string, used for comparing directory and
// role names.
func normalizeName(s string) string { return normKey(tokenize(s)) }

// displayTitle joins tokens back into a human title, capitalising only tokens
// that arrived entirely lowercase so "REDS" and "iMac" survive.
func displayTitle(toks []string) string {
	out := make([]string, 0, len(toks))
	for _, t := range toks {
		out = append(out, capitalize(t))
	}
	return strings.Join(out, " ")
}

func capitalize(t string) string {
	if t == "" || strings.IndexFunc(t, unicode.IsUpper) >= 0 {
		return t
	}
	r := []rune(t)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// workKey joins a content type and its already-normalised identity parts. Empty
// parts are dropped so an unknown artist or year cannot shift the shape of the
// key for everything that follows it.
func workKey(contentType string, parts ...string) string {
	out := make([]string, 0, len(parts)+1)
	out = append(out, contentType)
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ":")
}

func yearKey(year int) string {
	if year <= 0 {
		return ""
	}
	return strconv.Itoa(year)
}
