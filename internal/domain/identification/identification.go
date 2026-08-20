package identification

import (
	"path"
	"strings"
)

// Content types produced by this package. They mirror works.content_type and
// libraries.content_type (spec §12).
const (
	// Movie is a single feature-length work.
	Movie = "movie"
	// Series is an episodic work; the Work is the series, not the episode.
	Series = "series"
	// Music is an album; the Work is the album, not the track.
	Music = "music"
	// Book is a book, comic or audiobook.
	Book = "book"
	// Unknown is the content type of the synthetic Unidentified Work, and the
	// value a caller passes to Identify when the owning library's type should
	// not bias rule selection.
	Unknown = "unknown"
)

// Values written to assets.identification_source.
const (
	// SourcePathHeuristic marks a row this package identified from its path.
	// Milestone 3's identifier re-resolves exactly these rows.
	SourcePathHeuristic = "path-heuristic"
	// SourceUnidentified marks a row that ingested without being identified.
	SourceUnidentified = "unidentified"
)

// Asset roles (assets.role). A non-primary asset attaches to the same Work as
// its sibling primary file, which is the whole point of the role field.
const (
	// RolePrimary is the content itself.
	RolePrimary = "primary"
	// RoleSubtitle is an external subtitle track.
	RoleSubtitle = "subtitle"
	// RoleArtwork is a poster, cover, fanart or similar image.
	RoleArtwork = "artwork"
	// RoleExtra is a featurette, deleted scene or other bonus material.
	RoleExtra = "extra"
)

// UnidentifiedWorkKey is the work_key of the synthetic Unidentified Work. Every
// unparseable file in a library lands under this one Work rather than under a
// Work per file.
const UnidentifiedWorkKey = "unidentified"

// Candidate is what a path parses to. It is data only — it knows nothing about
// how or where it will be persisted.
type Candidate struct {
	ContentType string // one of the content-type constants
	WorkKey     string // stable normalised key; drives get-or-create on works(content_type, work_key)
	Title       string // works.title
	SortTitle   string // leading-article-stripped, lowercased, for works.sort_title
	Year        int    // 0 when unknown
	// WorkAttributes holds facts about the Work itself, stable across every
	// file that resolves to it -> works.attributes JSON.
	WorkAttributes map[string]any
	// EditionAttributes holds facts about this particular edition or placement
	// -> editions.attributes JSON. Per-file facts must never land on the Work:
	// the Work row is shared, so an episode number written there would be
	// rewritten by every episode on every scan.
	EditionAttributes map[string]any
	EditionKey        string // stable key for get-or-create of the edition within the work
	EditionLabel      string // editions.label, e.g. "2160p HDR", "Season 02", "FLAC"
	EditionType       string // editions.edition_type, e.g. "remux", "web-dl", "lossless", "epub"
	Language          string // editions.language, "" when unknown
	AssetRole         string // assets.role
	Source            string // SourcePathHeuristic or SourceUnidentified
	Rule              string // the matched rule's name, e.g. "movie/title-year-dir"; "" when unidentified
	Identified        bool
}

// Rule is one named path-shape heuristic. Registering a fourth content type is
// registering rules — never a schema change.
//
// Match reports whether the rule recognises the path. A rule may leave
// ContentType, Rule, the attribute maps, AssetRole, Source and Identified
// unset: the Registry fills them in. Setting Rule lets one registered Rule report a more
// precise sub-rule name than its own.
type Rule struct {
	Name        string
	ContentType string
	Match       func(p Path) (Candidate, bool)
}

// Path is the pre-split, pre-normalised input a rule matches against. It is
// derived from a slash-separated path relative to the library root.
type Path struct {
	Rel  string   // e.g. "Series/Season 02/S02E05 - Title.mkv" — always forward slashes
	Dirs []string // ["Series", "Season 02"]
	Base string   // "S02E05 - Title.mkv"
	Stem string   // "S02E05 - Title"
	Ext  string   // ".mkv", lowercased
}

// Dir returns the innermost directory, or "" when the path has none.
func (p Path) Dir() string {
	if len(p.Dirs) == 0 {
		return ""
	}
	return p.Dirs[len(p.Dirs)-1]
}

