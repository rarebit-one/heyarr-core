package worker

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The follow pipeline's seam, end to end against a FAKE feed adapter (§55, M12).
//
// This is the milestone's central claim made executable one layer up from the
// search job: a followed source polls a feed, each episode becomes a byte-less
// Item and an item-scoped want, and from there it is an ORDINARY want the
// existing acquisition pipeline drives — following = enumerate + project + get
// out of the way (ADR-0057). If the poll could not be driven by a fake feed
// provider, ADR-0058's values-in-values-out interface would have failed at its
// one job.

type followHarness struct {
	db       *sqlite.DB
	queue    *jobs.Queue
	cat      *catalog.Catalog
	reg      *providers.Registry
	feed     *providers.Fake
	sourceID string
	workID   string
	feedRef  string
}

func newFollowHarness(t *testing.T, backfill followed.Backfill) *followHarness {
	return newFollowHarnessOf(t, backfill, followed.TypeTVSeries, "tvdb:12345")
}

func newFollowHarnessOf(
	t *testing.T, backfill followed.Backfill, srcType followed.Type, feedRef string,
) *followHarness {
	t.Helper()
	ctx := t.Context()
	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{
		DB: db, Events: eventLog, PeerName: "test", PeerSite: "test-site",
	})
	if err != nil {
		t.Fatal(err)
	}
	queue, err := jobs.New(jobs.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		t.Fatal(err)
	}

	h := &followHarness{
		db: db, queue: queue, cat: cat,
		workID: "w1", feedRef: feedRef,
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	h.exec(t, `INSERT INTO quality_profiles
		(id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES ('q1', 'living-room', '', '[]', '[]', '[]', 1, ?, ?)`, stamp, stamp)
	h.exec(t, `INSERT INTO works
		(id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES ('w1', 'series', 'series:the-series', 'The Series', 'the series', 2020, '{}', ?, ?)`,
		stamp, stamp)

	// The source is created through the catalog, the same path the follow
	// surface will use — not raw SQL — so the test drives the real projection.
	src, err := cat.CreateFollowSource(ctx, followed.Source{
		WorkID:           h.workID,
		Type:             srcType,
		FeedRef:          h.feedRef,
		QualityProfileID: "q1",
		Monitor:          true,
		Backfill:         backfill,
		Reason:           "Kate watches this",
	})
	if err != nil {
		t.Fatal(err)
	}
	h.sourceID = src.ID

	h.feed = providers.NewFake("fake-feed", providers.CapabilityMetadata)
	h.reg = providers.New(nil)
	if err := h.reg.Register(h.feed); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *followHarness) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := h.db.Writer().Exec(query, args...); err != nil {
		t.Fatalf("seeding (%s): %v", query, err)
	}
}

func (h *followHarness) poll(t *testing.T) error {
	t.Helper()
	payload, err := json.Marshal(followed.PollSourcePayload{SourceID: h.sourceID})
	if err != nil {
		t.Fatal(err)
	}
	handler := PollSourceHandler(h.reg, h.cat, h.queue, slog.New(slog.DiscardHandler))
	return handler(t.Context(), jobs.Job{Type: followed.PollSourceJobType, Payload: payload})
}

