package worker

import (
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/downloads"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
)

// pollHarness drives a want through the WHOLE acquisition arc with a download
// client that moves real bytes — search, grab, poll — rather than applying
// transitions by hand.
//
// The hand-applied setup the other tests use is what let advancePipeline go
// untested: every one of them puts the want into VERIFYING itself, so the code
// that is supposed to get it there never runs.
type pollHarness struct {
	*ingestHarness
	client *downloads.Fake
}

func newPollHarness(t *testing.T) *pollHarness {
	t.Helper()
	h := newIngestHarness(t)
	client := downloads.NewFake("fake-downloader", h.downloads)
	if err := h.reg.Register(client); err != nil {
		t.Fatal(err)
	}
	return &pollHarness{ingestHarness: h, client: client}
}

// grabAfterSearch takes the want to QUEUED the way the pipeline does.
func (h *pollHarness) grabAfterSearch(t *testing.T, name string, content []byte) {
	t.Helper()
	src := secret.Value("magnet:?xt=urn:btih:good")
	h.client.Offer(src, name, content)
	h.fake.Offer("Arrival", offer("good", 2160, "hevc"))

	if err := h.run(t); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(acquisition.GrabPayload{DesiredItemID: h.want})
	if err != nil {
		t.Fatal(err)
	}
	handler := GrabReleaseHandler(h.reg, h.cat, slog.New(slog.DiscardHandler))
	if err := handler(t.Context(), jobs.Job{Type: acquisition.GrabJobType, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if got := h.state(t).Phase; got != acquisition.PhaseQueued {
		t.Fatalf("setup: phase is %s, expected queued", got)
	}
}

// transferID is the client's own id for this want's transfer, read from the
// acquisition row the grab wrote — which is also the value poll_downloads
// matches on, so a test using it is exercising the same join production does.
func (h *pollHarness) transferID(t *testing.T) string {
	t.Helper()
	row, err := h.cat.AcquisitionFor(t.Context(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	return row.ExternalID
}

func (h *pollHarness) poll(t *testing.T) {
	t.Helper()
	handler := PollDownloadsHandler(h.reg, h.cat, h.queue, slog.New(slog.DiscardHandler))
	if err := handler(t.Context(), jobs.Job{Type: downloads.PollJobType}); err != nil {
		t.Fatal(err)
	}
}

// A transfer that FINISHES BETWEEN TWO POLL PASSES must not strand its want.
//
// advancePipeline picks ONE transition from the transfer's current state: a
// done transfer asks for `downloaded`, which is legal only from DOWNLOADING.
// A want sitting in QUEUED never saw the intermediate `start_download`, so the
// edge is refused — and the refusal is deliberately silent, because on a repeat
// pass over an already-handled transfer it is the normal case.
//
// The result is a want in QUEUED with a finished download on disk and nothing
// that will ever move it. It looks exactly like a transfer still running.
//
// This is reachable with any client and any small release: the poll runs on a
// beat, and a download that starts and finishes inside one interval is never
// observed in progress. It was NOT reachable before #225, because nothing put a
// want into QUEUED at all — which is why it has survived until now.
func TestATransferThatCompletesBetweenPollsDoesNotStrandItsWant(t *testing.T) {
	h := newPollHarness(t)
	h.grabAfterSearch(t, "Arrival.2016.2160p.mkv", []byte("the bytes of a film"))

	// No intermediate Progress call: the transfer goes from queued to finished
	// without ever being observed downloading, which is what a fast release
	// does on a beat-driven poll.
	if _, err := h.client.Complete(h.transferID(t)); err != nil {
		t.Fatal(err)
	}

	h.poll(t)

	if got := h.state(t).Phase; got == acquisition.PhaseQueued {
		t.Fatal("the want is still QUEUED with a completed download on disk — " +
			"nothing will ever move it, and it looks like a transfer in flight")
	}
	if got := h.state(t).Phase; got != acquisition.PhaseVerifying {
		t.Errorf("phase = %s, want verifying so the ingest can be scheduled", got)
	}
}

// The ordinary path — observed downloading, then finished — still works.
//
// The control: a fix that walked every edge unconditionally would also pass the
// test above while double-advancing a want that was already DOWNLOADING.
func TestATransferObservedInProgressStillWalksOneEdgeAtATime(t *testing.T) {
	h := newPollHarness(t)
	h.grabAfterSearch(t, "Arrival.2016.2160p.mkv", []byte("the bytes of a film"))
	id := h.transferID(t)

	if err := h.client.Progress(id, 4); err != nil {
		t.Fatal(err)
	}
	h.poll(t)
	if got := h.state(t).Phase; got != acquisition.PhaseDownloading {
		t.Fatalf("phase = %s, want downloading", got)
	}

	if _, err := h.client.Complete(id); err != nil {
		t.Fatal(err)
	}
	h.poll(t)
	if got := h.state(t).Phase; got != acquisition.PhaseVerifying {
		t.Errorf("phase = %s, want verifying", got)
	}
}
