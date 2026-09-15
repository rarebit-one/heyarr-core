package worker

import (
	"errors"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// backdateLastSeen ages this want's acquisition row so it looks as though the
// poll has not observed its transfer for `by` — the same thing a real removal
// produces once enough passes go by without the transfer being listed.
func (h *pollHarness) backdateLastSeen(t *testing.T, by time.Duration) {
	t.Helper()
	old := time.Now().UTC().Add(-by).Format(time.RFC3339Nano)
	if _, err := h.db.Writer().ExecContext(t.Context(),
		`UPDATE acquisitions SET last_seen_at = ? WHERE desired_item_id = ?`,
		old, h.want); err != nil {
		t.Fatal(err)
	}
}

// A transfer removed from the download client out from under Heyarr — in the
// client's own UI, by another tool, by a restart that lost it — must not leave
// its want parked in DOWNLOADING forever.
//
// PollDownloadsHandler only ever iterates the transfers a client currently
// reports, so a transfer that is simply gone was never the subject of any
// transition and nothing else moved the want. The sweep detects the absence and
// fails the want back to idle, where a fresh search can re-acquire it.
func TestAWantWhoseTransferVanishesIsFailedBackToIdle(t *testing.T) {
	h := newPollHarness(t)
	h.grabAfterSearch(t, "Arrival.2016.2160p.mkv", []byte("the bytes of a film"))
	id := h.transferID(t)

	// Observe it downloading, so the want is in DOWNLOADING — the reported bug.
	if err := h.client.Progress(id, 10); err != nil {
		t.Fatal(err)
	}
	h.poll(t)
	if got := h.state(t).Phase; got != acquisition.PhaseDownloading {
		t.Fatalf("setup: phase is %s, expected downloading", got)
	}

	// The transfer is removed from the client, and enough passes go by without
	// it being listed that its row falls past the grace window.
	if err := h.client.Remove(t.Context(), id, false); err != nil {
		t.Fatal(err)
	}
	h.backdateLastSeen(t, 2*orphanDownloadGrace)

	h.poll(t)

	if got := h.state(t).Phase; got != acquisition.PhaseIdle {
		t.Fatalf("phase = %s, want idle: a want whose transfer vanished must be "+
			"freed to re-acquire, not stranded in DOWNLOADING", got)
	}
	if _, err := h.cat.AcquisitionFor(t.Context(), h.want); !errors.Is(err, catalog.ErrNoAcquisitionRow) {
		t.Fatalf("acquisition row still present (err=%v); the orphaned link should be dropped", err)
	}
}

// The guard against a transient read: a transfer absent from a SINGLE pass,
// still within the grace window, is left alone — a client that momentarily
// omits a transfer that still exists must not cost the download.
func TestATransientlyAbsentTransferSurvivesTheGrace(t *testing.T) {
	h := newPollHarness(t)
	h.grabAfterSearch(t, "Arrival.2016.2160p.mkv", []byte("the bytes of a film"))
	id := h.transferID(t)
	if err := h.client.Progress(id, 10); err != nil {
		t.Fatal(err)
	}
	h.poll(t) // records a fresh last_seen and moves the want to DOWNLOADING

	// Gone from the client this pass, but its row was refreshed moments ago
	// (well within orphanDownloadGrace), so the sweep must not conclude it lost.
	if err := h.client.Remove(t.Context(), id, false); err != nil {
		t.Fatal(err)
	}
	h.poll(t)

	if got := h.state(t).Phase; got != acquisition.PhaseDownloading {
		t.Fatalf("phase = %s, want downloading: a single missed poll inside the "+
			"grace must not fail the want", got)
	}
	if _, err := h.cat.AcquisitionFor(t.Context(), h.want); err != nil {
		t.Fatalf("acquisition row gone (err=%v); a within-grace absence should keep it", err)
	}
}
