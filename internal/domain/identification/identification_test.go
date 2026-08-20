package identification

import (
	"reflect"
	"strings"
	"testing"
)

func TestParsePath(t *testing.T) {
	tests := []struct {
		in   string
		want Path
	}{
		{"Series/Season 02/S02E05 - Title.mkv", Path{
			Rel:  "Series/Season 02/S02E05 - Title.mkv",
			Dirs: []string{"Series", "Season 02"},
			Base: "S02E05 - Title.mkv", Stem: "S02E05 - Title", Ext: ".mkv",
		}},
		{"movie.MKV", Path{Rel: "movie.MKV", Dirs: []string{}, Base: "movie.MKV", Stem: "movie", Ext: ".mkv"}},
		{"./a//b/c.mp3", Path{
			Rel: "a/b/c.mp3", Dirs: []string{"a", "b"}, Base: "c.mp3", Stem: "c", Ext: ".mp3",
		}},
		{`Windows\Path\file.mkv`, Path{
			Rel: "Windows/Path/file.mkv", Dirs: []string{"Windows", "Path"},
			Base: "file.mkv", Stem: "file", Ext: ".mkv",
		}},
		{"no-extension", Path{Rel: "no-extension", Dirs: []string{}, Base: "no-extension", Stem: "no-extension"}},
		{".hidden", Path{Rel: ".hidden", Dirs: []string{}, Base: ".hidden", Stem: "", Ext: ".hidden"}},
		{"", Path{}},
		{"/", Path{}},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := ParsePath(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParsePath(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

// TestUnparseablePathsStillIngest is the acceptance criterion that
// identification failure must never be ingest failure.
func TestUnparseablePathsStillIngest(t *testing.T) {
	r := Default()
	for _, p := range []string{
		"random-garbage.bin",
		".hidden",
		"no-extension",
		"",
		"   ",
		"...",
		"a/b/c/.DS_Store",
		"Thumbs.db",
		"data.sqlite",
		"archive.tar.gz",
		"____.___",
	} {
		t.Run(p, func(t *testing.T) {
			got := r.Identify(p, "")
			if got.Identified {
				t.Fatalf("%q was identified as %s/%q; it should not have been", p, got.ContentType, got.WorkKey)
			}
			if got.ContentType != Unknown {
				t.Errorf("ContentType = %q, want %q", got.ContentType, Unknown)
			}
			if got.WorkKey != UnidentifiedWorkKey {
				t.Errorf("WorkKey = %q, want %q — every unidentified file shares one Work", got.WorkKey, UnidentifiedWorkKey)
			}
			if got.Source != SourceUnidentified {
				t.Errorf("Source = %q, want %q", got.Source, SourceUnidentified)
			}
			if got.Rule != "" {
				t.Errorf("Rule = %q, want empty", got.Rule)
			}
			if got.WorkAttributes == nil || got.EditionAttributes == nil {
				t.Error("attribute maps must be empty, never nil")
			}
			if got.AssetRole == "" {
				t.Error("AssetRole must always be set")
			}
		})
	}
}

// TestUnidentifiedIsTotal: whatever a scanner hands over, a candidate comes
// back that a caller can persist without a nil check.
func TestUnidentifiedIsTotal(t *testing.T) {
	c := Unidentified(ParsePath("whatever.bin"))
	check(t, "ContentType", c.ContentType, Unknown)
	check(t, "WorkKey", c.WorkKey, UnidentifiedWorkKey)
	check(t, "Title", c.Title, "Unidentified")
	check(t, "SortTitle", c.SortTitle, "unidentified")
	check(t, "AssetRole", c.AssetRole, RolePrimary)
	check(t, "Source", c.Source, SourceUnidentified)
	check(t, "Identified", c.Identified, false)
	if len(c.WorkAttributes) != 0 || len(c.EditionAttributes) != 0 {
		t.Error("the synthetic Work carries no attributes")
	}
}

// TestCompanionFilesJoinTheirSibling: the whole point of the role field is that
// a poster and a subtitle land on the Work of the file beside them.
func TestCompanionFilesJoinTheirSibling(t *testing.T) {
	r := Default()
	primary := r.Identify("Movie Title (2019)/Movie Title (2019) - 2160p.mkv", Movie)

	companions := map[string]string{
		"Movie Title (2019)/poster.jpg":                            RoleArtwork,
		"Movie Title (2019)/cover.jpg":                             RoleArtwork,
		"Movie Title (2019)/folder.png":                            RoleArtwork,
		"Movie Title (2019)/Movie Title (2019)-fanart.jpg":         RoleArtwork,
		"Movie Title (2019)/Movie Title (2019).en.srt":             RoleSubtitle,
		"Movie Title (2019)/Movie Title (2019).es.ass":             RoleSubtitle,
		"Movie Title (2019)/Featurettes/Making Of.mkv":             RoleExtra,
		"Movie Title (2019)/Extras/Deleted Scenes.mkv":             RoleExtra,
		"Movie Title (2019)/Behind The Scenes/Interview.mkv":       RoleExtra,
		"Movie Title (2019)/Subs/Movie Title (2019).en.forced.srt": RoleSubtitle,
	}
	for path, wantRole := range companions {
		got := r.Identify(path, Movie)
		if got.AssetRole != wantRole {
			t.Errorf("%q: AssetRole = %q, want %q", path, got.AssetRole, wantRole)
		}
		if got.WorkKey != primary.WorkKey {
			t.Errorf("%q: WorkKey = %q, want %q — a companion must join its sibling's Work",
				path, got.WorkKey, primary.WorkKey)
		}
	}
}

func TestSubtitleLanguage(t *testing.T) {
	r := Default()
	tests := map[string]string{
		"Movie Title (2019)/Movie Title (2019).en.srt":        "en",
		"Movie Title (2019)/Movie Title (2019).eng.srt":       "en",
		"Movie Title (2019)/Movie Title (2019).english.srt":   "en",
		"Movie Title (2019)/Movie Title (2019).pt.forced.srt": "pt",
		"Movie Title (2019)/Movie Title (2019).srt":           "",
		// "It" is not a language code, and the title survives intact.
		"It (2017)/It (2017).srt": "",
	}
	for path, want := range tests {
		if got := r.Identify(path, Movie); got.Language != want {
			t.Errorf("%q: Language = %q, want %q", path, got.Language, want)
		}
	}
}

// TestLibraryContentTypeBiases: the owning library's type decides how an
// otherwise ambiguous shape is read.
func TestLibraryContentTypeBiases(t *testing.T) {
	r := Default()
	const path = "The Expanse (2015)/poster.jpg"

	if got := r.Identify(path, Series); got.ContentType != Series {
		t.Errorf("in a series library, %q identified as %q", path, got.ContentType)
	}
	if got := r.Identify(path, Movie); got.ContentType != Movie {
		t.Errorf("in a movie library, %q identified as %q", path, got.ContentType)
	}
	// Unknown falls back to registration order, which puts the movie fallback
	// ahead of the series one.
	if got := r.Identify(path, ""); got.ContentType != Movie {
		t.Errorf("with no library type, %q identified as %q", path, got.ContentType)
	}
	// The bias is a bias, not a filter: an episode in a movie library is still
	// read by the rule that understands it once the movie rules decline.
	got := r.Identify("Show/Season 02/S02E05.mkv", Book)
	if got.ContentType != Series || got.Rule != "series/sxxexx" {
		t.Errorf("in a book library, an episode identified as %q by %q", got.ContentType, got.Rule)
	}
}

// TestDeterministic: the same path must always produce the same candidate, from
// any registry instance. No clock, no map iteration order, no state.
func TestDeterministic(t *testing.T) {
	all := map[string][]string{
		Movie: movieCases, Series: seriesCases, Music: musicCases, Book: bookCases,
	}
	for libraryType, paths := range all {
		for _, p := range paths {
			first := Default().Identify(p, libraryType)
			for range 5 {
				if got := Default().Identify(p, libraryType); !reflect.DeepEqual(got, first) {
					t.Fatalf("%q is not deterministic:\n first: %#v\n later: %#v", p, first, got)
				}
			}
		}
	}
}

// TestEmptyRegistryIdentifiesNothing: a Registry with no rules is legal and
// still total.
func TestEmptyRegistryIdentifiesNothing(t *testing.T) {
	got := NewRegistry().Identify("Movie Title (2019)/Movie Title (2019).mkv", Movie)
	if got.Identified || got.WorkKey != UnidentifiedWorkKey {
		t.Errorf("an empty registry identified something: %#v", got)
	}
}

// TestRulesAreNamed: the rule name is written to the asset so Milestone 3 can
// find exactly the rows it may re-resolve. An unnamed rule is a row nobody can
// find again.
func TestRulesAreNamed(t *testing.T) {
	seen := map[string]bool{}
	for _, rule := range Default().Rules() {
		switch {
		case rule.Name == "":
			t.Error("a rule has no name")
		case rule.Match == nil:
			t.Errorf("rule %q has no Match", rule.Name)
		case seen[rule.Name]:
			t.Errorf("rule name %q is registered twice", rule.Name)
		case !strings.HasPrefix(rule.Name, rule.ContentType+"/"):
			t.Errorf("rule %q should be named after its content type %q", rule.Name, rule.ContentType)
		}
		seen[rule.Name] = true
	}
}
