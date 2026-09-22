package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The subtitle-want door (ADR-0085): create a subtitle want per held video that
// has no caption in a language, scoped precisely (by item for an episode, by
// edition for a film) so the fetch driver acquires exactly what is missing.

const wantStamp = "2026-09-09T12:00:00Z"

// seedSubtitleProfile inserts the seeded `subtitle` profile the door resolves by
// name. Its rules do not matter to the door (it only resolves the id); the row
// existing is what matters.
func seedSubtitleProfile(t *testing.T, h *harness) {
	t.Helper()
	h.exec(`INSERT INTO quality_profiles (id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
	        VALUES ('q-sub', 'subtitle', '', '[{"attribute":"size_bytes","op":"gte","value":1}]', '[]',
	                '[{"attribute":"size_bytes","op":"gte","value":1}]', 1, ?, ?)`, wantStamp, wantStamp)
}

// seedEpisodeVideo inserts a series work + season edition + an item + a managed
// video linked to that item (ADR-0086). withEN adds an English subtitle on the
// item, which is what "already captioned" looks like to the want scan.
func seedEpisodeVideo(t *testing.T, h *harness, workID, itemID, videoBlob string, withEN bool) {
	t.Helper()
	ed := workID + "-ed"
	h.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, created_at, updated_at)
	        VALUES (?, 'series', ?, ?, ?, ?, ?)`, workID, workID, workID, workID, wantStamp, wantStamp)
	h.exec(`INSERT INTO editions (id, work_id, created_at) VALUES (?, ?, ?)`, ed, workID, wantStamp)
	h.exec(`INSERT INTO items (id, work_id, edition_id, item_key, title, attributes, created_at, updated_at)
	        VALUES (?, ?, ?, 'S01E01', 'Pilot', '{"season":1,"episode":1}', ?, ?)`, itemID, workID, ed, wantStamp, wantStamp)
	h.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 1000, 'video/mp4', ?)`, videoBlob, wantStamp)
	h.exec(`INSERT INTO assets (id, edition_id, source_class, blob_hash, source_path, role, filename, mime, identification_source, item_id, created_at, updated_at)
	        VALUES (?, ?, 'managed', ?, ?, 'primary', ?, 'video/mp4', 'scan', ?, ?, ?)`,
		itemID+"-v", ed, videoBlob, "/lib/"+itemID+".mp4", itemID+".mp4", itemID, wantStamp, wantStamp)
	if withEN {
		subBlob := "blake3:" + strings.Repeat("e", 64)
		h.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 100, 'application/x-subrip', ?)`, subBlob, wantStamp)
		h.exec(`INSERT INTO assets (id, edition_id, source_class, blob_hash, source_path, role, filename, mime, identification_source, attributes, item_id, created_at, updated_at)
		        VALUES (?, ?, 'managed', ?, NULL, 'subtitle', ?, 'application/x-subrip', 'fetched', '{"language":"en"}', ?, ?, ?)`,
			itemID+"-sub", ed, subBlob, itemID+".en.srt", itemID, wantStamp, wantStamp)
	}
}

func wantWithScope(t *testing.T, h *harness, bodyJSON string) (candidates, created int, status int) {
	t.Helper()
	resp := h.do(http.MethodPost, "/api/v1/subtitles/want", "", strings.NewReader(bodyJSON))
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Candidates int `json:"candidates"`
		Created    int `json:"created"`
	}
	_ = json.Unmarshal(h.body(resp), &out)
	return out.Candidates, out.Created, resp.StatusCode
}

func TestSubtitleWantCreatesItemScopedWantForAnEpisode(t *testing.T) {
	h := newHarness(t)
	seedSubtitleProfile(t, h)
	seedEpisodeVideo(t, h, "ys", "ys-e1", "blake3:"+strings.Repeat("a", 64), false)

	cand, created, status := wantWithScope(t, h, `{"work_id":"ys","language":"en"}`)
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", status)
	}
	if cand != 1 || created != 1 {
		t.Fatalf("candidates=%d created=%d, want 1/1", cand, created)
	}
	// The want is item-scoped for the episode, aspect subtitle, language en.
	var scope, aspect, lang, itemID string
	if err := h.db.Reader().QueryRow(`SELECT scope, aspect, language, coalesce(item_id,'') FROM desired_items WHERE aspect='subtitle'`).
		Scan(&scope, &aspect, &lang, &itemID); err != nil {
		t.Fatal(err)
	}
	if scope != "item" || aspect != "subtitle" || lang != "en" || itemID != "ys-e1" {
		t.Errorf("want = scope=%q aspect=%q lang=%q item=%q", scope, aspect, lang, itemID)
	}
}

func TestSubtitleWantIsIdempotent(t *testing.T) {
	h := newHarness(t)
	seedSubtitleProfile(t, h)
	seedEpisodeVideo(t, h, "ys", "ys-e1", "blake3:"+strings.Repeat("a", 64), false)

	if _, created, _ := wantWithScope(t, h, `{"work_id":"ys","language":"en"}`); created != 1 {
		t.Fatalf("first run created %d, want 1", created)
	}
	// Second run: the want exists, so nothing new is created and it is not an error.
	cand, created, status := wantWithScope(t, h, `{"work_id":"ys","language":"en"}`)
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", status)
	}
	// The video still has no subtitle asset (only a want), so it is still a
	// candidate — but the duplicate want is skipped, so created is 0.
	if created != 0 {
		t.Errorf("second run created %d, want 0 (idempotent)", created)
	}
	_ = cand
	if n := h.countRows(t, `SELECT COUNT(*) FROM desired_items WHERE aspect='subtitle'`); n != 1 {
		t.Errorf("subtitle wants = %d, want 1 (no duplicate)", n)
	}
}

func TestSubtitleWantSkipsAlreadyCaptioned(t *testing.T) {
	h := newHarness(t)
	seedSubtitleProfile(t, h)
	seedEpisodeVideo(t, h, "got", "got-e1", "blake3:"+strings.Repeat("b", 64), true) // has an en sub on the item

	cand, created, status := wantWithScope(t, h, `{"work_id":"got","language":"en"}`)
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", status)
	}
	if cand != 0 || created != 0 {
		t.Errorf("an episode already captioned in en must not be a candidate, got candidates=%d created=%d", cand, created)
	}
}

func TestSubtitleWantForAnotherLanguageStillWanted(t *testing.T) {
	h := newHarness(t)
	seedSubtitleProfile(t, h)
	seedEpisodeVideo(t, h, "got", "got-e1", "blake3:"+strings.Repeat("b", 64), true) // has en, not de

	cand, created, status := wantWithScope(t, h, `{"work_id":"got","language":"de"}`)
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", status)
	}
	if cand != 1 || created != 1 {
		t.Errorf("a German want must be created even when English exists, got candidates=%d created=%d", cand, created)
	}
}

func TestSubtitleWantRefusesUnscopedOrLanguageless(t *testing.T) {
	h := newHarness(t)
	seedSubtitleProfile(t, h)
	// No scope.
	if _, _, status := wantWithScope(t, h, `{"language":"en"}`); status != http.StatusBadRequest {
		t.Errorf("an unscoped want must be 400, got %d", status)
	}
	// No language.
	if _, _, status := wantWithScope(t, h, `{"work_id":"ys"}`); status != http.StatusBadRequest {
		t.Errorf("a language-less want must be 400, got %d", status)
	}
}
