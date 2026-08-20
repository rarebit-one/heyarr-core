package identification

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

// goldenRow is one corpus entry and everything the identifier made of it.
type goldenRow struct {
	Path      string    `json:"path"`
	Candidate Candidate `json:"candidate"`
}

func goldenFor(t *testing.T, name, libraryType string, paths []string) {
	t.Helper()

	if len(paths) < 40 {
		t.Fatalf("%s: corpus has %d shapes, the acceptance bar is 40", name, len(paths))
	}

	r := Default()
	rows := make([]goldenRow, 0, len(paths))
	for _, p := range paths {
		rows = append(rows, goldenRow{Path: p, Candidate: r.Identify(p, libraryType)})
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rows); err != nil {
		t.Fatalf("encode: %v", err)
	}
	testutil.Golden(t, "testdata/"+name+".json", buf.Bytes())
}

func TestGoldenMovies(t *testing.T) { goldenFor(t, "movies", Movie, movieCases) }
func TestGoldenSeries(t *testing.T) { goldenFor(t, "series", Series, seriesCases) }
func TestGoldenMusic(t *testing.T)  { goldenFor(t, "music", Music, musicCases) }
func TestGoldenBooks(t *testing.T)  { goldenFor(t, "books", Book, bookCases) }

// TestAttributeMapsAreDisjoint guards the split between works.attributes and
// editions.attributes: a fact that lands in both rows is a fact that will
// disagree with itself.
func TestAttributeMapsAreDisjoint(t *testing.T) {
	r := Default()
	for _, group := range []struct {
		libraryType string
		paths       []string
	}{
		{Movie, movieCases},
		{Series, seriesCases},
		{Music, musicCases},
		{Book, bookCases},
	} {
		for _, p := range group.paths {
			c := r.Identify(p, group.libraryType)
			if c.WorkAttributes == nil || c.EditionAttributes == nil {
				t.Errorf("%s: attribute maps must never be nil, got work=%v edition=%v",
					p, c.WorkAttributes, c.EditionAttributes)
				continue
			}
			for k := range c.WorkAttributes {
				if _, dup := c.EditionAttributes[k]; dup {
					t.Errorf("%s: key %q appears in both WorkAttributes and EditionAttributes", p, k)
				}
			}
			assertJSONSafe(t, p, c.WorkAttributes)
			assertJSONSafe(t, p, c.EditionAttributes)
		}
	}
}

// assertJSONSafe rejects anything that will not survive a round trip through
// works.attributes / editions.attributes.
func assertJSONSafe(t *testing.T, path string, attrs map[string]any) {
	t.Helper()
	for k, v := range attrs {
		switch v.(type) {
		case string, int, int64, float64, bool, []int, []string:
		default:
			t.Errorf("%s: attribute %q has non-JSON-safe type %T", path, k, v)
		}
	}
}
