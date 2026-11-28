package worker

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
)

// The season fallback (#938) as the search job runs it.

// seriesWants turns the search harness's work into a series and its want into
// the S01E01 episode want, then adds item wants for episodes 2..n. It returns
// the want ids by item_key.
func (h *searchHarness) seriesWants(t *testing.T, episodes int) map[string]string {
	t.Helper()
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	h.exec(t, `UPDATE works SET content_type = 'series', work_key = 'series:arrival:2016' WHERE id = 'w1'`)
	wants := map[string]string{}
	for e := 1; e <= episodes; e++ {
		key := fmt.Sprintf("S01E%02d", e)
		itemID := "it-" + strings.ToLower(key)
		h.exec(t, `INSERT INTO items (id, work_id, item_key, title, attributes, created_at, updated_at)
			VALUES (?, 'w1', ?, ?, ?, ?, ?)`,
			itemID, key, key, fmt.Sprintf(`{"season":"1","episode":"%d"}`, e), stamp, stamp)
		if e == 1 {
			h.exec(t, `UPDATE desired_items SET scope = 'item', item_id = ? WHERE id = ?`, itemID, h.want)
			wants[key] = h.want
			continue
		}
		id := uuid.Must(uuid.NewV7()).String()
		h.exec(t, `INSERT INTO desired_items
			(id, scope, work_id, item_id, quality_profile_id, monitor, reason, created_at, updated_at)
			VALUES (?, 'item', 'w1', ?, 'q1', 1, '', ?, ?)`, id, itemID, stamp, stamp)
		if _, err := h.cat.StartAcquisition(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		wants[key] = id
	}
	return wants
}

func (h *searchHarness) runFor(t *testing.T, wantID string) {
	t.Helper()
	payload, err := json.Marshal(acquisition.SearchPayload{DesiredItemID: wantID})
	if err != nil {
		t.Fatal(err)
	}
	handler := SearchHandler(h.reg, h.cat, h.queue, slog.New(slog.DiscardHandler))
	if err := handler(t.Context(), jobs.Job{Type: acquisition.SearchJobType, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func (h *searchHarness) stateOf(t *testing.T, wantID string) (acquisition.State, string) {
	t.Helper()
	rec, err := h.cat.Acquisition(t.Context(), wantID)
	if err != nil {
		t.Fatal(err)
	}
	return rec.State, rec.Detail
}

// release is a candidate with a real release title, at a resolution.
func release(title string, resolution int64) acquisition.ReleaseCandidate {
	return acquisition.ReleaseCandidate{
		ID: strings.ToLower(title), Title: title, Provider: "fake-indexer",
		Source:     secret.Value("magnet:?xt=urn:btih:" + strings.ToLower(title)),
		Attributes: acquisition.Attributes{policy.AttrResolution: policy.Num(resolution)},
	}
}

// An episode whose own search finds nothing it can take asks for its season, and
// takes the season pack. Only what could contain the episode is kept from the
// season search: the season's other episodes and another season's pack are not
// stored as candidates at all.
func TestAnEpisodeWithNothingTakeableFallsBackToItsSeasonPack(t *testing.T) {
	t.Parallel()
	h := newSearchHarness(t)
	h.seriesWants(t, 2)

	h.fake.Offer("Arrival", release("Arrival.S01E01.720p.WEB-DL", 720)) // below the 1080 gate
	h.fake.OfferSeason("Arrival", 1,
		release("Arrival.S01.1080p.WEB-DL", 1080),
		release("Arrival.S01E02.1080p.WEB-DL", 1080),
		release("Arrival.S02.1080p.WEB-DL", 1080),
		release("Arrival.S01E01E02.1080p.WEB-DL", 1080),
	)
	h.runFor(t, h.want)

	queries := h.fake.Queries()
	if len(queries) != 2 || !queries[1].IsSeason() || queries[1].Season != 1 {
		t.Fatalf("queries = %+v, want an episode search then a season-1 search", queries)
	}
	if st, detail := h.stateOf(t, h.want); st.Name() != "SELECTED" {
		t.Fatalf("the want is %s (%s), want SELECTED", st.Name(), detail)
	}
	selected, err := h.cat.SelectedCandidate(t.Context(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Title != "Arrival.S01.1080p.WEB-DL" && selected.Title != "Arrival.S01E01E02.1080p.WEB-DL" {
		t.Errorf("selected %q, want the season pack or the two-episode file", selected.Title)
	}
	cands, err := h.cat.CandidatesFor(t.Context(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	stored := map[string]bool{}
	for _, c := range cands {
		stored[c.Title] = true
	}
	for _, dropped := range []string{"Arrival.S01E02.1080p.WEB-DL", "Arrival.S02.1080p.WEB-DL"} {
		if stored[dropped] {
			t.Errorf("%q was stored for an S01E01 want; a season search keeps only what could contain it", dropped)
		}
	}
	if len(cands) != 3 {
		t.Errorf("%d candidates stored, want 3 (the 720p episode, the pack, the two-episode file)", len(cands))
	}
}

// An episode search that found something takeable never asks for the season:
// the pack is a fallback, not a second search on every want.
func TestAnEpisodeWithATakeableReleaseDoesNotSearchItsSeason(t *testing.T) {
	t.Parallel()
	h := newSearchHarness(t)
	h.seriesWants(t, 1)

	h.fake.Offer("Arrival", release("Arrival.S01E01.1080p.WEB-DL", 1080))
	h.fake.OfferSeason("Arrival", 1, release("Arrival.S01.2160p.WEB-DL", 2160))
	h.runFor(t, h.want)

	if n := h.fake.Searches(); n != 1 {
		t.Errorf("%d searches, want 1 — the episode search had a takeable release", n)
	}
	if st, _ := h.stateOf(t, h.want); st.Name() != "SELECTED" {
		t.Errorf("the want is %s, want SELECTED", st.Name())
	}
}

// One pack, one transfer. The second episode want of the season chooses the same
// pack its sibling is already fetching, so it does NOT select it — it rests with
// a detail that names the sibling, and exactly one grab is queued. The sibling's
// ingest links the pack's files to both episodes and reconciliation satisfies it.
func TestASiblingDoesNotSelectAPackAlreadyBeingFetched(t *testing.T) {
	t.Parallel()
	h := newSearchHarness(t)
	wants := h.seriesWants(t, 2)

	h.fake.OfferSeason("Arrival", 1, release("Arrival.S01.1080p.WEB-DL", 1080))
	h.runFor(t, wants["S01E01"])
	if st, detail := h.stateOf(t, wants["S01E01"]); st.Name() != "SELECTED" {
		t.Fatalf("setup: S01E01 is %s (%s), want SELECTED", st.Name(), detail)
	}

	h.runFor(t, wants["S01E02"])
	st, detail := h.stateOf(t, wants["S01E02"])
	if st.Name() != "MISSING" {
		t.Errorf("S01E02 is %s, want MISSING — resting while its sibling fetches the pack", st.Name())
	}
	if !strings.Contains(detail, "already being fetched") || !strings.Contains(detail, wants["S01E01"]) {
		t.Errorf("S01E02's detail %q should say the pack is already being fetched for S01E01", detail)
	}
	if _, err := h.cat.SelectedCandidate(t.Context(), wants["S01E02"]); err == nil {
		t.Error("S01E02 has a selected candidate; the pack must be selected once")
	}

	var grabs int
	if err := h.db.Reader().QueryRow(`SELECT count(*) FROM jobs WHERE type = ?`,
		acquisition.GrabJobType).Scan(&grabs); err != nil {
		t.Fatal(err)
	}
	if grabs != 1 {
		t.Errorf("%d grabs queued, want 1 — one pack, one transfer", grabs)
	}
}

// A sibling at rest holds nothing in flight: its old selection does not keep the
// next episode from taking the pack (it may have failed, or not held this
// episode at all).
func TestASiblingAtRestDoesNotWithholdThePack(t *testing.T) {
	t.Parallel()
	h := newSearchHarness(t)
	wants := h.seriesWants(t, 2)

	h.fake.OfferSeason("Arrival", 1, release("Arrival.S01.1080p.WEB-DL", 1080))
	h.runFor(t, wants["S01E01"])
	h.exec(t, `UPDATE acquisition_state SET phase = 'idle' WHERE desired_item_id = ?`, wants["S01E01"])

	h.runFor(t, wants["S01E02"])
	if st, detail := h.stateOf(t, wants["S01E02"]); st.Name() != "SELECTED" {
		t.Errorf("S01E02 is %s (%s), want SELECTED — its sibling is at rest", st.Name(), detail)
	}
}
