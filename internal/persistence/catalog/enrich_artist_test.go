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

func TestAlbumEnrichmentCannotOverwriteRecordingIdentity(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.exec(t, `UPDATE works SET content_type='music', work_key='music-recording:11111111-1111-4111-8111-111111111111', title='Jump', attributes='{"artist":"Van Halen"}' WHERE id='w1'`)
	if err := h.cat.CorrectWorkIdentity(context.Background(), "w1", "Another Album", "Wrong Artist", time.Now()); err != nil {
		t.Fatal(err)
	}
	var title, artist string
	if err := h.db.Reader().QueryRow(`SELECT title, json_extract(attributes, '$.artist') FROM works WHERE id='w1'`).Scan(&title, &artist); err != nil {
		t.Fatal(err)
	}
	if title != "Jump" || artist != "Van Halen" {
		t.Fatalf("recording was rewritten: %s / %s", title, artist)
	}
	if _, found, err := h.cat.EnrichContext(context.Background(), "w1"); err != nil || found {
		t.Fatalf("album enrichment context: found=%v err=%v", found, err)
	}
}

func TestArtistNamedRecordingRemainsAnAlbum(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.exec(t, `UPDATE works SET content_type='music', work_key='music:recording:album', title='Album', attributes='{"artist":"Recording"}' WHERE id='w1'`)
	if err := h.cat.CorrectWorkIdentity(context.Background(), "w1", "Canonical Album", "Recording", time.Now()); err != nil {
		t.Fatal(err)
	}
	var title string
	if err := h.db.Reader().QueryRow(`SELECT title FROM works WHERE id='w1'`).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "Canonical Album" {
		t.Fatalf("ordinary album misidentified as recording-specific work: title=%q", title)
	}
}
