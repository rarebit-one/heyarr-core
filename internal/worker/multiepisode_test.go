package worker

import (
	"testing"
)

// A single-file release that holds two episodes ("S01E01E02") satisfies BOTH
// episode wants (#939): its asset is linked to the item the want asserted and to
// the further episode its name spells, so the E02 want is served by the bytes
// already held instead of being searched for forever. An episode the name does
// not spell is left alone.
func TestAMultiEpisodeFileSatisfiesEveryEpisodeItHolds(t *testing.T) {
	t.Parallel()
	h := newIngestHarness(t)
	wants := h.setupSeries(t, 3)

	h.selectAndComplete(t, "Arrival.S01E01E02.1080p.WEB-DL.mkv", []byte("two episodes in one file"), "S01E01")
	if err := h.ingest(t); err != nil {
		t.Fatal(err)
	}

	if n := h.count(t, `SELECT count(*) FROM asset_items ai JOIN items i ON i.id = ai.item_id
		WHERE i.item_key = 'S01E02'`); n != 1 {
		t.Fatalf("the S01E01E02 file is linked to S01E02 %d times, want 1", n)
	}
	for key, managed := range map[string]bool{"S01E01": true, "S01E02": true, "S01E03": false} {
		res, err := h.cat.ReconcileDesired(t.Context(), wants[key])
		if err != nil {
			t.Fatalf("reconciling %s: %v", key, err)
		}
		if res.State.Managed != managed {
			t.Errorf("%s: managed=%v after the S01E01E02 file ingested, want %v", key, res.State.Managed, managed)
		}
	}
}

// A two-episode file inside a season pack is linked to both of its episodes,
// alongside the pack's ordinary one-episode files.
func TestAMultiEpisodeFileInAPackSatisfiesBothEpisodes(t *testing.T) {
	t.Parallel()
	h := newIngestHarness(t)
	wants := h.setupSeries(t, 3)

	h.selectAndCompletePack(t, "Arrival.S01.1080p.WEB-DL", map[string][]byte{
		"Arrival.S01E01E02.1080p.WEB-DL.mkv": []byte("the double opener"),
		"Arrival.S01E03.1080p.WEB-DL.mkv":    []byte("the third episode"),
	})
	if err := h.ingest(t); err != nil {
		t.Fatal(err)
	}

	for key, wantID := range wants {
		res, err := h.cat.ReconcileDesired(t.Context(), wantID)
		if err != nil {
			t.Fatalf("reconciling %s: %v", key, err)
		}
		if !res.State.Managed {
			t.Errorf("%s holds no bytes after a pack holding it ingested", key)
		}
	}
}

// The start-up backfill links multi-episode files acquired before asset_items
// existed, and is idempotent.
func TestBackfillLinksAlreadyHeldMultiEpisodeFiles(t *testing.T) {
	t.Parallel()
	h := newIngestHarness(t)
	wants := h.setupSeries(t, 2)

	h.selectAndComplete(t, "Arrival.S01E01E02.1080p.WEB-DL.mkv", []byte("two episodes in one file"), "S01E01")
	if err := h.ingest(t); err != nil {
		t.Fatal(err)
	}
	// Simulate an asset ingested before #939: only the asserted item is linked.
	h.exec(t, `DELETE FROM asset_items`)

	n, err := h.cat.BackfillFurtherEpisodes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the backfill added %d links, want 1", n)
	}
	if again, err := h.cat.BackfillFurtherEpisodes(t.Context()); err != nil || again != 0 {
		t.Fatalf("a second backfill added %d links (err %v), want 0", again, err)
	}
	res, err := h.cat.ReconcileDesired(t.Context(), wants["S01E02"])
	if err != nil {
		t.Fatal(err)
	}
	if !res.State.Managed {
		t.Error("S01E02 holds no bytes after the backfill linked the S01E01E02 file to it")
	}
}
