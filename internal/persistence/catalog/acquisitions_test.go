package catalog_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Acquisitions, against a real database (§58, M3-10).
//
// The property these exist for is the duplicate grab: the poll job WILL be
// re-run, and re-running it must not queue a second copy of a transfer already
// downloading. It presents as bandwidth rather than as an error, which is why
// it needs a test rather than a comment.

const infohash = "481b6e3617be4c88f96cb25e47c9d8272130071e"

func transfer(id, name string) providers.Transfer {
	return providers.Transfer{
		ID: id, Name: name, BytesTotal: 1000, BytesDone: 250,
	}
}

func TestRecordingAnAcquisitionIsIdempotent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	a := catalog.TransferToAcquisition(
		"acq-1", h.want, "acquire", transfer(infohash, "Something.mkv"), "/local/Something.mkv")

	created, err := h.cat.RecordAcquisition(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("the first record should report a creation")
	}

	// The poll job runs again, and again, and again. Each pass sees the same
	// transfer and must converge rather than accumulate.
	for range 5 {
		created, err := h.cat.RecordAcquisition(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		if created {
			t.Fatal("a repeat pass reported a creation; the caller would emit an " +
				"event on every poll and the log would become a heartbeat")
		}
	}

	all, err := h.cat.Acquisitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("%d acquisition rows for one transfer — this is the duplicate grab", len(all))
	}
}

// Progress is refreshed on every pass even when nothing transitions, because it
// is what makes "stuck since Tuesday" visible.
func TestProgressIsRefreshedWithoutCreating(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	base := transfer(infohash, "Something.mkv")
	first := catalog.TransferToAcquisition("acq-1", h.want, "acquire", base, "/local/x")
	if _, err := h.cat.RecordAcquisition(ctx, first); err != nil {
		t.Fatal(err)
	}

	moved := base
	moved.BytesDone = 900
	moved.Error = "tracker_unreachable: every tracker failed to answer"
	if _, err := h.cat.RecordAcquisition(ctx,
		catalog.TransferToAcquisition("acq-1", h.want, "acquire", moved, "/local/x")); err != nil {
		t.Fatal(err)
	}

	got, err := h.cat.AcquisitionFor(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if got.BytesDone != 900 {
		t.Errorf("bytes_done = %d, want 900", got.BytesDone)
	}
	if got.Trouble == "" {
		t.Error("the trouble a poll observed was not recorded, so a stalled transfer " +
			"looks healthy in the database as well as in the client")
	}
}

// The case that actually breaks: a Transmission rebuilt from scratch reissues
// its numeric ids from 1, so a row keyed on one would silently start pointing
// at somebody else's transfer. Keying on the infohash survives it.
func TestTheInfohashSurvivesAClientRestart(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	before := transfer(infohash, "Something.mkv")
	if _, err := h.cat.RecordAcquisition(ctx,
		catalog.TransferToAcquisition("acq-1", h.want, "acquire", before, "/local/x")); err != nil {
		t.Fatal(err)
	}

	// The daemon was rebuilt. Same torrent, same infohash, and everything the
	// client assigns per session is different.
	after := transfer(infohash, "Something.mkv")
	after.BytesDone = 1000
	after.Done = true
	created, err := h.cat.RecordAcquisition(ctx,
		catalog.TransferToAcquisition("acq-1", h.want, "acquire", after, "/local/x"))
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("the transfer was treated as new after a client restart — that is a " +
			"second copy of something already downloading")
	}

	all, err := h.cat.Acquisitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("%d rows after a client restart", len(all))
	}
}

