package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The enrich_work handler against in-memory fakes: what it asks the provider,
// which writes it makes, which bytes it fetches, and how it schedules. The
// database side is covered by the catalog package's own tests; this is the
// routing and the decision logic (ADR-0087, ADR-0088, ADR-0122).

// enrichRecorder is an EnrichRecorder that remembers every write.
type enrichRecorder struct {
	mu        sync.Mutex
	ctx       catalog.DueEnrichWork
	ok        bool
	ids       map[string]string
	meta      []catalog.EnrichedMetadata
	artwork   []catalog.FetchedArtwork
	corrected []string
	scheduled []scheduledEnrich
}

type scheduledEnrich struct {
	fruitless int
	next      time.Time
}

func (r *enrichRecorder) EnrichContext(context.Context, string) (catalog.DueEnrichWork, bool, error) {
	return r.ctx, r.ok, nil
}

func (r *enrichRecorder) WriteWorkExternalID(_ context.Context, _, source, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ids == nil {
		r.ids = map[string]string{}
	}
	r.ids[source] = value
	return nil
}

func (r *enrichRecorder) RecordWorkMetadata(_ context.Context, _ string, meta catalog.EnrichedMetadata, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.meta = append(r.meta, meta)
	return nil
}

func (r *enrichRecorder) RecordFetchedArtwork(_ context.Context, _ string, art catalog.FetchedArtwork, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.artwork = append(r.artwork, art)
	return nil
}

func (r *enrichRecorder) CorrectWorkIdentity(_ context.Context, _, title, author string, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.corrected = append(r.corrected, title+"|"+author)
	return nil
}

func (r *enrichRecorder) RecordEnrichScheduled(_ context.Context, _ string, fruitless int, _, next time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scheduled = append(r.scheduled, scheduledEnrich{fruitless: fruitless, next: next})
	return nil
}

// enrichProvider is a scripted EnrichProvider: it serves the given content
// types and answers every query with one result (or a miss, or an error).
type enrichProvider struct {
	providers.Provider
	serves  []string
	result  providers.EnrichResult
	found   bool
	err     error
	queries []providers.EnrichQuery
}

func (p *enrichProvider) ServesContentType(ct string) bool {
	for _, s := range p.serves {
		if s == ct {
			return true
		}
	}
	return false
}

func (p *enrichProvider) Enrich(_ context.Context, q providers.EnrichQuery) (providers.EnrichResult, bool, error) {
	p.queries = append(p.queries, q)
	return p.result, p.found, p.err
}

// scriptedFetcher serves bytes per URL and counts the fetches: the proof that a
// face the Work already has is never downloaded again.
type scriptedFetcher struct {
	bodies  map[string]string
	fetched []string
}

func (f *scriptedFetcher) Fetch(_ context.Context, u secret.Value) (io.ReadCloser, string, error) {
	url := u.Reveal()
	f.fetched = append(f.fetched, url)
	body, ok := f.bodies[url]
	if !ok {
		return nil, "", errors.New("no such image")
	}
	return io.NopCloser(strings.NewReader(body)), "image/jpeg", nil
}

// hashingStore "adopts" bytes by naming them after their content, so a test can
// see which image became which asset.
type hashingStore struct{ puts int }

func (s *hashingStore) Put(_ context.Context, r io.Reader) (string, int64, error) {
	s.puts++
	b, err := io.ReadAll(r)
	if err != nil {
		return "", 0, err
	}
	return "blake3:" + string(b), int64(len(b)), nil
}

func enrichJob(t *testing.T, workID string) jobs.Job {
	t.Helper()
	p, err := json.Marshal(acquisition.EnrichWorkPayload{WorkID: workID})
	if err != nil {
		t.Fatal(err)
	}
	return jobs.Job{Type: acquisition.EnrichWorkJobType, Payload: p}
}

var enrichNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

const (
	posterURL   = "https://image.example/poster.jpg"
	backdropURL = "https://image.example/backdrop.jpg"
)

