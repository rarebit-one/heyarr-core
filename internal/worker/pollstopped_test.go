package worker

import (
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// A transfer an operator paused after it ran is recorded as stopped on its row
// and is not trouble (#883): the poll leaves the want downloading on it, and
// the stop clears when the transfer runs again.
func TestAPausedTransferIsRecordedStoppedAndDoesNotFailItsWant(t *testing.T) {
	t.Parallel()
	h := newPollHarness(t)
	h.grabAfterSearch(t, "Arrival.2016.2160p.mkv", []byte("the bytes of a film"))
	id := h.transferID(t)
	if err := h.client.Progress(id, 5); err != nil {
		t.Fatal(err)
	}
	h.poll(t)

	if err := h.client.Stop(id); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		h.poll(t)
	}
	row, err := h.cat.AcquisitionFor(t.Context(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	if row.Stopped != providers.StoppedAfterRun || row.StoppedSince.IsZero() || row.Trouble != "" {
		t.Fatalf("row = (stopped %q since %v, trouble %q), want after_run with a since and no trouble",
			row.Stopped, row.StoppedSince, row.Trouble)
	}
	if got := h.state(t).Phase; got != acquisition.PhaseDownloading {
		t.Fatalf("phase = %s, want downloading: a stop is not a failure", got)
	}

	if err := h.client.Progress(id, 9); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Resume(id); err != nil {
		t.Fatal(err)
	}
	h.poll(t)
	if row, err := h.cat.AcquisitionFor(t.Context(), h.want); err != nil {
		t.Fatal(err)
	} else if row.Stopped != "" || !row.StoppedSince.IsZero() {
		t.Errorf("a resumed transfer still reads stopped %q since %v", row.Stopped, row.StoppedSince)
	}
}