// One in-flight acquisition per want. When a want re-grabs — its selection
// moved to a different release — the new transfer REPLACES the old one rather
// than being refused. Two things downloading for one want would be wasted
// bandwidth, so the invariant holds; but refusing the second outright wedged
// grab_release in a permanent retry loop: the client already had the transfer,
// yet its row could never be written because desired_item_id is UNIQUE and the
// upsert only reconciles (provider, external_id). Replacing keeps exactly one
// row — the newer transfer — and lets the grab complete.
func TestOneAcquisitionPerWant(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.cat.RecordAcquisition(ctx, catalog.TransferToAcquisition(
		"acq-1", h.want, "acquire", transfer(infohash, "First.mkv"), "/local/1")); err != nil {
		t.Fatal(err)
	}
	// A different transfer for the same want: the want re-grabbed a new release.
	const second = "ffffffffffffffffffffffffffffffffffffffff"
	created, err := h.cat.RecordAcquisition(ctx, catalog.TransferToAcquisition(
		"acq-2", h.want, "acquire", transfer(second, "Second.mkv"), "/local/2"))
	if err != nil {
		t.Fatalf("re-grabbing a new release for the want was refused: %v", err)
	}
	if !created {
		t.Error("the replacing transfer is a new transfer — created should be true")
	}

	// Exactly one row for the want, and it is the second transfer.
	all, err := h.cat.Acquisitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("%d acquisition rows for one want, want exactly 1", len(all))
	}
	got, err := h.cat.AcquisitionFor(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExternalID != second {
		t.Errorf("the want points at %q, want the second transfer %q", got.ExternalID, second)
	}
}

// A want with nothing in flight is a typed answer, not a bare sql error.
func TestAnAbsentAcquisitionIsTyped(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if _, err := h.cat.AcquisitionFor(context.Background(), h.want); !errors.Is(err, catalog.ErrNoAcquisitionRow) {
		t.Errorf("expected ErrNoAcquisitionRow, got %v", err)
	}
}

// Dropping the link does not touch the download client. Forgetting about a
// transfer and deleting its bytes are different decisions.
func TestDroppingAnAcquisitionLeavesTheTransferAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.cat.RecordAcquisition(ctx, catalog.TransferToAcquisition(
		"acq-1", h.want, "acquire", transfer(infohash, "x.mkv"), "/local/x")); err != nil {
		t.Fatal(err)
	}
	if err := h.cat.DropAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cat.AcquisitionFor(ctx, h.want); !errors.Is(err, catalog.ErrNoAcquisitionRow) {
		t.Errorf("the row survived the drop: %v", err)
	}
}

// A conditional drop removes the row only while it names that transfer, so a
// row a newer grab wrote in its place survives (#819).
func TestDropAcquisitionOfSparesAReplacementRow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	other := strings.Repeat("f", 40)

	if _, err := h.cat.RecordAcquisition(ctx, catalog.TransferToAcquisition(
		"acq-1", h.want, "acquire", transfer(other, "replacement.mkv"), "/local/r")); err != nil {
		t.Fatal(err)
	}
	if dropped, err := h.cat.DropAcquisitionOf(ctx, h.want, "acquire", infohash); err != nil || dropped {
		t.Fatalf("dropped = %v (%v): a stale transfer id removed the replacement's row", dropped, err)
	}
	if _, err := h.cat.AcquisitionFor(ctx, h.want); err != nil {
		t.Fatalf("the replacement row is gone: %v", err)
	}
	if dropped, err := h.cat.DropAcquisitionOf(ctx, h.want, "acquire", other); err != nil || !dropped {
		t.Fatalf("dropped = %v (%v), want the named row removed", dropped, err)
	}
}

// A want's acquisition does not outlive it.
func TestDeletingAWantCascadesToItsAcquisition(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.cat.RecordAcquisition(ctx, catalog.TransferToAcquisition(
		"acq-1", h.want, "acquire", transfer(infohash, "x.mkv"), "/local/x")); err != nil {
		t.Fatal(err)
	}
	h.exec(t, `DELETE FROM desired_items WHERE id = ?`, h.want)

	all, err := h.cat.Acquisitions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Errorf("%d acquisition(s) outlived their want", len(all))
	}
}

