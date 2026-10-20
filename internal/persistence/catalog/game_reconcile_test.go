package catalog_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
)

// A game want under a store gate is satisfied by a verified store package and
// NOT by what a scanned filename claims (ADR-0118).
//
// A scan records identification's EditionAttributes on the ASSET and leaves the
// edition's attributes empty (resolveEdition), which is where the fixture puts
// them. A lone "-1.bin" carries the same store facts as the whole installer
// set, so if reconcile ever read the asset's facts, one fragment of a game
// would satisfy the want and become its only placement target. The verified
// case stands in for the store fetch, which writes package-level facts on the
// edition once it has checked the whole package against the store manifest.
func TestOnlyAVerifiedStorePackageSatisfiesAGameWant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	fragment := identification.Identify("setup_arrival_1.2.3_(4567)-1.bin", identification.Game)
	if fragment.ContentType != identification.Game || fragment.EditionAttributes["store"] != "gog" {
		t.Fatalf("fixture: the fragment identified as %q with %v", fragment.ContentType, fragment.EditionAttributes)
	}
	scanned, err := json.Marshal(fragment.EditionAttributes)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name         string
		editionAttrs string
		want         acquisition.Satisfaction
	}{
		{"a scanned installer part: filename facts on the asset only", `{}`, acquisition.SatisfactionNot},
		{"a package a store fetch verified", `{"store":"gog","drm_free":true,"platform":"windows","package_blobs":["blake3:` +
			repeat("c", 64) + `"]}`, acquisition.SatisfactionSatisfied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
				t.Fatal(err)
			}
			h.exec(t, `UPDATE works SET content_type = 'game', work_key = 'game:arrival' WHERE id = 'w1'`)
			h.setProfile(t, `[{"attribute":"store","op":"in","value":["gog","steam","epic"]}]`)
			hash := "blake3:" + repeat("c", 64)
			h.exec(t, `INSERT INTO blobs (hash, size, mime, first_seen_at)
				VALUES (?, 1073741824, 'application/octet-stream', ?)`, hash, stamp)
			h.exec(t, `INSERT INTO editions (id, work_id, label, edition_type, language, attributes, created_at)
				VALUES ('e-game', 'w1', 'GOG Windows', 'installer', '', ?, ?)`, tc.editionAttrs, stamp)
			h.exec(t, `INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash,
					source_path, role, filename, mime, identification_source, attributes, created_at, updated_at)
				VALUES ('a-game', 'e-game', NULL, 'managed', ?, '/srv/games/setup-1.bin', 'primary',
					'setup-1.bin', 'application/octet-stream', 'path', ?, ?, ?)`, hash, string(scanned), stamp, stamp)

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
