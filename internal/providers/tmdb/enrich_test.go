package tmdb

import (
	"context"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The enrich half of the adapter, driven through the same replayed corpus
// (ADR-0026): every test here speaks HTTP to the real client, and an unmatched
// request is a 599 — so a test that passes also proves which calls were made.

// A TMDB client IS a providers.EnrichProvider, so the enrich worker routes a held
// film or series to it by capability exactly as it routes a book to Open Library.
func TestClientSatisfiesEnrichProvider(t *testing.T) {
	var _ providers.EnrichProvider = (*Client)(nil)
}

func TestServesContentType(t *testing.T) {
	c := newClient(t, "http://example.invalid")
	for _, ct := range []string{"movie", "series", " Movie "} {
		if !c.ServesContentType(ct) {
			t.Errorf("tmdb must enrich %q", ct)
		}
	}
	for _, ct := range []string{"music", "book", "game", "document", ""} {
		if c.ServesContentType(ct) {
			t.Errorf("tmdb must not claim %q — that is another adapter's Work", ct)
		}
	}
}

func enrichClient(t *testing.T) *Client {
	t.Helper()
	srv := loadCorpus(t).Server()
	t.Cleanup(srv.Close)
	c := newClient(t, srv.URL)
	c.http = srv.Client()
	return c
}

// A held film with only a title and a year: the search ranks a same-named 1999
// decoy first, and the client must choose the 2016 film the library holds by
// validating the year — then fetch its detail for the ids, artwork and genres.
func TestEnrichMovieByTitleValidatesTheYear(t *testing.T) {
	c := enrichClient(t)
	res, found, err := c.Enrich(context.Background(), providers.EnrichQuery{
		ContentType: "movie", Title: "A Held Movie", Year: 2016,
	})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if !found {
		t.Fatal("the held film should match")
	}
	if res.ExternalIDs["tmdb"] != "700" {
		t.Errorf("tmdb id = %q, want 700 (the 2016 film, not the 1999 decoy ranked first)", res.ExternalIDs["tmdb"])
	}
	if res.ExternalIDs["imdb"] != "tt0700700" {
		t.Errorf("imdb id = %q, want tt0700700 from the appended external_ids", res.ExternalIDs["imdb"])
	}
	if _, has := res.ExternalIDs["tvdb"]; has {
		t.Error("a film carries no tvdb id")
	}
	if got := res.CoverURL.Reveal(); got != "https://image.tmdb.org/t/p/w780/held-poster.jpg" {
		t.Errorf("poster = %q, want the w780 CDN link to the film's own poster", got)
	}
	if got := res.BackdropURL.Reveal(); got != "https://image.tmdb.org/t/p/w1280/held-backdrop.jpg" {
		t.Errorf("backdrop = %q, want the w1280 CDN link to the film's own backdrop", got)
	}
	if res.Overview != "A film used to exercise enrichment." {
		t.Errorf("overview = %q", res.Overview)
	}
	if len(res.Genres) != 2 || res.Genres[0] != "Science Fiction" || res.Genres[1] != "Drama" {
		t.Errorf("genres = %v, want the detail's two genre names in order", res.Genres)
	}
	if res.Year != 2016 {
		t.Errorf("year = %d, want 2016", res.Year)
	}
	if res.Confidence != 1 {
		t.Errorf("confidence = %v, want 1 for an exact title and year", res.Confidence)
	}
	// A video Work is decorated, never renamed: no correction is offered, so the
	// worker's ADR-0088 path cannot fire on it.
	if res.Title != "" || res.Author != "" {
		t.Errorf("a video match must offer no display correction, got title=%q author=%q", res.Title, res.Author)
	}
}

// With no year on the Work both same-named hits pass validation; TMDB's own
// order then decides, and the 1999 decoy it ranked first wins. That is the
// honest answer without a year — the test pins that the adapter does not
// invent a preference it has no basis for.
func TestEnrichMovieWithoutYearTakesTheServiceOrder(t *testing.T) {
	c := enrichClient(t)
	// The corpus has no detail fixture for the decoy, so a search that picked
	// it surfaces as a 599 transport error here — which is exactly how this
	// test tells the two apart.
	_, found, err := c.Enrich(context.Background(), providers.EnrichQuery{ContentType: "movie", Title: "A Held Movie"})
	if err == nil && found {
		t.Fatal("without a year the service-ranked decoy (701) must be chosen, and the corpus has no detail for it")
	}
	if err == nil {
		t.Fatal("expected the decoy's missing detail to surface as an error, got found=false")
	}
}

// A year more than one apart refuses every hit: the 1980 film by this name is
// neither the 1999 nor the 2016 one, and a stranger's poster is worse than none.
func TestEnrichRefusesAYearMismatch(t *testing.T) {
	c := enrichClient(t)
	_, found, err := c.Enrich(context.Background(), providers.EnrichQuery{
		ContentType: "movie", Title: "A Held Movie", Year: 1980,
	})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if found {
		t.Fatal("no hit within a year of 1980 — must report nothing matched")
	}
}

// A one-year slip is accepted with a small penalty: a December premiere and a
// January wide release are the same film.
func TestEnrichToleratesAOneYearSlip(t *testing.T) {
	c := enrichClient(t)
	res, found, err := c.Enrich(context.Background(), providers.EnrichQuery{
		ContentType: "movie", Title: "A Held Movie", Year: 2017,
	})
	if err != nil || !found {
		t.Fatalf("enrich: found=%v err=%v", found, err)
	}
	if res.ExternalIDs["tmdb"] != "700" {
		t.Errorf("tmdb id = %q, want 700", res.ExternalIDs["tmdb"])
	}
	if res.Confidence >= 1 || res.Confidence < enrichMinMatch {
		t.Errorf("confidence = %v, want 1 less the slip penalty", res.Confidence)
	}
}

// TMDB answers something for almost any string; a hit sharing no words with the
// query is refused rather than attached.
func TestEnrichRefusesAWeakTitleMatch(t *testing.T) {
	c := enrichClient(t)
	_, found, err := c.Enrich(context.Background(), providers.EnrichQuery{
		ContentType: "movie", Title: "A Weak Match", Year: 2016,
	})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if found {
		t.Fatal("a hit with no title words in common must be refused")
	}
}

func TestEnrichNothingMatchedIsNotAnError(t *testing.T) {
	c := enrichClient(t)
	_, found, err := c.Enrich(context.Background(), providers.EnrichQuery{
		ContentType: "movie", Title: "Totally Unknown Film", Year: 2016,
	})
	if err != nil {
		t.Fatalf("an empty result is the modelled outcome, not an error: %v", err)
	}
	if found {
		t.Fatal("nothing matched")
	}
}

// A Work already carrying a tmdb id is fetched by it: the corpus holds no search
// fixture that could have answered a title this misspelt, so a pass proves the
// search was never made.
func TestEnrichByRecordedTMDBIDSkipsTheSearch(t *testing.T) {
	c := enrichClient(t)
	res, found, err := c.Enrich(context.Background(), providers.EnrichQuery{
		ContentType: "movie", Title: "a hld mvie, misspelt by ingest", Year: 1900,
		ExternalIDs: map[string]string{"tmdb": "700"},
	})
	if err != nil || !found {
		t.Fatalf("enrich: found=%v err=%v", found, err)
	}
	if res.ExternalIDs["tmdb"] != "700" || res.Confidence != 1 {
		t.Errorf("result = %+v, want the keyed film at confidence 1", res)
	}
}

// A recorded imdb id resolves through /find, then the detail — no title search.
func TestEnrichByIMDBIDResolvesThroughFind(t *testing.T) {
	c := enrichClient(t)
	res, found, err := c.Enrich(context.Background(), providers.EnrichQuery{
		ContentType: "movie", Title: "not a title the corpus can search",
		ExternalIDs: map[string]string{"imdb": "tt0700700"},
	})
	if err != nil || !found {
		t.Fatalf("enrich: found=%v err=%v", found, err)
	}
	if res.ExternalIDs["tmdb"] != "700" {
		t.Errorf("tmdb id = %q, want 700 via /find", res.ExternalIDs["tmdb"])
	}
}

// A tmdb id that no longer resolves is "no such title", which the worker backs
// off on — not a transport error it would retry forever.
func TestEnrichTreatsAVanishedIDAsNotFound(t *testing.T) {
	c := enrichClient(t)
	_, found, err := c.Enrich(context.Background(), providers.EnrichQuery{
		ContentType: "movie", Title: "A Held Movie", ExternalIDs: map[string]string{"tmdb": "4040"},
	})
	if err != nil {
		t.Fatalf("a 404 is the modelled outcome: %v", err)
	}
	if found {
		t.Fatal("a vanished id matches nothing")
	}
}

// A held series: the tv spelling of every field, imdb ONLY under external_ids,
// tvdb as a number, and a null backdrop that stays empty rather than being
// replaced by the poster.
func TestEnrichSeriesByTitle(t *testing.T) {
	c := enrichClient(t)
	res, found, err := c.Enrich(context.Background(), providers.EnrichQuery{
		ContentType: "series", Title: "A Held Series", Year: 2011,
	})
	if err != nil || !found {
		t.Fatalf("enrich: found=%v err=%v", found, err)
	}
	if res.ExternalIDs["tmdb"] != "900" || res.ExternalIDs["imdb"] != "tt0900900" || res.ExternalIDs["tvdb"] != "123456" {
		t.Errorf("ids = %v, want tmdb 900, imdb tt0900900, tvdb 123456", res.ExternalIDs)
	}
	if got := res.CoverURL.Reveal(); got != "https://image.tmdb.org/t/p/w780/series-poster.jpg" {
		t.Errorf("poster = %q", got)
	}
	if got := res.BackdropURL.Reveal(); got != "" {
		t.Errorf("backdrop = %q, want empty — the service has none, and a poster must not stand in", got)
	}
	if len(res.Genres) != 1 || res.Genres[0] != "Sci-Fi & Fantasy" {
		t.Errorf("genres = %v", res.Genres)
	}
	if res.Year != 2011 {
		t.Errorf("year = %d", res.Year)
	}
}

// A series followed through TVDB carries the tvdb id its poll recorded; /find
// turns it into the TMDB series without a title search.
func TestEnrichSeriesByTVDBIDResolvesThroughFind(t *testing.T) {
	c := enrichClient(t)
	res, found, err := c.Enrich(context.Background(), providers.EnrichQuery{
		ContentType: "series", Title: "not searchable in the corpus",
		ExternalIDs: map[string]string{"tvdb": "123456"},
	})
	if err != nil || !found {
		t.Fatalf("enrich: found=%v err=%v", found, err)
	}
	if res.ExternalIDs["tmdb"] != "900" {
		t.Errorf("tmdb id = %q, want 900 via /find", res.ExternalIDs["tmdb"])
	}
}

// A film's tvdb id is not a key TMDB's /find knows for movies, so it is ignored
// and the title search runs instead.
func TestEnrichMovieIgnoresATVDBID(t *testing.T) {
	c := enrichClient(t)
	res, found, err := c.Enrich(context.Background(), providers.EnrichQuery{
		ContentType: "movie", Title: "A Held Movie", Year: 2016,
		ExternalIDs: map[string]string{"tvdb": "123456"},
	})
	if err != nil || !found {
		t.Fatalf("enrich: found=%v err=%v", found, err)
	}
	if res.ExternalIDs["tmdb"] != "700" {
		t.Errorf("tmdb id = %q, want 700 from the title search", res.ExternalIDs["tmdb"])
	}
}

// Another adapter's Work is the modelled empty result, and costs no request.
func TestEnrichIgnoresOtherContentTypes(t *testing.T) {
	c := newClient(t, "http://example.invalid")
	for _, ct := range []string{"book", "music", ""} {
		_, found, err := c.Enrich(context.Background(), providers.EnrichQuery{ContentType: ct, Title: "Anything"})
		if err != nil || found {
			t.Errorf("%q: found=%v err=%v, want the modelled empty result with no call", ct, found, err)
		}
	}
}

func TestScoreHit(t *testing.T) {
	cases := []struct {
		name            string
		query           string
		qYear           int
		title, original string
		hYear           int
		wantOK          bool
	}{
		{"exact", "Arrival", 2016, "Arrival", "Arrival", 2016, true},
		{"exact, no years", "Arrival", 0, "Arrival", "", 0, true},
		{"query contained in hit", "Alien", 1979, "Alien: Covenant", "", 1979, true},
		{"original title matches when the local one does not", "Un Film Tenu", 2016, "A Held Movie", "Un Film Tenu", 2016, true},
		{"one year off", "Arrival", 2017, "Arrival", "", 2016, true},
		{"two years off", "Arrival", 2018, "Arrival", "", 2016, false},
		{"hit year unknown", "Arrival", 2016, "Arrival", "", 0, true},
		{"no shared words", "Arrival", 2016, "Departure", "", 2016, false},
		{"half the words", "The Long Good Friday", 1980, "Good Friday Agreement", "", 1980, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := scoreHit(tc.query, tc.qYear, tc.title, tc.original, tc.hYear)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
		})
	}
}