// BlockTransferRelease blocks the selected release only while the want is in
// the phase the caller sampled and its row still names the transfer (#819), so
// a search running in parallel can never make it block a replacement.
func TestBlockTransferReleaseChecksTheWantIsStillTheTransfers(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) *harness {
		t.Helper()
		h := newHarness(t)
		ctx := context.Background()
		if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
			t.Fatal(err)
		}
		ranked := acquisition.EvaluateAll([]acquisition.ReleaseCandidate{
			release("good", 2160, "hevc"), release("plain", 1080, "h264"),
		}, candidateProfile())
		if _, err := h.cat.RecordSearch(ctx, h.want, ranked, acquisition.Incumbent{}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.cat.RecordAcquisition(ctx, catalog.TransferToAcquisition(
			"acq-1", h.want, "acquire", transfer(infohash, "x.exe"), "")); err != nil {
			t.Fatal(err)
		}
		if _, err := h.db.Writer().Exec(`UPDATE acquisition_state SET phase = 'downloading' WHERE desired_item_id = ?`, h.want); err != nil {
			t.Fatal(err)
		}
		return h
	}
	downloading := []acquisition.Phase{acquisition.PhaseDownloading}
	ctx := context.Background()

	h := setup(t)
	ours, err := h.cat.BlockTransferRelease(ctx, h.want, "acquire", infohash, downloading,
		"payload_executable: a program", catalog.BlockVerificationFailed)
	if err != nil || !ours {
		t.Fatalf("ours = %v (%v), want the transfer judged the want's", ours, err)
	}
	blocked, err := h.cat.BlockedFor(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 1 || blocked[0].CandidateID != "good" {
		t.Fatalf("blocked = %+v, want the selected release", blocked)
	}

	for name, call := range map[string]func(h *harness) (bool, error){
		"the phase moved on": func(h *harness) (bool, error) {
			return h.cat.BlockTransferRelease(ctx, h.want, "acquire", infohash,
				[]acquisition.Phase{acquisition.PhaseIdle}, "x", catalog.BlockVerificationFailed)
		},
		"the row names another transfer": func(h *harness) (bool, error) {
			return h.cat.BlockTransferRelease(ctx, h.want, "acquire", strings.Repeat("e", 40),
				downloading, "x", catalog.BlockVerificationFailed)
		},
	} {
		h := setup(t)
		ours, err := call(h)
		if err != nil || ours {
			t.Errorf("%s: ours = %v (%v), want false", name, ours, err)
		}
		if blocked, _ := h.cat.BlockedFor(ctx, h.want); len(blocked) != 0 {
			t.Errorf("%s: blocked %+v, want nothing written", name, blocked)
		}
	}
}

