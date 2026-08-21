package catalog_test

import (
	"context"
	"errors"
	"testing"

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

// One in-flight acquisition per want, enforced by the database rather than by
// the caller remembering.
func TestOneAcquisitionPerWant(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if _, err := h.cat.RecordAcquisition(ctx, catalog.TransferToAcquisition(
		"acq-1", h.want, "acquire", transfer(infohash, "First.mkv"), "/local/1")); err != nil {
		t.Fatal(err)
	}
	// A different transfer for the same want. Two things downloading for one
	// want means one of them is wasted bandwidth and neither is obviously the
	// right one.
	_, err := h.cat.RecordAcquisition(ctx, catalog.TransferToAcquisition(
		"acq-2", h.want, "acquire",
		transfer("ffffffffffffffffffffffffffffffffffffffff", "Second.mkv"), "/local/2"))
	if err == nil {
		t.Fatal("a second in-flight acquisition for one want was accepted")
	}
}

// A want with nothing in flight is a typed answer, not a bare sql error.
func TestAnAbsentAcquisitionIsTyped(t *testing.T) {
	h := newHarness(t)
	if _, err := h.cat.AcquisitionFor(context.Background(), h.want); !errors.Is(err, catalog.ErrNoAcquisitionRow) {
		t.Errorf("expected ErrNoAcquisitionRow, got %v", err)
	}
}

// Dropping the link does not touch the download client. Forgetting about a
// transfer and deleting its bytes are different decisions.
func TestDroppingAnAcquisitionLeavesTheTransferAlone(t *testing.T) {
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

// A want's acquisition does not outlive it.
func TestDeletingAWantCascadesToItsAcquisition(t *testing.T) {
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
