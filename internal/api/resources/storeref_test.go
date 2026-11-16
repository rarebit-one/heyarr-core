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

// The want path's events are published only once its transaction commits
// (#796). A commit that fails after the Work was inserted — here the store_ref's
// row is refused by a trigger, so the whole transaction rolls back — must
// publish nothing: a subscriber that heard content.work.created would go and
// read a Work that never existed.
func TestAWantWhoseTransactionFailsPublishesNothing(t *testing.T) {
	h := newHarness(t)
	h.exec(`INSERT INTO quality_profiles (id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES ('q-owned', 'owned', '', '[]', '[]', '[]', 1, '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z')`)
	h.exec(`CREATE TRIGGER refuse_store_ref BEFORE INSERT ON external_ids
		BEGIN SELECT RAISE(ABORT, 'refused by the test'); END`)

	sub := h.events.Subscribe(16)
	defer sub.Close()

	_, err := h.api.WantContent(context.Background(), resources.WantContentRequest{
		Work: &resources.WorkDescriptor{
			ContentType: "game", Title: "Some Game", StoreRef: "gog:some_game",
		},
		QualityProfile: "owned",
	})
	if err == nil {
		t.Fatal("setup: the want's transaction was expected to fail")
	}
	if n := h.countRows(t, `SELECT count(*) FROM works WHERE content_type = 'game'`); n != 0 {
		t.Fatalf("setup: %d work(s) survived the rolled-back transaction", n)
	}
	select {
	case ev := <-sub.Events():
		t.Fatalf("published %s for a transaction that never committed", ev.Type)
	default:
	}
}

// And a want whose transaction commits still publishes what it emitted, after
// the commit: the Work is readable by the time content.work.created arrives.
func TestAWantPublishesItsWorkEventsAfterCommitting(t *testing.T) {
	h := newHarness(t)
	h.exec(`INSERT INTO quality_profiles (id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES ('q-owned', 'owned', '', '[]', '[]', '[]', 1, '2026-08-01T00:00:00Z', '2026-08-01T00:00:00Z')`)

	sub := h.events.Subscribe(16, "content.work.created", "content.work.updated")
	defer sub.Close()

	if _, err := h.api.WantContent(context.Background(), resources.WantContentRequest{
		Work: &resources.WorkDescriptor{
			ContentType: "game", Title: "Some Game", StoreRef: "gog:some_game",
		},
		QualityProfile: "owned",
	}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for len(got) < 2 {
		select {
		case ev := <-sub.Events():
			got = append(got, ev.Type)
			if n := h.countRows(t, `SELECT count(*) FROM works WHERE id = ?`, ev.SubjectID); n != 1 {
				t.Fatalf("%s arrived before its Work was readable", ev.Type)
			}
		default:
			t.Fatalf("published %v, want content.work.created and content.work.updated", got)
		}
	}
}
