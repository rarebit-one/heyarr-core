package worker

import (
	"errors"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/downloads"
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

// backdatePhaseEntered ages this want's acquisition_state so its current phase
// looks as though it was entered `by` ago — the way a want that failed back to
// idle and then sat there looks once the grace elapses.
func (h *pollHarness) backdatePhaseEntered(t *testing.T, by time.Duration) {
	t.Helper()
	old := time.Now().UTC().Add(-by).Format(time.RFC3339Nano)
	if _, err := h.db.Writer().ExecContext(t.Context(),
		`UPDATE acquisition_state SET phase_entered_at = ? WHERE desired_item_id = ?`,
		old, h.want); err != nil {
		t.Fatal(err)
	}
}

// A grab the download client could never fetch — a tracker that refuses the
// infohash after the add — fails the release back to idle but leaves the want
// carrying its troubled transfer row and its now-dead selection. Nothing
// re-drives an idle want on its own: reconcile only sets satisfaction,
// sweepOrphanedDownloads only looks at in-flight phases, and the search beat
// waits out the failed search's backoff AND would re-pick the very release that
// just failed. The stuck-grab sweep must block that release and clear the want's
// backoff so a fresh search chooses a different one — the exact wedge E7 hit.
// A transient tracker error must not abandon a download that is still moving.
//
// stall.go surfaces a per-tracker rejection ("info hash is not authorized") as
// the transfer's Error even while other trackers serve it and bytes flow.
// Failing on that string alone dropped a viable download to idle, where the
// transfer then completed in the client but was orphaned — the want never
// ingested it (Alien Earth E7). A dead tracker among several does not stop a
// torrent that is still pulling bytes, so a progressing transfer keeps going and
// a completed one ingests, error string or not.
func TestATransientTrackerErrorDoesNotAbandonAProgressingDownload(t *testing.T) {
	t.Parallel()
	h := newPollHarness(t)
	h.grabAfterSearch(t, "Arrival.2016.2160p.mkv", []byte("the actual bytes of a film"))
	id := h.transferID(t)

	if err := h.client.Progress(id, 5); err != nil {
		t.Fatal(err)
	}
	h.poll(t)
	if got := h.state(t).Phase; got != acquisition.PhaseDownloading {
		t.Fatalf("setup: phase = %s, want downloading", got)
	}

	// A dead tracker sets an Error, but the transfer is STILL pulling bytes
	// (12 > 5). It must not be failed to idle for the string.
	if err := h.client.Fail(id, downloads.TroubleClientError,
		"info hash is not authorized with this tracker"); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Progress(id, 12); err != nil {
		t.Fatal(err)
	}
	h.poll(t)
	if got := h.state(t).Phase; got != acquisition.PhaseDownloading {
		t.Fatalf("phase = %s, want downloading: a still-moving transfer with a transient "+
			"tracker error must keep going, not be abandoned to idle", got)
	}

	// It finishes with the stale error still on it — the completed bytes ingest,
	// they are not thrown away.
	if _, err := h.client.Complete(id); err != nil {
		t.Fatal(err)
	}
	h.poll(t)
	if got := h.state(t).Phase; got == acquisition.PhaseIdle || got == acquisition.PhaseDownloading {
		t.Fatalf("phase = %s: a completed transfer must reach verifying even with a stale error", got)
	}
}

// The control: an errored transfer that is genuinely STUCK — no progress since
// the last pass — is still failed back to idle, so a fresh search picks another
// release. Without this, dropping the immediate fail-on-error would strand a
// dead release in DOWNLOADING forever.
func TestAStalledTransferWithAnErrorIsStillFailed(t *testing.T) {
	t.Parallel()
	h := newPollHarness(t)
	h.grabAfterSearch(t, "Arrival.2016.2160p.mkv", []byte("the actual bytes of a film"))
	id := h.transferID(t)

	if err := h.client.Progress(id, 8); err != nil {
		t.Fatal(err)
	}
	h.poll(t) // downloading, bytes_done recorded as 8

	// Errored, and NOT progressing (still 8 this pass) — a dead release.
	if err := h.client.Fail(id, downloads.TroubleClientError,
		"info hash is not authorized with this tracker"); err != nil {
		t.Fatal(err)
	}
	h.poll(t)
	if got := h.state(t).Phase; got != acquisition.PhaseIdle {
		t.Fatalf("phase = %s, want idle: a stalled, errored transfer is a dead release and must be failed", got)
	}
}

func TestAWantWhoseGrabIsRejectedIsBlockedAndReDriven(t *testing.T) {
	t.Parallel()
	h := newPollHarness(t)
	h.grabAfterSearch(t, "Arrival.2016.2160p.mkv", []byte("the bytes of a film"))
	id := h.transferID(t)

	// The tracker refuses the infohash — the release is unfetchable, not bad
	// bytes and not a local problem.
	if err := h.client.Fail(id, downloads.TroubleClientError,
		"info hash is not authorized with this tracker"); err != nil {
		t.Fatal(err)
	}
	h.poll(t) // advancePipeline fails the want to idle, keeping its row and selection
	if got := h.state(t).Phase; got != acquisition.PhaseIdle {
		t.Fatalf("setup: phase is %s, expected idle after the grab failed", got)
	}

	// Age the idle past the grace, so the sweep treats it as settled rather than
	// as a want that only just fell back this pass.
	h.backdatePhaseEntered(t, 2*stuckGrabGrace)

	h.poll(t)

	// The unfetchable release is blocked, so the next search will not pick it.
	blocked, err := h.cat.BlockedFor(t.Context(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 1 || blocked[0].Reason != catalog.BlockGrabFailed {
		t.Fatalf("blocked = %+v, want one grab_failed entry: the unfetchable release "+
			"must be blocked so the search stops choosing it", blocked)
	}
	// Its transfer row is dropped...
	if _, err := h.cat.AcquisitionFor(t.Context(), h.want); !errors.Is(err, catalog.ErrNoAcquisitionRow) {
		t.Fatalf("acquisition row still present (err=%v); the failed transfer should be dropped", err)
	}
	// ...and its backoff cleared, so the want is due a search now instead of
	// sitting out the failed search's exponential wait.
	if _, found, err := h.cat.SearchSchedule(t.Context(), h.want); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("search schedule still present; a re-driven want must be due a fresh search at once")
	}
}

// The guard against racing the pass that failed it: a grab that only just failed
// is left alone until the grace elapses — its release stays unblocked and its
// row stays put.
func TestAJustFailedGrabSurvivesTheGrace(t *testing.T) {
	t.Parallel()
	h := newPollHarness(t)
	h.grabAfterSearch(t, "Arrival.2016.2160p.mkv", []byte("the bytes of a film"))
	id := h.transferID(t)
	if err := h.client.Fail(id, downloads.TroubleClientError,
		"info hash is not authorized with this tracker"); err != nil {
		t.Fatal(err)
	}
	h.poll(t) // fails to idle, phase entered just now — inside the grace
	h.poll(t) // a second pass, still inside the grace

	blocked, err := h.cat.BlockedFor(t.Context(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 0 {
		t.Fatalf("blocked = %+v, want none: a grab that failed inside the grace must not be swept", blocked)
	}
	if _, err := h.cat.AcquisitionFor(t.Context(), h.want); err != nil {
		t.Fatalf("acquisition row gone (err=%v); a within-grace failure should keep it", err)
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
	t.Parallel()
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
	t.Parallel()
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
