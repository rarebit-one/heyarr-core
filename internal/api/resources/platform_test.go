package resources_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/api/resources"
)

// A ROM want is keyed by its platform and title (ADR-0121): the same title on
// two consoles is two Works, and the key carries the platform so a search (and
// a fan-out sibling) can read it back.
func TestAROMWantIsKeyedByPlatform(t *testing.T) {
	h := newHarness(t)
	h.exec(`INSERT INTO quality_profiles (id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES ('q-rom', 'rom-indexed', '', '[]', '[]', '[]', 1, '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z')`)
	want := func(platform string) error {
		_, err := h.api.WantContent(context.Background(), resources.WantContentRequest{
			Work:           &resources.WorkDescriptor{ContentType: "rom", Title: "Tetris (World)", Platform: platform},
			QualityProfile: "rom-indexed",
		})
		return err
	}
	if err := want("gb"); err != nil {
		t.Fatal(err)
	}
	if err := want("NES"); err != nil {
		t.Fatal(err)
	}
	if n := h.countRows(t, `SELECT count(*) FROM works WHERE content_type = 'rom'
		AND work_key IN ('rom:gb:tetris', 'rom:nes:tetris')`); n != 2 {
		t.Errorf("%d rom works keyed by platform, want 2 (gb and nes)", n)
	}
}

// platform is required on a ROM, must be in the vocabulary, and is refused on
// anything else — all before anything is written.
func TestPlatformIsRequiredOnAROMAndRefusedElsewhere(t *testing.T) {
	h := newHarness(t)
	h.exec(`INSERT INTO quality_profiles (id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES ('q-any', 'everyday', '', '[]', '[]', '[]', 1, '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z')`)
	for name, d := range map[string]resources.WorkDescriptor{
		"a rom with no platform":       {ContentType: "rom", Title: "Okami"},
		"a rom on an unknown platform": {ContentType: "rom", Title: "Okami", Platform: "playstation"},
		"a movie with a platform":      {ContentType: "movie", Title: "Tron", Platform: "snes"},
	} {
		_, err := h.api.WantContent(context.Background(), resources.WantContentRequest{Work: &d, QualityProfile: "everyday"})
		if err == nil || !strings.Contains(err.Error(), "platform") {
			t.Errorf("%s: err = %v, want a refusal that names the platform", name, err)
		}
	}
	if n := h.countRows(t, `SELECT count(*) FROM works`); n != 0 {
		t.Errorf("refused wants left %d work(s) behind", n)
	}
}

// Correcting an existing Work into a rom is refused: its key would name no
// platform, and every search for it would fail (ADR-0121).
func TestAWorkCannotBeCorrectedIntoAROM(t *testing.T) {
	h := newHarness(t)
	h.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES ('w-movie', 'movie', 'movie:tetris', 'Tetris', 'tetris', NULL, '{}',
			'2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z')`)
	resp := h.doStable(http.MethodPatch, "/api/v1/works/w-movie", strings.NewReader(`{"content_type":"rom"}`))
	defer func() { _ = resp.Body.Close() }()
	if body := string(h.body(resp)); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "platform") {
		t.Errorf("status = %d (%s), want a 400 that names the platform", resp.StatusCode, body)
	}
	if n := h.countRows(t, `SELECT count(*) FROM works WHERE content_type = 'rom'`); n != 0 {
		t.Errorf("the work was relabelled as a rom")
	}
}

// platform is refused on a game even when its store_ref names a Work that
// already exists: the request is judged on its own fields, not let through by
// the store-ref fast path.
func TestPlatformIsRefusedOnAGameWithAKnownStoreRef(t *testing.T) {
	h := newHarness(t)
	h.exec(`INSERT INTO quality_profiles (id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES ('q-owned', 'owned', '', '[]', '[]', '[]', 1, '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z')`)
	want := func(platform string) error {
		_, err := h.api.WantContent(context.Background(), resources.WantContentRequest{
			Work: &resources.WorkDescriptor{
				ContentType: "game", Title: "Some Game", StoreRef: "gog:some_game", Platform: platform,
			},
			QualityProfile: "owned",
		})
		return err
	}
	if err := want(""); err != nil {
		t.Fatal(err)
	}
	if err := want("snes"); err == nil || !strings.Contains(err.Error(), "platform") {
		t.Errorf("err = %v, want the platform refused even though the store_ref is known", err)
	}
}