// A by-hand override while the transfer is in flight moves the selection but
// not the phase. The selection is then newer than the transfer's grab, so it is
// not the transfer's release and is not blocked (#819).
func TestBlockTransferReleaseSparesAnOverrideMadeInFlight(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	ranked := acquisition.EvaluateAll([]acquisition.ReleaseCandidate{
		release("good", 2160, "hevc"), release("plain", 1080, "h264"),
	}, candidateProfile())
	if _, err := h.cat.RecordSearch(ctx, h.want, ranked, acquisition.Incumbent{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cat.RecordAcquisition(ctx, catalog.TransferToAcquisition(
		"acq-1", h.want, "acquire", transfer(infohash, "x.exe"), "")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Writer().Exec(`UPDATE acquisition_state SET phase = 'downloading' WHERE desired_item_id = ?`, h.want); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cat.OverrideSelection(ctx, h.want, "plain"); err != nil {
		t.Fatal(err)
	}
	// The harness clock is fixed; make the override visibly later than the grab.
	if _, err := h.db.Writer().Exec(`UPDATE release_candidates SET selected_at = '2099-01-01T00:00:00Z'
		WHERE desired_item_id = ? AND selected = 1`, h.want); err != nil {
		t.Fatal(err)
	}

	ours, err := h.cat.BlockTransferRelease(ctx, h.want, "acquire", infohash,
		[]acquisition.Phase{acquisition.PhaseDownloading}, "x", catalog.BlockVerificationFailed)
	if err != nil || !ours {
		t.Fatalf("ours = %v (%v), want the transfer still the want's", ours, err)
	}
	if blocked, _ := h.cat.BlockedFor(ctx, h.want); len(blocked) != 0 {
		t.Errorf("blocked %+v: the override's pick is not the transfer's release", blocked)
	}
}

// A want that fails and re-selects the same release re-grabs the same transfer,
// so its row already exists. The grab restamps the row; otherwise the fresh
// selection reads as newer than the grab, nothing is ever blocked, and the want
// re-grabs the same dead release forever (#824). A poll keeps the stamp.
func TestBlockTransferReleaseBlocksARegrabOfTheSameTransfer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	downloading := []acquisition.Phase{acquisition.PhaseDownloading}

	for name, regrab := range map[string]bool{"a grab restamps": true, "a poll does not": false} {
		h := newHarness(t)
		if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
			t.Fatal(err)
		}
		ranked := acquisition.EvaluateAll([]acquisition.ReleaseCandidate{
			release("good", 2160, "hevc"), release("plain", 1080, "h264"),
		}, candidateProfile())
		if _, err := h.cat.RecordSearch(ctx, h.want, ranked, acquisition.Incumbent{}); err != nil {
			t.Fatal(err)
		}
		first := catalog.TransferToAcquisition("acq-1", h.want, "acquire", transfer(infohash, "x.mkv"), "")
		first.Grab = true
		if _, err := h.cat.RecordAcquisition(ctx, first); err != nil {
			t.Fatal(err)
		}
		// The first grab was weeks ago; the re-search that chose the same
		// release again came after it. The harness clock is fixed at a later
		// instant, which is when the second record happens.
		h.exec(t, `UPDATE acquisitions SET created_at = '2026-07-01T00:00:00Z' WHERE desired_item_id = ?`, h.want)
		h.exec(t, `UPDATE release_candidates SET selected_at = '2026-07-15T00:00:00Z'
			WHERE desired_item_id = ? AND selected = 1`, h.want)
		again := catalog.TransferToAcquisition("acq-2", h.want, "acquire", transfer(infohash, "x.mkv"), "")
		again.Grab = regrab
		if _, err := h.cat.RecordAcquisition(ctx, again); err != nil {
			t.Fatal(err)
		}
		h.exec(t, `UPDATE acquisition_state SET phase = 'downloading' WHERE desired_item_id = ?`, h.want)

		ours, err := h.cat.BlockTransferRelease(ctx, h.want, "acquire", infohash, downloading,
			"no_seeders: nothing for 6h", catalog.BlockGrabFailed)
		if err != nil || !ours {
			t.Fatalf("%s: ours = %v (%v), want the transfer judged the want's", name, ours, err)
		}
		blocked, err := h.cat.BlockedFor(ctx, h.want)
		if err != nil {
			t.Fatal(err)
		}
		if regrab && (len(blocked) != 1 || blocked[0].CandidateID != "good") {
			t.Errorf("%s: blocked = %+v, want the re-grabbed release", name, blocked)
		}
		if !regrab && len(blocked) != 0 {
			t.Errorf("%s: blocked = %+v, want the selection still newer than the grab", name, blocked)
		}
	}
}

// A selection that names the transfer's own infohash is that transfer's release
// whatever the stamps say, so it is blocked even when it looks newer (#824).
func TestBlockTransferReleaseBlocksASelectionNamingTheTransfer(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	ranked := acquisition.EvaluateAll([]acquisition.ReleaseCandidate{
		release("infohash:"+strings.ToUpper(infohash), 2160, "hevc"), release("plain", 1080, "h264"),
	}, candidateProfile())
	if _, err := h.cat.RecordSearch(ctx, h.want, ranked, acquisition.Incumbent{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cat.RecordAcquisition(ctx, catalog.TransferToAcquisition(
		"acq-1", h.want, "acquire", transfer(infohash, "x.mkv"), "")); err != nil {
		t.Fatal(err)
	}
	h.exec(t, `UPDATE acquisition_state SET phase = 'downloading' WHERE desired_item_id = ?`, h.want)
	h.exec(t, `UPDATE release_candidates SET selected_at = '2099-01-01T00:00:00Z'
		WHERE desired_item_id = ? AND selected = 1`, h.want)

	ours, err := h.cat.BlockTransferRelease(ctx, h.want, "acquire", infohash,
		[]acquisition.Phase{acquisition.PhaseDownloading}, "x", catalog.BlockGrabFailed)
	if err != nil || !ours {
		t.Fatalf("ours = %v (%v), want the transfer still the want's", ours, err)
	}
	if blocked, _ := h.cat.BlockedFor(ctx, h.want); len(blocked) != 1 {
		t.Errorf("blocked %+v, want the release that names the transfer", blocked)
	}
}