// itemWants returns every item-scoped desired item, by item_id, so the test can
// assert one want per episode with the subscription's policy carried onto it.
func (h *followHarness) itemWants(t *testing.T) map[string]struct {
	profile string
	monitor int
	reason  string
} {
	t.Helper()
	rows, err := h.db.Reader().Query(`
		SELECT item_id, quality_profile_id, monitor, reason
		FROM desired_items WHERE scope = 'item'`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]struct {
		profile string
		monitor int
		reason  string
	}{}
	for rows.Next() {
		var itemID, profile, reason string
		var monitor int
		if err := rows.Scan(&itemID, &profile, &monitor, &reason); err != nil {
			t.Fatal(err)
		}
		out[itemID] = struct {
			profile string
			monitor int
			reason  string
		}{profile, monitor, reason}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func episode(key, title string, published time.Time) followed.FeedItem {
	return followed.FeedItem{
		Key: key, Title: title, PublishedAt: published,
		Attributes: map[string]string{"season": "2", "episode": key},
	}
}

// THE assertion this slice exists for: a feed of two episodes becomes two
// byte-less Items and two item-scoped wants carrying the subscription's policy.
func TestAPollProjectsAWantPerEpisode(t *testing.T) {
	h := newFollowHarness(t, followed.BackfillFull)
	aired := time.Date(2020, 3, 1, 0, 0, 0, 0, time.UTC)
	h.feed.OfferFeed(h.feedRef,
		episode("S02E01", "The Return", aired),
		episode("S02E02", "The Reckoning", aired.AddDate(0, 0, 7)))

	if err := h.poll(t); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if h.feed.Enumerations() != 1 {
		t.Errorf("the feed adapter was asked %d times, want 1", h.feed.Enumerations())
	}

	items, err := h.cat.ItemsForWork(t.Context(), h.workID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("the poll recorded %d items, want 2", len(items))
	}

	wants := h.itemWants(t)
	if len(wants) != 2 {
		t.Fatalf("the poll projected %d item-scoped wants, want 2", len(wants))
	}
	// Each item has exactly one want, and it carries the subscription's policy —
	// its profile, its monitoring and its reason — which is what makes a want six
	// months later say where it came from (ADR-0057).
	for _, it := range items {
		w, ok := wants[it.ID]
		if !ok {
			t.Fatalf("item %s (%s) has no projected want", it.ItemKey, it.ID)
		}
		if w.profile != "q1" || w.monitor != 1 || w.reason != "Kate watches this" {
			t.Errorf("want for %s carried %+v, want profile q1 / monitor 1 / reason", it.ItemKey, w)
		}
	}

	// Every projected want has a resting acquisition row and a queued
	// reconciliation — it is an ordinary want the pipeline can now advance, not a
	// row the sweep can never touch (the quietest failure this feature has).
	for itemID := range wants {
		var desiredID string
		if err := h.db.Reader().QueryRow(
			`SELECT id FROM desired_items WHERE item_id = ?`, itemID).Scan(&desiredID); err != nil {
			t.Fatal(err)
		}
		if _, err := h.cat.Acquisition(t.Context(), desiredID); err != nil {
			t.Errorf("projected want %s has no acquisition state: %v", desiredID, err)
		}
	}
}

func podcastEpisode(guid, title, enclosure string, published time.Time) followed.FeedItem {
	return followed.FeedItem{
		Key: guid, Title: title, PublishedAt: published,
		Attributes: map[string]string{followed.AttrEnclosureURL: enclosure},
	}
}

// The Phase-2 seam, end to end: a followed podcast is archived WITHOUT a search.
// The feed hands over each episode's enclosure URL, the poll records it as the
// want's direct release and drives it to SELECTED, and the ORDINARY grab fetches
// it over a download client. No indexer is configured — and none is needed, which
// is the whole point of podcast-following being nearly free.
func TestAFollowedPodcastArchivesEachEpisodeDirectly(t *testing.T) {
	h := newFollowHarnessOf(t, followed.BackfillFull,
		followed.TypePodcast, "https://feeds.example.com/show.xml")
	aired := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	enclosureByKey := map[string]string{
		"ep-0001": "https://cdn.example.com/ep1.mp3",
		"ep-0002": "https://cdn.example.com/ep2.mp3",
	}
	h.feed.OfferFeed(h.feedRef,
		podcastEpisode("ep-0001", "Episode One", enclosureByKey["ep-0001"], aired),
		podcastEpisode("ep-0002", "Episode Two", enclosureByKey["ep-0002"], aired.AddDate(0, 0, 7)))

	// A download client so the grab this poll enqueues has somewhere to route,
	// and deliberately NO indexer — a podcast needs none.
	dl := providers.NewFake("fake-http", providers.CapabilityDownload)
	if err := h.reg.Register(dl); err != nil {
		t.Fatal(err)
	}

	if err := h.poll(t); err != nil {
		t.Fatalf("poll: %v", err)
	}

	items, err := h.cat.ItemsForWork(t.Context(), h.workID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("recorded %d items, want 2", len(items))
	}

	desiredFor := func(itemID string) string {
		t.Helper()
		var id string
		if err := h.db.Reader().QueryRow(
			`SELECT id FROM desired_items WHERE item_id = ?`, itemID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	// Each want is already SELECTED — no search ran — with the enclosure as its
	// release source, keyed by the episode's guid.
	for _, it := range items {
		desiredID := desiredFor(it.ID)
		rec, err := h.cat.Acquisition(t.Context(), desiredID)
		if err != nil {
			t.Fatal(err)
		}
		if got := rec.State.Name(); got != "SELECTED" {
			t.Fatalf("episode %s reached %s, want SELECTED (a direct release, no search)", it.ItemKey, got)
		}
		candID, source, err := h.cat.SelectedSource(t.Context(), desiredID)
		if err != nil {
			t.Fatalf("episode %s has no selected release: %v", it.ItemKey, err)
		}
		if candID != it.ItemKey {
			t.Errorf("selected candidate %q, want the item key %q", candID, it.ItemKey)
		}
		if got := source.Reveal(); got != enclosureByKey[it.ItemKey] {
			t.Errorf("release source = %q, want the enclosure %q", got, enclosureByKey[it.ItemKey])
		}
	}

	// The poll enqueued a grab per episode; running them hands each enclosure to
	// the download client and advances the want to QUEUED — the archive actually
	// fetches, over the existing KindHTTP path (a Fake stands in for it here).
	grab := GrabReleaseHandler(h.reg, h.cat, slog.New(slog.DiscardHandler))
	for _, it := range items {
		desiredID := desiredFor(it.ID)
		payload, _ := json.Marshal(acquisition.GrabPayload{DesiredItemID: desiredID})
		if err := grab(t.Context(), jobs.Job{Type: acquisition.GrabJobType, Payload: payload}); err != nil {
			t.Fatalf("grab for %s: %v", it.ItemKey, err)
		}
		rec, err := h.cat.Acquisition(t.Context(), desiredID)
		if err != nil {
			t.Fatal(err)
		}
		if got := rec.State.Name(); got != "QUEUED" {
			t.Errorf("after the grab, %s is %s, want QUEUED", it.ItemKey, got)
		}
	}

	added := map[string]bool{}
	for _, s := range dl.Added() {
		added[s.Reveal()] = true
	}
	for key, url := range enclosureByKey {
		if !added[url] {
			t.Errorf("the download client was never handed %s's enclosure %q", key, url)
		}
	}

	// A re-poll is idempotent (invariant 9): the same episodes reappear in the
	// feed, but no want is re-driven — still two items, and each keeps exactly one
	// selected release rather than a second being stacked on.
	if err := h.poll(t); err != nil {
		t.Fatalf("re-poll: %v", err)
	}
	if again, _ := h.cat.ItemsForWork(t.Context(), h.workID); len(again) != 2 {
		t.Errorf("after a re-poll there are %d items, want 2", len(again))
	}
	for _, it := range items {
		desiredID := desiredFor(it.ID)
		var selectedCount int
		if err := h.db.Reader().QueryRow(
			`SELECT count(*) FROM release_candidates WHERE desired_item_id = ? AND selected = 1`,
			desiredID).Scan(&selectedCount); err != nil {
			t.Fatal(err)
		}
		if selectedCount != 1 {
			t.Errorf("episode %s has %d selected releases after a re-poll, want 1", it.ItemKey, selectedCount)
		}
	}
}

// A poll is re-run — invariant 9 says it will be — and produces no duplicate
// items and no duplicate wants.
func TestPollingIsIdempotent(t *testing.T) {
	h := newFollowHarness(t, followed.BackfillFull)
	aired := time.Date(2020, 3, 1, 0, 0, 0, 0, time.UTC)
	h.feed.OfferFeed(h.feedRef,
		episode("S02E01", "The Return", aired),
		episode("S02E02", "The Reckoning", aired.AddDate(0, 0, 7)))

	for i := 0; i < 3; i++ {
		if err := h.poll(t); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	items, err := h.cat.ItemsForWork(t.Context(), h.workID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Errorf("after three polls there are %d items, want 2 — the upsert is not idempotent", len(items))
	}
	if got := len(h.itemWants(t)); got != 2 {
		t.Errorf("after three polls there are %d wants, want 2 — projection is not idempotent", got)
	}
}

// from_now archives only what the source emitted after it was followed, so a
// back-catalogue episode is recorded as an Item but NOT projected onto a want.
func TestBackfillFromNowLeavesTheBackCatalogueUnwanted(t *testing.T) {
	h := newFollowHarness(t, followed.BackfillFromNow)
	old := time.Now().UTC().AddDate(-1, 0, 0)
	fresh := time.Now().UTC().AddDate(0, 0, 1)
	h.feed.OfferFeed(h.feedRef,
		episode("S01E01", "The Pilot", old),
		episode("S03E10", "The Premiere", fresh))

	if err := h.poll(t); err != nil {
		t.Fatalf("poll: %v", err)
	}
	items, err := h.cat.ItemsForWork(t.Context(), h.workID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("both episodes should be recorded as items, got %d", len(items))
	}
	wants := h.itemWants(t)
	if len(wants) != 1 {
		t.Fatalf("from_now should project only the post-follow episode, got %d wants", len(wants))
	}
}

// The "get out of the way" proof: a projected episode is an ORDINARY want the
// existing search pipeline drives to SELECTED with a grab enqueued — no follow-
// specific code touches acquisition. This is the seam ADR-0057 promises.
func TestAProjectedEpisodeFlowsIntoTheSearchPipeline(t *testing.T) {
	h := newFollowHarness(t, followed.BackfillFull)
	h.feed.OfferFeed(h.feedRef, episode("S02E01", "The Return",
		time.Date(2020, 3, 1, 0, 0, 0, 0, time.UTC)))
	if err := h.poll(t); err != nil {
		t.Fatalf("poll: %v", err)
	}

	var desiredID string
	if err := h.db.Reader().QueryRow(
		`SELECT id FROM desired_items WHERE scope = 'item'`).Scan(&desiredID); err != nil {
		t.Fatal(err)
	}

	// A fake indexer offers a release for the SERIES title — an item-scoped want
	// searches on its work's title, so the existing search context resolves it
	// without knowing an episode is involved.
	indexer := providers.NewFake("fake-indexer", providers.CapabilityIndexer)
	indexer.Offer("The Series", offer("good", 2160, "hevc"))
	if err := h.reg.Register(indexer); err != nil {
		t.Fatal(err)
	}

	payload, _ := json.Marshal(acquisition.SearchPayload{DesiredItemID: desiredID})
	search := SearchHandler(h.reg, h.cat, h.queue, slog.New(slog.DiscardHandler))
	if err := search(t.Context(), jobs.Job{Type: acquisition.SearchJobType, Payload: payload}); err != nil {
		t.Fatalf("search: %v", err)
	}

	rec, err := h.cat.Acquisition(context.Background(), desiredID)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.State.Name(); got != "SELECTED" {
		t.Fatalf("the projected episode reached %s, want SELECTED — the pipeline did not drive it", got)
	}
	sel, err := h.cat.SelectedCandidate(t.Context(), desiredID)
	if err != nil {
		t.Fatal(err)
	}
	if sel.CandidateID != "good" {
		t.Errorf("selected %s, want good", sel.CandidateID)
	}
}
