package catalog_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestEnrichmentKeepsManuallyCorrectedArtist(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.exec(t, `UPDATE works SET content_type = 'music', attributes = ? WHERE id = 'w1'`,
		`{"artist":"Correct Artist","artist_override":true,"label":"Original"}`)

	if err := h.cat.CorrectWorkIdentity(context.Background(), "w1", "Canonical Album", "Wrong Artist",
		time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	var title, raw string
	if err := h.db.Reader().QueryRow(`SELECT title, attributes FROM works WHERE id = 'w1'`).Scan(&title, &raw); err != nil {
		t.Fatal(err)
	}
	var attrs map[string]any
	if err := json.Unmarshal([]byte(raw), &attrs); err != nil {
		t.Fatal(err)
	}
	if title != "Canonical Album" || attrs["artist"] != "Correct Artist" || attrs["label"] != "Original" {
		t.Fatalf("title = %q, attributes = %v; want new title and the corrected artist", title, attrs)
	}
}
