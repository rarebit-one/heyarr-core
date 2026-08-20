package identification

import (
	"regexp"
	"strings"
)

var (
	textBookExts  = newSet(".epub", ".mobi", ".azw", ".azw3", ".pdf", ".djvu", ".fb2", ".lit", ".pdb")
	comicExts     = newSet(".cbz", ".cbr", ".cb7", ".cbt")
	audiobookExts = newSet(".m4b")
)

// "Series 03 - Title", "Series #3 - Title", "Series Book 3 - Title".
var reBookSeries = regexp.MustCompile(`(?i)^(.+?)[ ._-]+(?:book[ ._-]*)?#?(\d{1,3})(?:\.\d)?[ ._-]*-[ ._-]*(.+)$`)

// A Calibre author directory ends with the book id: "Title (1234)".
var reCalibreDir = regexp.MustCompile(`^(.+?)\s*\((\d{1,7})\)$`)

func isBookExt(ext string) bool {
	return textBookExts[ext] || comicExts[ext] || audiobookExts[ext]
}

func isBookPath(p Path) bool { return isBookExt(p.Ext) || isAuxExt(p.Ext) }

func bookRules() []Rule {
	return []Rule{
		{Name: "book/calibre", ContentType: Book, Match: matchBookCalibre},
		{Name: "book/author-dir-series", ContentType: Book, Match: matchBookAuthorSeries},
		{Name: "book/author-dir", ContentType: Book, Match: matchBookAuthorDir},
		{Name: "book/author-title", ContentType: Book, Match: matchBookFlat},
		{Name: "book/title-only", ContentType: Book, Match: matchBookTitleOnly},
	}
}

// matchBookCalibre is the Calibre layout: "Author/Title (1234)/Title - Author.epub".
func matchBookCalibre(p Path) (Candidate, bool) {
	if !isBookPath(p) || len(p.Dirs) < 2 {
		return Candidate{}, false
	}
	m := reCalibreDir.FindStringSubmatch(strings.TrimSpace(p.Dir()))
	if m == nil {
		return Candidate{}, false
	}
	author := p.Dirs[len(p.Dirs)-2]
	title := m[1]
	// Calibre repeats the author in the filename; only trust the shape when it
	// actually does, otherwise this is an ordinary "Title (2011)" directory.
	if left, right, ok := splitPair(p.Stem); ok {
		if normalizeName(right) != normalizeName(author) {
			return Candidate{}, false
		}
		title = left
	} else if normalizeName(p.Stem) != normalizeName(title) {
		return Candidate{}, false
	}
	return bookCandidate(parseName(author), parseName(title), "", 0, p), true
}

// matchBookAuthorSeries is "Author/Series 03 - Title.epub".
func matchBookAuthorSeries(p Path) (Candidate, bool) {
	if !isBookPath(p) || len(p.Dirs) == 0 {
		return Candidate{}, false
	}
	m := reBookSeries.FindStringSubmatch(p.Stem)
	if m == nil {
		return Candidate{}, false
	}
	series := parseName(m[1])
	if series.Empty() {
		return Candidate{}, false
	}
	return bookCandidate(parseName(p.Dir()), parseName(m[3]), displayTitle(series.Title), atoi(m[2]), p), true
}

// matchBookAuthorDir is "Author/Title.epub", the commonest shelf layout.
func matchBookAuthorDir(p Path) (Candidate, bool) {
	if !isBookPath(p) || len(p.Dirs) == 0 {
		return Candidate{}, false
	}
	author := parseName(p.Dir())
	title := parseName(p.Stem)
	if author.Empty() || title.Empty() {
		return Candidate{}, false
	}
	// "Author/Author - Title.epub" repeats the author; do not read it as a title.
	if left, right, ok := splitPair(p.Stem); ok && normalizeName(left) == normKey(author.Title) {
		title = parseName(right)
	}
	return bookCandidate(author, title, "", 0, p), true
}

// matchBookFlat is "Author - Title (2011).epub" with no directory to help.
func matchBookFlat(p Path) (Candidate, bool) {
	if !isBookPath(p) {
		return Candidate{}, false
	}
	left, right, ok := splitPair(p.Stem)
	if !ok {
		return Candidate{}, false
	}
	author := parseName(left)
	title := parseName(right)
	if author.Empty() || title.Empty() {
		return Candidate{}, false
	}
	return bookCandidate(author, title, "", 0, p), true
}

// matchBookTitleOnly is the last resort: a bare title with a book extension.
func matchBookTitleOnly(p Path) (Candidate, bool) {
	if !isBookExt(p.Ext) {
		return Candidate{}, false
	}
	title := parseName(p.Stem)
	if title.Empty() {
		return Candidate{}, false
	}
	return bookCandidate(nameParts{}, title, "", 0, p), true
}

func bookCandidate(author, title nameParts, series string, seriesIndex int, p Path) Candidate {
	titleKey := normKey(title.Title)
	if titleKey == "" {
		return Candidate{}
	}
	authorKey := normKey(author.Title)

	work := map[string]any{}
	if authorKey != "" {
		work["author"] = displayTitle(author.Title)
	}
	if series != "" {
		work["series"] = series
		if seriesIndex > 0 {
			work["series_index"] = seriesIndex
		}
	}

	edition := map[string]any{}
	format := strings.TrimPrefix(p.Ext, ".")
	editionType := format
	switch {
	case comicExts[p.Ext]:
		edition["format"] = "comic"
	case audiobookExts[p.Ext]:
		edition["format"] = "audiobook"
	}
	if !isBookExt(p.Ext) {
		// A companion file inherits the work but names no edition of its own.
		format, editionType = "default", ""
	}

	label := ""
	if editionType != "" {
		label = strings.ToUpper(editionType)
	}

	return Candidate{
		ContentType:       Book,
		WorkKey:           workKey(Book, authorKey, titleKey, yearKey(title.Year)),
		Title:             displayTitle(title.Title),
		SortTitle:         titleKey,
		Year:              title.Year,
		WorkAttributes:    work,
		EditionAttributes: edition,
		EditionKey:        format,
		EditionLabel:      label,
		EditionType:       editionType,
	}
}
