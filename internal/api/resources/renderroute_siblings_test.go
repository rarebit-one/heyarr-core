package resources_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/api/resources"
)

// A refused cross-site route offers "a smaller copy of the same thing". A
// season edition holds every episode's video, so the lookup must not offer a
// different episode just because it shares the work (ADR-0111 §R2).
func TestSiblingAssetsStayOnTheSameEpisodeOrFilm(t *testing.T) {
	t.Parallel()
	h := newHarness(t).seed()

	const (
		series   = "01990000-0000-7000-8000-0000000000s1"
		season   = "01990000-0000-7000-8000-0000000000s2"
		remux    = "01990000-0000-7000-8000-0000000000s3"
		ep1, ep2 = "01990000-0000-7000-8000-0000000000e1", "01990000-0000-7000-8000-0000000000e2"
	)
	h.exec(`INSERT INTO works (id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES (?, 'series', 'a-show|2020', 'A Show', 'a show', 2020, '{}', ?, ?)`, series, seedTime, seedTime)
	h.exec(`INSERT INTO editions (id, work_id, edition_key, label, edition_type, language, attributes, created_at) VALUES
		(?, ?, 's1-web', 'season 1', 'web', 'en', '{}', ?), (?, ?, 's1-remux', 'season 1 remux', 'remux', 'en', '{}', ?)`,
		season, series, seedTime, remux, series, seedTime)
	h.exec(`INSERT INTO items (id, work_id, edition_id, item_key, title, published_at, attributes, created_at, updated_at) VALUES
		(?, ?, ?, 'S01E01', 'One', NULL, '{}', ?, ?), (?, ?, ?, 'S01E02', 'Two', NULL, '{}', ?, ?)`,
		ep1, series, season, seedTime, seedTime, ep2, series, season, seedTime, seedTime)

	video := func(height int) string {
		return `[{"index":0,"type":"video","codec":"h264","height":` + strconv.Itoa(height) + `}]`
	}
	// e1 4K remux (the one asked for), e1 720p (a real alternative), e2 720p
	// (a different episode: must NOT be offered).
	for i, a := range []struct {
		id, edition, item string
		height            int
		bps               int
	}{
		{"as-e1-4k", remux, ep1, 2160, 60_000_000},
		{"as-e1-720", season, ep1, 720, 4_000_000},
		{"as-e2-720", season, ep2, 720, 4_000_000},
	} {
		hash := "blake3:" + string(rune('a'+i)) + "000000000000000000000000000000000000000000000000000000000000000"
		h.exec(`INSERT INTO blobs (hash, size, mime, first_seen_at) VALUES (?, 1, 'video/mp4', ?)`, hash, seedTime)
		h.exec(`INSERT INTO assets (id, edition_id, item_id, library_id, source_class, blob_hash, source_path,
				role, filename, mime, identification_source, missing_since, created_at, updated_at)
			VALUES (?, ?, ?, NULL, 'managed', ?, '/x', 'primary', 'x.mp4', 'video/mp4', 'path', NULL, ?, ?)`,
			a.id, a.edition, a.item, hash, seedTime, seedTime)
		h.exec(`INSERT INTO blob_probes (blob_hash, container, format_long, duration_seconds, bitrate_bps,
				streams, bytes_read, materialised, probed_at)
			VALUES (?, 'mov,mp4,m4a', '', 60.0, ?, ?, 4096, 0, ?)`, hash, a.bps, video(a.height), seedTime)
	}

	got := resources.SiblingAssets(h.api, context.Background(), "as-e1-4k")
	if len(got) != 1 || got[0].AssetID != "as-e1-720" || got[0].HeightPx != 720 || got[0].BitrateKbps != 4000 {
		t.Fatalf("siblings of the episode-1 remux = %+v, want only the episode-1 720p copy", got)
	}
}
