package identification

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzIdentify: identification is on the ingest path, and ingest must survive
// whatever a filesystem hands it. Nothing here checks that the answer is
// sensible — only that there is always an answer, and never a panic.
func FuzzIdentify(f *testing.F) {
	for _, group := range [][]string{movieCases, seriesCases, musicCases, bookCases} {
		for _, p := range group {
			f.Add(p, Movie)
		}
	}
	for _, libraryType := range []string{"", Series, Music, Book, Unknown, "nonsense"} {
		f.Add("Show/Season 02/S02E05 - Title.mkv", libraryType)
	}
	for _, seed := range []string{
		"", " ", "/", "//", "..", "../../etc/passwd", ".hidden", "no-extension",
		"S99E99", "s02e05e06e07e08", "9999x9999", "(((((", "[[[[", "()[]{}",
		"Season 99999999999999999999/S99999999999999999999E1.mkv",
		"\x00\x01\x02.mkv", strings.Repeat("a/", 200) + "b.mkv",
		strings.Repeat("x", 4096) + ".mkv", "é/ü/ß.mkv", "日本語/映画 (2019).mkv",
		"CD1/CD2/CD3/01.flac", "-.-.-.-.-.mkv", "1080p.mkv", "2019.mkv",
	} {
		f.Add(seed, "")
	}

	r := Default()
	f.Fuzz(func(t *testing.T, relPath, libraryType string) {
		c := r.Identify(relPath, libraryType)

		if c.ContentType == "" {
			t.Fatalf("%q: empty ContentType", relPath)
		}
		if c.WorkKey == "" {
			t.Fatalf("%q: empty WorkKey", relPath)
		}
		if c.EditionKey == "" {
			t.Fatalf("%q: empty EditionKey", relPath)
		}
		if c.AssetRole == "" {
			t.Fatalf("%q: empty AssetRole", relPath)
		}
		if c.WorkAttributes == nil || c.EditionAttributes == nil {
			t.Fatalf("%q: nil attribute map", relPath)
		}
		switch c.Source {
		case SourcePathHeuristic:
			if !c.Identified || c.Rule == "" {
				t.Fatalf("%q: identified without a rule name", relPath)
			}
		case SourceUnidentified:
			if c.Identified || c.WorkKey != UnidentifiedWorkKey {
				t.Fatalf("%q: unidentified candidate is malformed: %#v", relPath, c)
			}
		default:
			t.Fatalf("%q: unknown Source %q", relPath, c.Source)
		}
		if utf8.ValidString(relPath) && !utf8.ValidString(c.WorkKey) {
			t.Fatalf("%q: valid input produced an invalid WorkKey %q", relPath, c.WorkKey)
		}
		// The key is the primary key of a get-or-create: it must not carry
		// separators that would make two different works collide oddly.
		if strings.ContainsAny(c.WorkKey, "\n\r\x00") {
			t.Fatalf("%q: WorkKey contains a control character: %q", relPath, c.WorkKey)
		}
	})
}

// FuzzParsePath: the split must round-trip and never lose the basename.
func FuzzParsePath(f *testing.F) {
	for _, seed := range []string{
		"a/b/c.mkv", "", "/", "//a", "a//b", "./a", "a/./b.mp3", "..", "a/../b.mp3",
		".hidden", "x.", "x..y", strings.Repeat("/", 100),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, relPath string) {
		p := ParsePath(relPath)
		if !strings.HasPrefix(p.Base, p.Stem) {
			t.Fatalf("%q: Stem %q is not a prefix of Base %q", relPath, p.Stem, p.Base)
		}
		// Ext is the lowercased remainder. Case folding is not always
		// length-preserving (Turkish dotted capital I folds to two runes), so
		// the split is asserted on the raw bytes and the fold separately.
		if raw := p.Base[len(p.Stem):]; p.Ext != strings.ToLower(raw) {
			t.Fatalf("%q: Ext %q is not the lowercased tail %q", relPath, p.Ext, raw)
		}
		if p.Base == "" && p.Rel != "" {
			t.Fatalf("%q: non-empty Rel %q with an empty Base", relPath, p.Rel)
		}
		if p.Ext != strings.ToLower(p.Ext) {
			t.Fatalf("%q: Ext %q is not lowercased", relPath, p.Ext)
		}
		for _, d := range p.Dirs {
			if d == "" || d == "." {
				t.Fatalf("%q: empty directory segment in %#v", relPath, p.Dirs)
			}
		}
	})
}