func videoResult() providers.EnrichResult {
	return providers.EnrichResult{
		ExternalIDs: map[string]string{"tmdb": "700", "imdb": "tt0700700"},
		CoverURL:    secret.Value(posterURL),
		BackdropURL: secret.Value(backdropURL),
		Overview:    "A film used to exercise enrichment.",
		Genres:      []string{"Drama"},
		Year:        2016,
		Confidence:  1,
	}
}

func tmdbProvider(serves ...string) *enrichProvider {
	return &enrichProvider{Provider: providers.NewFake("tmdb", providers.CapabilityEnrich), serves: serves}
}

func runEnrich(t *testing.T, rec *enrichRecorder, prov *enrichProvider, fetcher *scriptedFetcher) (*hashingStore, error) {
	t.Helper()
	store := &hashingStore{}
	if fetcher.bodies == nil {
		fetcher.bodies = map[string]string{posterURL: "poster-bytes", backdropURL: "backdrop-bytes"}
	}
	h := EnrichHandler(EnrichHandlerOptions{
		Providers: []providers.EnrichProvider{prov},
		Recorder:  rec, Store: store, Fetcher: fetcher,
		Now: func() time.Time { return enrichNow },
	})
	return store, h(context.Background(), enrichJob(t, "w1"))
}

func TestEnrichMovieStoresIDsMetadataPosterAndBackdrop(t *testing.T) {
	rec := &enrichRecorder{ok: true, ctx: catalog.DueEnrichWork{
		WorkID: "w1", ContentType: "movie", Title: "A Held Movie", Year: 2016,
		ExternalIDs: map[string]string{"tvdb": "1"},
	}}
	prov := tmdbProvider("movie", "series")
	prov.result, prov.found = videoResult(), true
	fetcher := &scriptedFetcher{}
	store, err := runEnrich(t, rec, prov, fetcher)
	if err != nil {
		t.Fatal(err)
	}

	// The query carried the Work's recorded ids as strong keys.
	if len(prov.queries) != 1 || prov.queries[0].ExternalIDs["tvdb"] != "1" || prov.queries[0].ContentType != "movie" {
		t.Fatalf("query = %+v", prov.queries)
	}
	if rec.ids["tmdb"] != "700" || rec.ids["imdb"] != "tt0700700" {
		t.Errorf("ids = %v", rec.ids)
	}
	if len(rec.meta) != 1 || rec.meta[0].Source != "tmdb" || rec.meta[0].Overview == "" || rec.meta[0].Year != 2016 {
		t.Errorf("metadata = %+v, want one tmdb-sourced record", rec.meta)
	}
	// Two faces, two kinds, two downloads.
	if len(fetcher.fetched) != 2 || store.puts != 2 {
		t.Fatalf("fetched %v (%d puts), want the poster and the backdrop once each", fetcher.fetched, store.puts)
	}
	if len(rec.artwork) != 2 {
		t.Fatalf("artwork = %+v", rec.artwork)
	}
	kinds := map[string]catalog.FetchedArtwork{}
	for _, a := range rec.artwork {
		kinds[a.Kind] = a
	}
	if p := kinds[catalog.ArtworkPoster]; p.BlobHash != "blake3:poster-bytes" || p.Source != "tmdb" || p.MIME != "image/jpeg" {
		t.Errorf("poster = %+v", p)
	}
	if b := kinds[catalog.ArtworkBackdrop]; b.BlobHash != "blake3:backdrop-bytes" || b.Source != "tmdb" {
		t.Errorf("backdrop = %+v", b)
	}
	// A video match offered no correction, so none was applied.
	if len(rec.corrected) != 0 {
		t.Errorf("corrected = %v, want none", rec.corrected)
	}
	// Fully enriched: the streak resets and the next ask is at the cap.
	if len(rec.scheduled) != 1 || rec.scheduled[0].fruitless != 0 || !rec.scheduled[0].next.Equal(enrichNow.Add(enrichCap)) {
		t.Errorf("scheduled = %+v", rec.scheduled)
	}
}

