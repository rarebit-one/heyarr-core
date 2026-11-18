package worker

import (
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// A by-hand select or block on a want whose grab is already at the client
// (#888), end to end against the fake download client: the catalog moves the
// want and hands back the displaced transfer, remove_transfer takes it out of
// the client, and the grab fetches the new choice.

// queueOnGood searches with two acceptable releases on offer — "good" wins on
// codec — and grabs, leaving the want QUEUED on good's transfer.
func (h *pollHarness) queueOnGood(t *testing.T) string {
	t.Helper()
	h.client.Offer(secret.Value("magnet:?xt=urn:btih:good"), "Arrival.2160p.hevc.mkv", []byte("the good film"))
	h.client.Offer(secret.Value("magnet:?xt=urn:btih:plain"), "Arrival.1080p.avc.mkv", []byte("the plain film"))
	h.fake.Offer("Arrival", offer("good", 2160, "hevc"), offer("plain", 1080, "avc"))
	if err := h.run(t); err != nil {
		t.Fatal(err)
	}
	h.grabNow(t)
	if got := h.state(t).Phase; got != acquisition.PhaseQueued {
		t.Fatalf("setup: phase = %s, want queued", got)
	}
	return h.transferID(t)
}

func (h *pollHarness) grabNow(t *testing.T) {
	t.Helper()
	payload, err := json.Marshal(acquisition.GrabPayload{DesiredItemID: h.want})
	if err != nil {
		t.Fatal(err)
	}
	if err := GrabReleaseHandler(h.reg, h.cat, slog.New(slog.DiscardHandler))(
		t.Context(), jobs.Job{Type: acquisition.GrabJobType, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func (h *pollHarness) removeTransfer(t *testing.T, d *catalog.Displaced) error {
	t.Helper()
	payload, err := json.Marshal(acquisition.RemoveTransferPayload{
		DesiredItemID: d.DesiredItemID, Provider: d.Provider, ExternalID: d.ExternalID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return RemoveTransferHandler(h.reg, h.cat, slog.New(slog.DiscardHandler))(
		t.Context(), jobs.Job{Type: acquisition.RemoveTransferJobType, Payload: payload})
}

// clientHolds lists the transfer ids the fake client has.
func (h *pollHarness) clientHolds(t *testing.T) map[string]bool {
	t.Helper()
	ts, err := h.client.Transfers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, tr := range ts {
		out[tr.ID] = true
	}
	return out
}

func (h *pollHarness) selected(t *testing.T) string {
	t.Helper()
	sel, err := h.cat.SelectedCandidate(t.Context(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	return sel.CandidateID
}

// The reported bug: select on a QUEUED want answered 200 and nothing followed.
// A transfer that has fetched nothing is replaced: the old one leaves the
// client, the new release is grabbed, and the want is queued on it.
func TestASelectReplacesAQueuedTransferThatFetchedNothing(t *testing.T) {
	t.Parallel()
	h := newPollHarness(t)
	old := h.queueOnGood(t)

	sel, err := h.cat.SelectByHand(t.Context(), h.want, "plain", false)
	if err != nil {
		t.Fatal(err)
	}
	if !sel.Changed || sel.Phase != acquisition.PhaseSelected {
		t.Fatalf("selection = %+v, want changed and selected", sel)
	}
	if sel.Displaced == nil || sel.Displaced.ExternalID != old {
		t.Fatalf("displaced = %+v, want the queued transfer %s", sel.Displaced, old)
	}
	if _, err := h.cat.AcquisitionFor(t.Context(), h.want); !errors.Is(err, catalog.ErrNoAcquisitionRow) {
		t.Fatalf("the displaced transfer's row should be gone, got err=%v", err)
	}
	// The cancel and the select are both in the log (invariant 7).
	if n := h.count(t, `SELECT count(*) FROM events
		WHERE type = 'acquisition.phase_changed' AND json_extract(payload, '$.transition') = 'cancel'`); n != 1 {
		t.Errorf("%d cancel transitions recorded, want 1", n)
	}

	if err := h.removeTransfer(t, sel.Displaced); err != nil {
		t.Fatal(err)
	}
	h.grabNow(t)

	if got := h.state(t).Phase; got != acquisition.PhaseQueued {
		t.Fatalf("phase = %s, want queued on the new release", got)
	}
	now := h.transferID(t)
	if now == old {
		t.Fatal("the want is still queued on the replaced transfer")
	}
	if held := h.clientHolds(t); len(held) != 1 || !held[now] {
		t.Errorf("the client holds %v, want only the new transfer %s", held, now)
	}
}

// A transfer that has fetched bytes is the operator's call: refused without
// force, and naming how far it got; replaced with it.
func TestASelectRefusesToReplaceADownloadWithoutForce(t *testing.T) {
	t.Parallel()
	h := newPollHarness(t)
	old := h.queueOnGood(t)
	if err := h.client.Progress(old, 4); err != nil {
		t.Fatal(err)
	}
	h.poll(t)
	if got := h.state(t).Phase; got != acquisition.PhaseDownloading {
		t.Fatalf("setup: phase = %s, want downloading", got)
	}

	_, err := h.cat.SelectByHand(t.Context(), h.want, "plain", false)
	if !errors.Is(err, catalog.ErrTransferInProgress) {
		t.Fatalf("err = %v, want ErrTransferInProgress", err)
	}
	if got := h.state(t).Phase; got != acquisition.PhaseDownloading {
		t.Errorf("a refused select moved the want to %s", got)
	}
	if got := h.selected(t); got != "good" {
		t.Errorf("a refused select changed the selection to %s", got)
	}

	sel, err := h.cat.SelectByHand(t.Context(), h.want, "plain", true)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Displaced == nil || sel.Displaced.BytesDone != 4 {
		t.Fatalf("displaced = %+v, want the 4-byte transfer", sel.Displaced)
	}
	if err := h.removeTransfer(t, sel.Displaced); err != nil {
		t.Fatal(err)
	}
	if h.clientHolds(t)[old] {
		t.Error("a forced select left the replaced transfer at the client")
	}
}

// Naming the release already in flight is not a replacement.
func TestSelectingTheReleaseInFlightChangesNothing(t *testing.T) {
	t.Parallel()
	h := newPollHarness(t)
	old := h.queueOnGood(t)

	sel, err := h.cat.SelectByHand(t.Context(), h.want, "good", false)
	if err != nil {
		t.Fatal(err)
	}
	if sel.Changed || sel.Displaced != nil {
		t.Fatalf("selection = %+v, want nothing changed", sel)
	}
	if got := h.state(t).Phase; got != acquisition.PhaseQueued {
		t.Errorf("phase = %s, want still queued", got)
	}
	if h.transferID(t) != old {
		t.Error("the transfer row changed")
	}
}

// Blocking the release a want is queued on cancels the grab and lets a search
// choose again, which can no longer choose the blocked one.
func TestBlockingTheReleaseInFlightCancelsItsGrab(t *testing.T) {
	t.Parallel()
	h := newPollHarness(t)
	old := h.queueOnGood(t)

	b, _, err := h.cat.BlockByHand(t.Context(), h.want, "", "good", "wrong language")
	if err != nil {
		t.Fatal(err)
	}
	displaced, cancelled, err := h.cat.CancelInFlightRelease(t.Context(), h.want, b.Provider, b.CandidateID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !cancelled || displaced == nil || displaced.ExternalID != old {
		t.Fatalf("cancelled = %v, displaced = %+v; want the queued transfer cancelled", cancelled, displaced)
	}
	if got := h.state(t).Phase; got != acquisition.PhaseIdle {
		t.Fatalf("phase = %s, want idle", got)
	}
	if err := h.removeTransfer(t, displaced); err != nil {
		t.Fatal(err)
	}
	if h.clientHolds(t)[old] {
		t.Error("the blocked release's transfer is still at the client")
	}

	if err := h.run(t); err != nil {
		t.Fatal(err)
	}
	if got := h.selected(t); got != "plain" {
		t.Fatalf("the re-search selected %s, want plain", got)
	}

	// A second cancel is a no-op: the want is no longer on the blocked release.
	if _, again, err := h.cat.CancelInFlightRelease(t.Context(), h.want, b.Provider, b.CandidateID, ""); err != nil || again {
		t.Errorf("a repeat cancel = (%v, %v), want (false, nil)", again, err)
	}
}

// Blocking a release the want is NOT fetching changes only the next search.
func TestBlockingAnotherReleaseLeavesTheGrabAlone(t *testing.T) {
	t.Parallel()
	h := newPollHarness(t)
	h.queueOnGood(t)

	b, _, err := h.cat.BlockByHand(t.Context(), h.want, "", "plain", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, cancelled, err := h.cat.CancelInFlightRelease(t.Context(), h.want, b.Provider, b.CandidateID, ""); err != nil || cancelled {
		t.Fatalf("cancel = (%v, %v), want (false, nil)", cancelled, err)
	}
	if got := h.state(t).Phase; got != acquisition.PhaseQueued {
		t.Errorf("phase = %s, want still queued", got)
	}
}

// The removal's guard: a transfer a want has taken up again is left alone. A
// person replaced good with plain and then chose good again before the
// removal ran; the second grab re-adopted good's transfer.
func TestARemovalSparesATransferAWantTookUpAgain(t *testing.T) {
	t.Parallel()
	h := newPollHarness(t)
	old := h.queueOnGood(t)

	sel, err := h.cat.SelectByHand(t.Context(), h.want, "plain", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.cat.SelectByHand(t.Context(), h.want, "good", false); err != nil {
		t.Fatal(err)
	}
	h.grabNow(t)
	if h.transferID(t) != old {
		t.Fatal("setup: the re-grab should re-adopt good's transfer")
	}

	if err := h.removeTransfer(t, sel.Displaced); err != nil {
		t.Fatal(err)
	}
	if !h.clientHolds(t)[old] {
		t.Error("the removal took out a transfer the want had taken up again")
	}
}

// A transfer already gone is what the removal wanted.
func TestRemovingAnAbsentTransferSucceeds(t *testing.T) {
	t.Parallel()
	h := newPollHarness(t)
	d := &catalog.Displaced{DesiredItemID: h.want, Provider: h.client.Name(), ExternalID: "fake:gone"}
	if err := h.removeTransfer(t, d); err != nil {
		t.Errorf("removing an absent transfer = %v, want nil", err)
	}
}

// A want whose transfer vanished from the client is due a search at once, not
// when the backoff of the search that chose it says (#888's third point).
func TestAWantWhoseTransferVanishedIsDueASearchAtOnce(t *testing.T) {
	t.Parallel()
	h := newPollHarness(t)
	id := h.queueOnGood(t)
	now := time.Now().UTC()
	if _, err := h.cat.RecordSearchScheduled(t.Context(), h.want,
		acquisition.MissingSearches(), 3, now, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, found, err := h.cat.SearchSchedule(t.Context(), h.want); err != nil || !found {
		t.Fatalf("setup: schedule found=%v err=%v", found, err)
	}

	if err := h.client.Remove(t.Context(), id, false); err != nil {
		t.Fatal(err)
	}
	h.backdateLastSeen(t, 2*orphanDownloadGrace)
	h.poll(t)

	if got := h.state(t).Phase; got != acquisition.PhaseIdle {
		t.Fatalf("phase = %s, want idle", got)
	}
	if _, found, err := h.cat.SearchSchedule(t.Context(), h.want); err != nil {
		t.Fatal(err)
	} else if found {
		t.Error("the search backoff survived; the want would sit idle until it ran out")
	}
	due, err := h.cat.DueSearches(t.Context(), time.Now().UTC(), 10)
	if err != nil {
		t.Fatal(err)
	}
	var listed bool
	for _, d := range due {
		listed = listed || d.DesiredItemID == h.want
	}
	if !listed {
		t.Error("the want is not due a search on the next beat")
	}
}
