package catalog_test

import (
	"context"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
)

// An edition first created by a companion has no type; the dump recorded into
// it afterwards fills it (ADR-0121). Scan order is not chosen, so cover art
// ahead of its disc image must not leave the edition typeless for good.
func TestALaterDumpFillsTheTypeOfAnEditionCreatedByArtwork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t)
	h.exec(t, `INSERT INTO libraries (id, name, content_type, enabled, created_at)
		VALUES ('lib-rom', 'ROMs', 'rom', 1, ?)`, stamp)
	h.exec(t, `INSERT INTO library_roots (id, library_id, path, ingest_mode, enabled, created_at)
		VALUES ('root-rom', 'lib-rom', '/srv/roms', 'reflink', 1, ?)`, stamp)
	root := ingest.Root{
		ID: "root-rom", LibraryID: "lib-rom", LibraryContentType: identification.ROM,
		Path: "/srv/roms", Mode: ingest.Reflink, Enabled: true,
	}

	self, err := h.cat.SelfPeer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	record := func(rel, fill string) {
		t.Helper()
		c := identification.Identify(rel, identification.ROM)
		if c.ContentType != identification.ROM {
			t.Fatalf("fixture: %s identified as %s", rel, c.ContentType)
		}
		if _, err := h.cat.Record(ctx, ingest.Recording{
			Root: root, Candidate: c,
			Blob:       ingest.Blob{Hash: "blake3:" + repeat(fill, 64), Size: 4096, Materialised: ingest.Reflink},
			SourcePath: "/srv/roms/" + rel, RelPath: rel, Filename: rel,
			MIME: "application/octet-stream", PeerID: self, Now: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	record("psx/Some Game/cover.jpg", "a")
	record("psx/Some Game/Some Game.bin", "b")

	var n int
	var typ string
	if err := h.db.Reader().QueryRowContext(ctx, `SELECT count(*), max(edition_type) FROM editions e
		JOIN works w ON w.id = e.work_id WHERE w.work_key = 'rom:psx:some game'`).Scan(&n, &typ); err != nil {
		t.Fatal(err)
	}
	if n != 1 || typ != "disc-image" {
		t.Errorf("%d edition(s), type %q; want one disc-image edition", n, typ)
	}
	// The fill is a state transition, so it is in the log (invariant 7).
	var emitted int
	if err := h.db.Reader().QueryRowContext(ctx, `SELECT count(*) FROM events
		WHERE type = 'content.edition.updated' AND json_extract(payload, '$.edition_type') = 'disc-image'`).Scan(&emitted); err != nil {
		t.Fatal(err)
	}
	if emitted != 1 {
		t.Errorf("%d content.edition.updated event(s) for the fill, want 1", emitted)
	}
}