// A re-run on a Work that already has its poster fetches only the backdrop: no
// repeated download, and the poster the node holds is left exactly as it is.
func TestEnrichFetchesOnlyTheMissingFace(t *testing.T) {
	rec := &enrichRecorder{ok: true, ctx: catalog.DueEnrichWork{
		WorkID: "w1", ContentType: "movie", Title: "A Held Movie", Year: 2016, HasPoster: true,
	}}
	prov := tmdbProvider("movie")
	prov.result, prov.found = videoResult(), true
	fetcher := &scriptedFetcher{}
	if _, err := runEnrich(t, rec, prov, fetcher); err != nil {
		t.Fatal(err)
	}
	if len(fetcher.fetched) != 1 || fetcher.fetched[0] != backdropURL {
		t.Fatalf("fetched %v, want only the backdrop", fetcher.fetched)
	}
	if len(rec.artwork) != 1 || rec.artwork[0].Kind != catalog.ArtworkBackdrop {
		t.Fatalf("artwork = %+v, want one backdrop", rec.artwork)
	}

	// And with both faces present, nothing at all is downloaded; the pass only
	// refreshes ids and metadata and reschedules far out.
	rec = &enrichRecorder{ok: true, ctx: catalog.DueEnrichWork{
		WorkID: "w1", ContentType: "movie", Title: "A Held Movie", HasPoster: true, HasBackdrop: true,
	}}
	fetcher = &scriptedFetcher{}
	if _, err := runEnrich(t, rec, prov, fetcher); err != nil {
		t.Fatal(err)
	}
	if len(fetcher.fetched) != 0 || len(rec.artwork) != 0 {
		t.Fatalf("fetched %v, want nothing — both faces are held", fetcher.fetched)
	}
	if rec.scheduled[0].fruitless != 0 {
		t.Fatalf("scheduled = %+v, want a reset streak", rec.scheduled)
	}
}

// A provider with no backdrop for a film: the poster lands, nothing stands in
// for the backdrop, and the Work is treated as done rather than held fruitless
// for an image the authority does not have.
func TestEnrichWithoutABackdropOfferedIsDone(t *testing.T) {
	res := videoResult()
	res.BackdropURL = ""
	rec := &enrichRecorder{ok: true, ctx: catalog.DueEnrichWork{WorkID: "w1", ContentType: "series", Title: "A Held Series"}}
	prov := tmdbProvider("series")
	prov.result, prov.found = res, true
	if _, err := runEnrich(t, rec, prov, &scriptedFetcher{}); err != nil {
		t.Fatal(err)
	}
	if len(rec.artwork) != 1 || rec.artwork[0].Kind != catalog.ArtworkPoster {
		t.Fatalf("artwork = %+v, want the poster alone — never the poster filed as a backdrop", rec.artwork)
	}
	if rec.scheduled[0].fruitless != 0 {
		t.Fatalf("scheduled = %+v, want done", rec.scheduled)
	}
}

// A backdrop that will not download backs the Work off for it, with the poster
// and ids already kept.
func TestEnrichBacksOffWhenABackdropWillNotFetch(t *testing.T) {
	rec := &enrichRecorder{ok: true, ctx: catalog.DueEnrichWork{WorkID: "w1", ContentType: "movie", Title: "A Held Movie", Fruitless: 1}}
	prov := tmdbProvider("movie")
	prov.result, prov.found = videoResult(), true
	fetcher := &scriptedFetcher{bodies: map[string]string{posterURL: "poster-bytes"}}
	if _, err := runEnrich(t, rec, prov, fetcher); err != nil {
		t.Fatal(err)
	}
	if len(rec.artwork) != 1 || rec.artwork[0].Kind != catalog.ArtworkPoster {
		t.Fatalf("artwork = %+v, want the poster kept", rec.artwork)
	}
	if rec.ids["tmdb"] != "700" {
		t.Fatal("the ids are kept even when an image does not land")
	}
	if len(rec.scheduled) != 1 || rec.scheduled[0].fruitless != 2 || !rec.scheduled[0].next.Equal(enrichNow.Add(enrichDelay(2))) {
		t.Fatalf("scheduled = %+v, want a widened backoff", rec.scheduled)
	}
}

