package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedOwned writes the seeded game profile, as controller start would.
func seedOwned(h *apiHarness) {
	h.exec(`INSERT INTO quality_profiles
		(id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES ('q-owned', 'owned', '', '[]', '[]', '[]', 1, ?, ?)`, seedTime, seedTime)
}

// A GOG library listing becomes one game want per owned game, each Work named
// by its GOG id — which is what the store fetch beat looks for (ADR-0120) — and
// a second sync wants nothing twice.
func TestStoresSyncWantsEveryOwnedGameOnce(t *testing.T) {
	h := newAPIHarness(t)
	seedOwned(h)
	listing := filepath.Join(t.TempDir(), "gog.json")
	if err := os.WriteFile(listing, []byte(`[
		{"gamename": "some_game", "title": "Some Game", "product_id": "1"},
		{"gamename": "f1_2020", "title": "F1 2020", "product_id": "2"},
		{"gamename": "Bad.Name", "title": "Not Fetchable", "product_id": "3"}
	]`), 0o600); err != nil {
		t.Fatal(err)
	}

	var first storeSyncResult
	if err := json.Unmarshal([]byte(h.mustRun("stores", "sync", "gog", "--from-file", listing, "--json")), &first); err != nil {
		t.Fatal(err)
	}
	if first.Owned != 3 || len(first.Wanted) != 2 || len(first.Skipped) != 1 {
		t.Fatalf("first sync %+v, want 3 owned, 2 wanted, the malformed one skipped", first)
	}

	for name, title := range map[string]string{"some_game": "Some Game", "f1_2020": "F1 2020"} {
		var contentType, gotTitle, profile string
		if err := h.db.Reader().QueryRow(`
			SELECT w.content_type, w.title, q.name
			FROM external_ids x
			JOIN works w ON w.id = x.entity_id
			JOIN desired_items d ON d.work_id = w.id
			JOIN quality_profiles q ON q.id = d.quality_profile_id
			WHERE x.entity_type = 'work' AND x.source = 'gog' AND x.value = ?`, name).
			Scan(&contentType, &gotTitle, &profile); err != nil {
			t.Fatalf("%s: no wanted game work named by its GOG id: %v", name, err)
		}
		if contentType != "game" || gotTitle != title || profile != "owned" {
			t.Errorf("%s: %s %q under %q, want game %q under owned", name, contentType, gotTitle, profile, title)
		}
	}

	var second storeSyncResult
	if err := json.Unmarshal([]byte(h.mustRun("stores", "sync", "gog", "--from-file", listing, "--json")), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Wanted) != 0 || len(second.AlreadyWanted) != 2 {
		t.Errorf("second sync %+v, want both games already wanted and nothing new", second)
	}
}

// A store id names the game more exactly than a title does: wanting it again
// under a different title converges on the Work the id already names.
func TestAStoreRefFindsItsWorkBeforeTheTitle(t *testing.T) {
	h := newAPIHarness(t)
	seedOwned(h)
	h.mustRun("desired", "add", "The Witcher 3", "--content-type", "game", "--store-ref", "gog:the_witcher_3")
	_, errOut, err := h.run("desired", "add", "The Witcher 3 Wild Hunt GOTY", "--content-type", "game",
		"--store-ref", "gog:the_witcher_3")
	if err == nil {
		t.Fatal("a second want for the same store id under the same profile was accepted")
	}
	if !strings.Contains(errOut+err.Error(), "already wanted") {
		t.Errorf("refusal %q / %v, want it to say the game is already wanted", errOut, err)
	}
	var works int
	if err := h.db.Reader().QueryRow(`SELECT count(*) FROM works WHERE content_type = 'game'`).Scan(&works); err != nil {
		t.Fatal(err)
	}
	if works != 1 {
		t.Errorf("%d game works, want the one the store id names", works)
	}
}

// A store ref is a game's, and well-formed, or it is refused — never stored,
// since the store tool takes it as a pattern.
func TestAStoreRefIsRefusedWhereItCannotMeanAnything(t *testing.T) {
	h := newAPIHarness(t)
	seedOwned(h)
	for _, args := range [][]string{
		{"desired", "add", "Arrival", "--content-type", "movie", "--store-ref", "gog:arrival"},
		{"desired", "add", "Some Game", "--content-type", "game", "--store-ref", "gog:.*"},
		{"desired", "add", "Some Game", "--content-type", "game", "--store-ref", "steam:123"},
		{"desired", "add", "Some Game", "--content-type", "game", "--store-ref", "gog:some_game@other"},
	} {
		if _, _, err := h.run(args...); err == nil {
			t.Errorf("%v was accepted", args[len(args)-1])
		}
	}
	var n int
	if err := h.db.Reader().QueryRow(`SELECT count(*) FROM external_ids WHERE source = 'gog'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d gog ids stored from refused wants", n)
	}
}

// A game already in the catalog is named on its store by --work-id and
// --store-ref together: the ref is recorded on that Work, never silently
// dropped into a want the store fetch could not find.
func TestAStoreRefNamesAnExistingGameWork(t *testing.T) {
	h := newAPIHarness(t)
	seedOwned(h)
	h.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES ('w-game', 'game', 'game:some game', 'Some Game', 'some game', NULL, '{}', ?, ?),
		       ('w-film', 'movie', 'movie:arrival:2016', 'Arrival', 'arrival', 2016, '{}', ?, ?)`,
		seedTime, seedTime, seedTime, seedTime)

	h.mustRun("desired", "add", "--work-id", "w-game", "--store-ref", "gog:some_game", "--quality-profile", "owned")
	var n int
	if err := h.db.Reader().QueryRow(`SELECT count(*) FROM external_ids
		WHERE entity_type = 'work' AND entity_id = 'w-game' AND source = 'gog' AND value = 'some_game'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the existing game's store ref was not recorded (%d rows)", n)
	}
	// Gaining a store id changes what the Work is known as: it emits (invariant 7).
	if err := h.db.Reader().QueryRow(`SELECT count(*) FROM events
		WHERE type = 'content.work.updated' AND subject_type = 'work' AND subject_id = 'w-game'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d content.work.updated events for the attached store ref, want 1", n)
	}

	if _, _, err := h.run("desired", "add", "--work-id", "w-film", "--store-ref", "gog:arrival",
		"--quality-profile", "owned"); err == nil {
		t.Error("a store ref on an existing film was accepted")
	}

	// A Work already named by one product refuses a second; a product already
	// naming one Work refuses a second Work. Neither is silently dropped.
	h.exec(`INSERT INTO quality_profiles
		(id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES ('q-arch', 'archival', '', '[]', '[]', '[]', 1, ?, ?)`, seedTime, seedTime)
	h.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES ('w-game2', 'game', 'game:other game', 'Other Game', 'other game', NULL, '{}', ?, ?)`, seedTime, seedTime)
	for _, args := range [][]string{
		{"--work-id", "w-game", "--store-ref", "gog:another_game"},
		{"--work-id", "w-game2", "--store-ref", "gog:some_game"},
	} {
		if _, _, err := h.run(append([]string{"desired", "add"}, append(args, "--quality-profile", "archival")...)...); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
	if err := h.db.Reader().QueryRow(`SELECT count(*) FROM external_ids WHERE source = 'gog'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d gog ids recorded, want only the first", n)
	}
}