// ParsePath builds a Path from a slash-separated relative path. Backslashes are
// accepted and folded to slashes so a Windows scanner does not need to care.
func ParsePath(relPath string) Path {
	rel := strings.ReplaceAll(relPath, "\\", "/")
	rel = path.Clean("/" + rel)
	rel = strings.TrimPrefix(rel, "/")

	segs := make([]string, 0, 4)
	for _, s := range strings.Split(rel, "/") {
		if s != "" && s != "." {
			segs = append(segs, s)
		}
	}
	p := Path{Rel: strings.Join(segs, "/")}
	if len(segs) == 0 {
		return p
	}
	p.Base = segs[len(segs)-1]
	p.Dirs = segs[:len(segs)-1]
	ext := path.Ext(p.Base)
	p.Stem = p.Base[:len(p.Base)-len(ext)]
	p.Ext = strings.ToLower(ext)
	return p
}

// Unidentified is the synthetic fallback Work. Identification failure must
// never be ingest failure: an unparseable file still gets a Work, an Edition
// and an Asset, all flagged so they can be found again.
func Unidentified(_ Path) Candidate {
	return Candidate{
		ContentType:       Unknown,
		WorkKey:           UnidentifiedWorkKey,
		Title:             "Unidentified",
		SortTitle:         "unidentified",
		WorkAttributes:    map[string]any{},
		EditionAttributes: map[string]any{},
		EditionKey:        UnidentifiedWorkKey,
		AssetRole:         RolePrimary,
		Source:            SourceUnidentified,
		Identified:        false,
	}
}

// Registry is an ordered set of rules. The zero value is not usable; call
// NewRegistry or Default.
type Registry struct {
	rules []Rule
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry { return &Registry{} }

// Register appends rules. Order is significant: the first rule that matches
// wins, so register the specific before the general.
func (r *Registry) Register(rules ...Rule) { r.rules = append(r.rules, rules...) }

// Rules returns the registered rules in order.
func (r *Registry) Rules() []Rule { return r.rules[:len(r.rules):len(r.rules)] }

// Identify parses relPath (slash-separated, relative to the library root).
//
// libraryContentType is the owning library's content_type and biases rule
// selection: rules of that type are tried first, then the rest. Pass "" or
// Unknown to try every rule in registration order.
//
// It never returns an error and never returns a zero Candidate: an unparseable
// path yields the synthetic Unidentified candidate, carrying whatever asset
// role could still be determined from the file extension.
func (r *Registry) Identify(relPath, libraryContentType string) Candidate {
	p := ParsePath(relPath)
	if p.Stem == "" {
		return Unidentified(p)
	}

	role, lang, view := roleView(p)
	if view.Stem != "" {
		for _, rule := range r.ordered(libraryContentType) {
			if rule.Match == nil {
				continue
			}
			c, ok := rule.Match(view)
			if !ok || c.WorkKey == "" {
				continue
			}
			if c.ContentType == "" {
				c.ContentType = rule.ContentType
			}
			if c.Rule == "" {
				c.Rule = rule.Name
			}
			if c.WorkAttributes == nil {
				c.WorkAttributes = map[string]any{}
			}
			if c.EditionAttributes == nil {
				c.EditionAttributes = map[string]any{}
			}
			if c.EditionKey == "" {
				c.EditionKey = "default"
			}
			if c.Language == "" {
				c.Language = lang
			}
			c.AssetRole = role
			c.Source = SourcePathHeuristic
			c.Identified = true
			return c
		}
	}

	c := Unidentified(p)
	c.AssetRole = role
	c.Language = lang
	return c
}

// ordered returns the rules with those matching ct first — a bias, not a
// filter, so a stray file of another shape is still recognised.
func (r *Registry) ordered(ct string) []Rule {
	if ct == "" || ct == Unknown {
		return r.rules
	}
	out := make([]Rule, 0, len(r.rules))
	for _, rule := range r.rules {
		if rule.ContentType == ct {
			out = append(out, rule)
		}
	}
	if len(out) == 0 || len(out) == len(r.rules) {
		return r.rules
	}
	for _, rule := range r.rules {
		if rule.ContentType != ct {
			out = append(out, rule)
		}
	}
	return out
}

// Default returns a Registry with the built-in movie, series, music and book
// rules, most specific first.
func Default() *Registry {
	r := NewRegistry()
	r.Register(seriesRules()...)
	r.Register(movieRules()...)
	r.Register(musicRules()...)
	r.Register(bookRules()...)
	r.Register(fallbackRules()...)
	return r
}

// Identify is a convenience wrapper over Default. It builds the default
// registry on every call; hold a *Registry in anything hot.
func Identify(relPath, libraryContentType string) Candidate {
	return Default().Identify(relPath, libraryContentType)
}
