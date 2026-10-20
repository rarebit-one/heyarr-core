package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// gameWant creates a game want resting in MISSING. A non-empty gog names the
// game's GOG gamename as the Work's external id, the way an owned-library sync
// records it.
func (h *beatHarness) gameWant(t *testing.T, id, contentType, gog string) string {
	t.Helper()
	work := "w-" + id
	h.exec(t, `INSERT INTO works
		(id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES (?, ?, ?, 'Some Game', 'some game', 0, '{}', ?, ?)`,
		work, contentType, contentType+":"+id, beatStamp, beatStamp)
	h.exec(t, `INSERT INTO desired_items
		(id, scope, work_id, edition_id, quality_profile_id, monitor, reason, created_at, updated_at)
		VALUES (?, 'work', ?, NULL, 'q1', 1, '', ?, ?)`, id, work, beatStamp, beatStamp)
	if _, err := h.cat.StartAcquisition(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if gog != "" {
		h.exec(t, `INSERT INTO external_ids (id, entity_type, entity_id, source, value)
			VALUES (?, 'work', ?, 'gog', ?)`, "x-"+id, work, gog)
	}
	return id
}

func (h *beatHarness) storeScheduler() *storeScheduler {
	return newStoreScheduler(h.cat, h.queue, h.clock, discard())
}

func (h *beatHarness) grabJobs(t *testing.T) int {
	t.Helper()
	var n int
	if err := h.db.Reader().QueryRow(
		`SELECT count(*) FROM jobs WHERE type = ?`, acquisition.GrabJobType).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (h *beatHarness) phase(t *testing.T, id string) string {
	t.Helper()
	var p string
	if err := h.db.Reader().QueryRow(
		`SELECT phase FROM acquisition_state WHERE desired_item_id = ?`, id).Scan(&p); err != nil {
		t.Fatal(err)
	}
	return p
}

// A game want whose Work names its GOG game is fetched from the store without
// anyone asking: the beat records the store's package as the want's release —
// with the store's provenance, not a feed's — and hands it to a download client.
func TestAGameWantIsFetchedFromItsStore(t *testing.T) {
	t.Parallel()
	h := newBeatHarness(t)
	id := h.gameWant(t, "g1", "game", "some_game")

	started, err := h.storeScheduler().pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if started != 1 || h.grabJobs(t) != 1 {
		t.Fatalf("started %d, grab jobs %d; want one grab for the game", started, h.grabJobs(t))
	}
	if got := h.phase(t, id); got != string(acquisition.PhaseSelected) {
		t.Errorf("phase = %q, want %q", got, acquisition.PhaseSelected)
	}
	var provider, candidate, evaluation string
	if err := h.db.Reader().QueryRow(`SELECT provider, candidate_id, evaluation
		FROM release_candidates WHERE desired_item_id = ? AND selected = 1`, id).
		Scan(&provider, &candidate, &evaluation); err != nil {
		t.Fatal(err)
	}
	if provider != "gog" || candidate != "gog:some_game@"+id {
		t.Errorf("selected release = %s %s, want gog gog:some_game@%s (one release per want)", provider, candidate, id)
	}
	if !strings.Contains(evaluation, "release/store-owned") {
		t.Errorf("evaluation %s does not name the store provenance", evaluation)
	}

	// The fetch is in flight: a second pass starts nothing.
	again, err := h.storeScheduler().pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 || h.grabJobs(t) != 1 {
		t.Errorf("a second pass started %d more (grab jobs %d); a game is fetched once", again, h.grabJobs(t))
	}
}

// Only a game, only one its Work names on a store, and only while it is
// unsatisfied. A movie with a stray gog id is not a store fetch, and a game
// with no store id has nothing a store could be asked for.
func TestOnlyUnsatisfiedGamesNamedOnAStoreAreFetched(t *testing.T) {
	t.Parallel()
	h := newBeatHarness(t)
	h.gameWant(t, "movie", "movie", "some_game")
	h.gameWant(t, "unnamed", "game", "")
	h.gameWant(t, "held", "game", "held_game")
	h.satisfy(t, "held")
	h.gameWant(t, "bad", "game", "Not.A_Name")

	started, err := h.storeScheduler().pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if started != 0 || h.grabJobs(t) != 0 {
		t.Errorf("started %d (grab jobs %d); none of these is a store fetch", started, h.grabJobs(t))
	}
}

// Two controllers' passes at the same instant start one fetch between them: a
// game is tens of gigabytes, and the claim is a compare-and-set.
func TestTwoStoreSchedulersStartOneFetch(t *testing.T) {
	t.Parallel()
	h := newBeatHarness(t)
	h.gameWant(t, "g1", "game", "some_game")

	a, err := h.storeScheduler().pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.storeScheduler().pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if a+b != 1 {
		t.Errorf("passes started %d and %d; want exactly one fetch", a, b)
	}
}

// A fetch that did not leave the want satisfied backs off: not retried on the
// next pass, retried once the delay has passed, and the delay widens.
func TestAFailedStoreFetchBacksOff(t *testing.T) {
	t.Parallel()
	h := newBeatHarness(t)
	id := h.gameWant(t, "g1", "game", "some_game")
	if n, err := h.storeScheduler().pass(t.Context()); err != nil || n != 1 {
		t.Fatalf("first pass started %d (%v)", n, err)
	}
	// The fetch failed and the want came back to rest, unsatisfied.
	h.exec(t, `UPDATE acquisition_state SET phase = 'idle', managed = 0 WHERE desired_item_id = ?`, id)

	if n, _ := h.storeScheduler().pass(t.Context()); n != 0 {
		t.Fatalf("retried at once (%d); a failed fetch must back off", n)
	}
	first := acquisition.StoreFetches().Delay(0)
	h.clock.advance(first + time.Minute)
	if n, _ := h.storeScheduler().pass(t.Context()); n != 1 {
		t.Fatalf("not retried after %s", first)
	}

	var fruitless int
	var next string
	if err := h.db.Reader().QueryRow(`SELECT fruitless, next_attempt_at FROM store_fetch_schedule
		WHERE desired_item_id = ?`, id).Scan(&fruitless, &next); err != nil {
		t.Fatal(err)
	}
	if fruitless != 2 {
		t.Errorf("fruitless = %d after two attempts", fruitless)
	}
	nextAt, err := time.Parse(time.RFC3339Nano, next)
	if err != nil {
		t.Fatal(err)
	}
	if gap := nextAt.Sub(h.clock.Now()); gap <= first {
		t.Errorf("the second delay %s did not widen past the first %s", gap, first)
	}
}

// Two wants for one game (two profiles) share one fetch: one transfer, one
// ingest, both satisfied by it. Starting two would hand the second want a
// transfer it could never be reconciled with.
func TestTwoWantsForOneGameStartOneFetch(t *testing.T) {
	t.Parallel()
	h := newBeatHarness(t)
	h.gameWant(t, "g1", "game", "some_game")
	h.exec(t, `INSERT INTO quality_profiles
		(id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES ('q2', 'archival', '', '[]', '[]', '[]', 1, ?, ?)`, beatStamp, beatStamp)
	h.exec(t, `INSERT INTO desired_items
		(id, scope, work_id, edition_id, quality_profile_id, monitor, reason, created_at, updated_at)
		VALUES ('g2', 'work', 'w-g1', NULL, 'q2', 1, '', ?, ?)`, beatStamp, beatStamp)
	if _, err := h.cat.StartAcquisition(t.Context(), "g2"); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if _, err := h.storeScheduler().pass(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if n := h.grabJobs(t); n != 1 {
		t.Errorf("%d grabs for one game wanted twice, want one", n)
	}
}

// A selection whose grab never reached the queue — a shutdown between the two
// writes — is found and its grab re-queued once it has sat past the grace,
// rather than left SELECTED forever.
func TestAStrandedStoreSelectionIsGrabbed(t *testing.T) {
	t.Parallel()
	h := newBeatHarness(t)
	id := h.gameWant(t, "g1", "game", "some_game")
	if n, err := h.storeScheduler().pass(t.Context()); err != nil || n != 1 {
		t.Fatalf("first pass started %d (%v)", n, err)
	}
	// The grab was lost.
	h.exec(t, `DELETE FROM jobs WHERE type = ?`, acquisition.GrabJobType)

	age := func(d time.Duration) {
		h.exec(t, `UPDATE acquisition_state SET phase_entered_at = ? WHERE desired_item_id = ?`,
			sqlite.FormatTimestamp(h.clock.Now().Add(-d)), id)
	}
	age(time.Minute)
	if n, _ := h.storeScheduler().pass(t.Context()); n != 0 {
		t.Fatalf("re-grabbed within the grace (%d)", n)
	}
	age(strandedGrace + time.Minute)
	if n, err := h.storeScheduler().pass(t.Context()); err != nil || n != 1 {
		t.Fatalf("stranded selection not re-grabbed: started %d (%v)", n, err)
	}
	if h.grabJobs(t) != 1 || h.phase(t, id) != string(acquisition.PhaseSelected) {
		t.Errorf("grab jobs %d, phase %s", h.grabJobs(t), h.phase(t, id))
	}
}

// The backoff is the game's: after a failed fetch for one want, a second
// profile's want for the same game — which has no schedule row of its own —
// waits out the same backoff instead of refetching on the next pass.
func TestABackingOffGameIsNotRefetchedForASiblingWant(t *testing.T) {
	t.Parallel()
	h := newBeatHarness(t)
	id := h.gameWant(t, "g1", "game", "some_game")
	h.exec(t, `INSERT INTO quality_profiles
		(id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES ('q2', 'archival', '', '[]', '[]', '[]', 1, ?, ?)`, beatStamp, beatStamp)
	h.exec(t, `INSERT INTO desired_items
		(id, scope, work_id, edition_id, quality_profile_id, monitor, reason, created_at, updated_at)
		VALUES ('g2', 'work', 'w-g1', NULL, 'q2', 1, '', ?, ?)`, beatStamp, beatStamp)
	if _, err := h.cat.StartAcquisition(t.Context(), "g2"); err != nil {
		t.Fatal(err)
	}
	if n, err := h.storeScheduler().pass(t.Context()); err != nil || n != 1 {
		t.Fatalf("first pass started %d (%v)", n, err)
	}
	// The fetch failed; both wants are resting again.
	h.exec(t, `UPDATE acquisition_state SET phase = 'idle', managed = 0 WHERE desired_item_id = ?`, id)
	h.exec(t, `DELETE FROM jobs`)
	h.exec(t, `UPDATE acquisition_state SET phase = 'idle', managed = 0`)

	if n, _ := h.storeScheduler().pass(t.Context()); n != 0 {
		t.Errorf("the game was refetched at once for its other want (%d)", n)
	}
}

// Recovered grabs and new fetches share the per-pass bound.
func TestRecoveredAndNewFetchesShareThePassBound(t *testing.T) {
	t.Parallel()
	h := newBeatHarness(t)
	for i := range 2 * storeBatchLimit {
		h.gameWant(t, "g"+string(rune('a'+i)), "game", "game_"+string(rune('a'+i)))
	}
	if _, err := h.storeScheduler().pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Every grab that pass queued was lost, and has sat past the grace.
	h.exec(t, `DELETE FROM jobs`)
	h.exec(t, `UPDATE acquisition_state SET phase_entered_at = ? WHERE phase = 'selected'`,
		sqlite.FormatTimestamp(h.clock.Now().Add(-strandedGrace-time.Minute)))

	n, err := h.storeScheduler().pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n > storeBatchLimit {
		t.Errorf("one pass started %d grabs; the bound is %d", n, storeBatchLimit)
	}
}

// A game that already holds bytes that do not satisfy it — scanned installer
// files with no verified package — is still fetched from its store: the
// verified package is exactly what it lacks.
func TestAGameHoldingUnverifiedBytesIsStillFetched(t *testing.T) {
	t.Parallel()
	h := newBeatHarness(t)
	id := h.gameWant(t, "g1", "game", "some_game")
	h.exec(t, `UPDATE acquisition_state SET managed = 1, content = 'not_satisfied' WHERE desired_item_id = ?`, id)

	if n, err := h.storeScheduler().pass(t.Context()); err != nil || n != 1 {
		t.Fatalf("started %d (%v); a held-but-unverified game must still be fetched", n, err)
	}
	if got := h.phase(t, id); got != string(acquisition.PhaseSelected) {
		t.Errorf("phase = %q, want selected", got)
	}
}

// A game wanted under several profiles takes one slot of a pass, not one per
// want: a backfill of games each wanted twice still starts a full batch of games.
func TestEachGameTakesOneSlotOfAPass(t *testing.T) {
	t.Parallel()
	h := newBeatHarness(t)
	h.exec(t, `INSERT INTO quality_profiles
		(id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES ('q2', 'archival', '', '[]', '[]', '[]', 1, ?, ?)`, beatStamp, beatStamp)
	for i := range storeBatchLimit {
		id := "g" + string(rune('a'+i))
		h.gameWant(t, id, "game", "game_"+string(rune('a'+i)))
		second := id + "-2"
		h.exec(t, `INSERT INTO desired_items
			(id, scope, work_id, edition_id, quality_profile_id, monitor, reason, created_at, updated_at)
			VALUES (?, 'work', ?, NULL, 'q2', 1, '', ?, ?)`, second, "w-"+id, beatStamp, beatStamp)
		if _, err := h.cat.StartAcquisition(t.Context(), second); err != nil {
			t.Fatal(err)
		}
	}
	n, err := h.storeScheduler().pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n != storeBatchLimit {
		t.Errorf("one pass started %d games, want a full batch of %d", n, storeBatchLimit)
	}
}