// Two GOG products that normalise to one title are two games: the second is
// refused rather than attached to the first's Work (where only one of them would
// ever be fetched), the rest of the library still syncs, and the command fails
// so the collision is seen.
func TestTwoProductsWithOneTitleAreNotFoldedTogether(t *testing.T) {
	h := newAPIHarness(t)
	seedOwned(h)
	listing := filepath.Join(t.TempDir(), "gog.json")
	if err := os.WriteFile(listing, []byte(`[
		{"gamename": "some_game", "title": "Some Game"},
		{"gamename": "some_game_classic", "title": "Some Game"},
		{"gamename": "other_game", "title": "Other Game"}
	]`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err := h.run("stores", "sync", "gog", "--from-file", listing, "--json")
	if err == nil {
		t.Fatal("a title collision between two products did not fail the sync")
	}
	var res storeSyncResult
	if jerr := json.Unmarshal([]byte(out), &res); jerr != nil {
		t.Fatalf("%v: %s", jerr, out)
	}
	if len(res.Wanted) != 2 || len(res.Failed) != 1 || !strings.HasPrefix(res.Failed[0], "some_game_classic") {
		t.Errorf("sync %+v, want two wanted and the second same-titled product refused", res)
	}
	var n int
	if err := h.db.Reader().QueryRow(`SELECT count(*) FROM external_ids WHERE source = 'gog' AND value = 'some_game_classic'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("the refused product's id was attached anyway")
	}
}

// A sync's profile is checked once, before any game is sent: a misspelled or
// missing profile fails the sync up front instead of as one refusal per game.
func TestStoresSyncRefusesAMissingProfileUpFront(t *testing.T) {
	h := newAPIHarness(t)
	seedOwned(h)
	listing := filepath.Join(t.TempDir(), "gog.json")
	if err := os.WriteFile(listing, []byte(`[{"gamename": "some_game", "title": "Some Game"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := h.run("stores", "sync", "gog", "--from-file", listing, "--quality-profile", "ownd")
	if err == nil || !strings.Contains(err.Error(), "no quality profile") {
		t.Fatalf("err = %v, want the missing profile named", err)
	}
	var n int
	if err := h.db.Reader().QueryRow(`SELECT count(*) FROM desired_items`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d wants created under a missing profile", n)
	}
}

// A profile given by id is used as that profile, not refused game by game.
func TestStoresSyncAcceptsAProfileByID(t *testing.T) {
	h := newAPIHarness(t)
	seedOwned(h)
	listing := filepath.Join(t.TempDir(), "gog.json")
	if err := os.WriteFile(listing, []byte(`[{"gamename": "some_game", "title": "Some Game"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	var res storeSyncResult
	out := h.mustRun("stores", "sync", "gog", "--from-file", listing, "--quality-profile", "q-owned", "--json")
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Wanted) != 1 || len(res.Failed) != 0 {
		t.Errorf("sync by profile id %+v, want the game wanted", res)
	}
}

// A game whose Work was deleted is wanted again by the next sync: the id its
// old Work left behind names nothing and does not block a new Work.
func TestAGameWhoseWorkWasDeletedIsWantedAgain(t *testing.T) {
	h := newAPIHarness(t)
	seedOwned(h)
	h.exec(`INSERT INTO external_ids (id, entity_type, entity_id, source, value)
		VALUES ('x-orphan', 'work', 'w-deleted', 'gog', 'some_game')`)
	listing := filepath.Join(t.TempDir(), "gog.json")
	if err := os.WriteFile(listing, []byte(`[{"gamename": "some_game", "title": "Some Game"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	var res storeSyncResult
	if err := json.Unmarshal([]byte(h.mustRun("stores", "sync", "gog", "--from-file", listing, "--json")), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Wanted) != 1 {
		t.Fatalf("sync %+v, want the game wanted again", res)
	}
	var owner string
	if err := h.db.Reader().QueryRow(`SELECT x.entity_id FROM external_ids x JOIN works w ON w.id = x.entity_id
		WHERE x.source = 'gog' AND x.value = 'some_game'`).Scan(&owner); err != nil {
		t.Fatalf("the game's id does not name a live work: %v", err)
	}
}
