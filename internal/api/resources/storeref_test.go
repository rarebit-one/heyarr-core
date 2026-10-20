package resources_test

import (
	"context"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/api/resources"
)

// A want refused for its own fields (here, a scope that does not exist) is
// refused before anything is written: its store_ref must not be left on the
// Work, where the store fetch would find it and download a game nobody wanted.
func TestARefusedWantRecordsNoStoreRef(t *testing.T) {
	h := newHarness(t)
	h.exec(`INSERT INTO quality_profiles (id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES ('q-owned', 'owned', '', '[]', '[]', '[]', 1, '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z')`)
	h.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES ('w-game', 'game', 'game:some game', 'Some Game', 'some game', NULL, '{}',
			'2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z')`)

	_, err := h.api.WantContent(context.Background(), resources.WantContentRequest{
		Scope: "bogus", WorkID: "w-game", StoreRef: "gog:some_game", QualityProfile: "owned",
	})
	if err == nil {
		t.Fatal("a want with an unknown scope was accepted")
	}
	if n := h.countRows(t, `SELECT count(*) FROM external_ids WHERE source = 'gog'`); n != 0 {
		t.Errorf("the refused want left %d gog id(s) on the work", n)
	}
}

// An edition-scoped want for an edition the Work does not have is refused before
// its store_ref is attached.
func TestAWantForAMissingEditionRecordsNoStoreRef(t *testing.T) {
	h := newHarness(t)
	h.exec(`INSERT INTO quality_profiles (id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES ('q-owned', 'owned', '', '[]', '[]', '[]', 1, '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z')`)
	h.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES ('w-game', 'game', 'game:some game', 'Some Game', 'some game', NULL, '{}',
			'2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z')`)
	_, err := h.api.WantContent(context.Background(), resources.WantContentRequest{
		Scope: "edition", WorkID: "w-game", EditionID: "no-such-edition",
		StoreRef: "gog:some_game", QualityProfile: "owned",
	})
	if err == nil {
		t.Fatal("a want for a missing edition was accepted")
	}
	if n := h.countRows(t, `SELECT count(*) FROM external_ids WHERE source = 'gog'`); n != 0 {
		t.Errorf("the refused want left %d gog id(s) on the work", n)
	}
}
