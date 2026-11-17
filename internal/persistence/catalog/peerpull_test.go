package catalog_test

import (
	"context"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// An outcome is recorded with an event when it is new or moves, and an
// unchanged one re-evaluated on a later pass writes and emits nothing
// (invariant 7, invariant 9).
func TestRecordHoldApplication(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	blob := hashOf('a')
	h.exec(t, `INSERT INTO catalog_ops (op_hash, content_type, work_key, op, signer, prev, iat, token, received_at)
		VALUES ('sha256:hold', 'rom', 'rom:snes:x', 'hold', 'ed25519:x', '[]', ?, 'tok', ?)`, stamp, stamp)

	steps := []struct {
		outcome catalog.HoldOutcome
		detail  string
		events  int
	}{
		{catalog.HoldPulling, "from site north; pull queued", 1},
		{catalog.HoldPulling, "from site north; pull queued", 1},
		{catalog.HoldHeld, "from site north; recorded in library roms", 2},
	}
	for i, s := range steps {
		if err := h.cat.RecordHoldApplication(ctx, catalog.HoldApplication{
			OpHash: "sha256:hold", BlobHash: blob, Outcome: s.outcome, Detail: s.detail,
		}); err != nil {
			t.Fatal(err)
		}
		if n := h.eventsOf(t, events.TypeHoldApplicationChanged); n != s.events {
			t.Fatalf("step %d: %d hold_changed events, want %d", i, n, s.events)
		}
	}
	got, err := h.cat.HoldApplications(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a := got["sha256:hold"]; a.Outcome != catalog.HoldHeld || a.BlobHash != blob || !a.Outcome.Final() {
		t.Fatalf("application = %+v, want held and final", a)
	}
	if catalog.HoldPulling.Final() || catalog.HoldNoLibrary.Final() || !catalog.HoldOwn.Final() {
		t.Fatal("only own and held are final")
	}
}