// A book: one face, Open Library's cover, and a confident match corrects the
// display identity (ADR-0088) — the pre-existing behaviour, unchanged by the
// video extension.
func TestEnrichBookStillCorrectsIdentityAndFetchesOneCover(t *testing.T) {
	rec := &enrichRecorder{ok: true, ctx: catalog.DueEnrichWork{WorkID: "w1", ContentType: "book", Title: "noisy filename", Author: "Shelf"}}
	prov := &enrichProvider{
		Provider: providers.NewFake("openlibrary", providers.CapabilityEnrich), serves: []string{"book"}, found: true,
		result: providers.EnrichResult{
			ExternalIDs: map[string]string{"openlibrary": "OL1W"}, CoverURL: secret.Value(posterURL),
			Title: "A Real Title", Author: "A Real Author", Confidence: 0.9, Overview: "About a thing.",
		},
	}
	if _, err := runEnrich(t, rec, prov, &scriptedFetcher{}); err != nil {
		t.Fatal(err)
	}
	if len(rec.corrected) != 1 || rec.corrected[0] != "A Real Title|A Real Author" {
		t.Errorf("corrected = %v", rec.corrected)
	}
	if len(rec.artwork) != 1 || rec.artwork[0].Kind != catalog.ArtworkPoster || rec.artwork[0].Source != "openlibrary" {
		t.Errorf("artwork = %+v", rec.artwork)
	}
	if len(rec.meta) != 1 || rec.meta[0].Source != "openlibrary" || rec.meta[0].Overview != "About a thing." {
		t.Errorf("metadata = %+v, want the overview the provider offered, now kept", rec.meta)
	}
	if rec.scheduled[0].fruitless != 0 {
		t.Errorf("scheduled = %+v, want done — a book owes no backdrop", rec.scheduled)
	}
}

func TestEnrichRoutesByContentTypeAndBacksOffWithNoProvider(t *testing.T) {
	rec := &enrichRecorder{ok: true, ctx: catalog.DueEnrichWork{WorkID: "w1", ContentType: "movie", Title: "A Held Movie"}}
	books := &enrichProvider{Provider: providers.NewFake("openlibrary", providers.CapabilityEnrich), serves: []string{"book"}, found: true, result: videoResult()}
	if _, err := runEnrich(t, rec, books, &scriptedFetcher{}); err != nil {
		t.Fatal(err)
	}
	if len(books.queries) != 0 {
		t.Fatal("a film must not be handed to the book adapter")
	}
	if len(rec.scheduled) != 1 || rec.scheduled[0].fruitless != 1 {
		t.Fatalf("scheduled = %+v, want one fruitless pass — another peer may serve it", rec.scheduled)
	}
}

func TestEnrichNoMatchAndProviderErrorBackOffWithoutFailing(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"no match", nil},
		{"provider error", errors.New("unreachable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &enrichRecorder{ok: true, ctx: catalog.DueEnrichWork{WorkID: "w1", ContentType: "movie", Title: "x", Fruitless: 2}}
			prov := tmdbProvider("movie")
			prov.err = tc.err
			fetcher := &scriptedFetcher{}
			if _, err := runEnrich(t, rec, prov, fetcher); err != nil {
				t.Fatalf("a provider outcome is never a job failure: %v", err)
			}
			if len(rec.ids) != 0 || len(rec.artwork) != 0 || len(fetcher.fetched) != 0 {
				t.Fatal("nothing is written or fetched when nothing matched")
			}
			if len(rec.scheduled) != 1 || rec.scheduled[0].fruitless != 3 {
				t.Fatalf("scheduled = %+v, want the streak advanced", rec.scheduled)
			}
		})
	}
}

func TestEnrichIsANoOpForAWorkThatIsGone(t *testing.T) {
	rec := &enrichRecorder{ok: false}
	prov := tmdbProvider("movie")
	prov.result, prov.found = videoResult(), true
	if _, err := runEnrich(t, rec, prov, &scriptedFetcher{}); err != nil {
		t.Fatal(err)
	}
	if len(prov.queries) != 0 || len(rec.scheduled) != 0 {
		t.Fatal("a Work that is gone costs no call and no schedule row")
	}
}
