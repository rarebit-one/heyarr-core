package catalog_test

import (
	"context"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
)

// A ROM want is satisfied by a dump, never by the cover art or the cue sheet
// beside it (ADR-0121). `rom-indexed` judges size alone, so without this any
// nonempty companion a scan attached to the Work would end the want with
// nothing playable.
func TestOnlyADumpSatisfiesAROMWant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name, path, role string
		want             acquisition.Satisfaction
	}{
		{"cover art alone", "/srv/roms/psx/Final Fantasy VII/cover.jpg", "artwork", acquisition.SatisfactionNot},
		{"an orphan cue sheet", "/srv/roms/psx/Final Fantasy VII/Final Fantasy VII (USA).cue", "primary", acquisition.SatisfactionNot},
		{"the disc image", "/srv/roms/psx/Final Fantasy VII/Final Fantasy VII (USA).bin", "primary", acquisition.SatisfactionSatisfied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
				t.Fatal(err)
			}
			h.exec(t, `UPDATE works SET content_type = 'rom', work_key = 'rom:psx:final fantasy vii' WHERE id = 'w1'`)
			h.setProfile(t, `[{"attribute":"size_bytes","op":"gte","value":1}]`)
			hash := "blake3:" + repeat("d", 64)
			h.exec(t, `INSERT INTO blobs (hash, size, mime, first_seen_at)
				VALUES (?, 4096, 'application/octet-stream', ?)`, hash, stamp)
			h.exec(t, `INSERT INTO editions (id, work_id, label, edition_type, language, attributes, created_at)
				VALUES ('e-rom', 'w1', 'USA', 'disc-image', '', '{"platform":"psx"}', ?)`, stamp)
			h.exec(t, `INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash,
					source_path, role, filename, mime, identification_source, attributes, created_at, updated_at)
				VALUES ('a-rom', 'e-rom', NULL, 'managed', ?, ?, ?, 'f', 'application/octet-stream', 'path', '{}', ?, ?)`,
				hash, tc.path, tc.role, stamp, stamp)

			got, err := h.cat.ReconcileDesired(ctx, h.want)
			if err != nil {
				t.Fatal(err)
			}
			if got.Content.Satisfaction != tc.want {
				t.Errorf("content = %s, want %s", got.Content.Satisfaction, tc.want)
			}
		})
	}
}
