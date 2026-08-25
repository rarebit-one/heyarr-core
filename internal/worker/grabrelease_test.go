package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// grabHarness is a searchHarness with a download client registered.
//
// It reuses the search harness deliberately rather than seeding a SELECTED want
// directly: the thing under test is that the two halves connect, and a want put
// into SELECTED by hand would prove the grab works on a state the pipeline may
// never actually produce.
type grabHarness struct {
	*searchHarness
	client *providers.Fake
}

func newGrabHarness(t *testing.T) *grabHarness {
	t.Helper()
	h := newSearchHarness(t)
	client := providers.NewFake("client-a", providers.CapabilityDownload)
	if err := h.reg.Register(client); err != nil {
		t.Fatal(err)
	}
	return &grabHarness{searchHarness: h, client: client}
}

// grab runs the grab handler for whatever the want currently has selected.
func (h *grabHarness) grab(t *testing.T, candidateID string) error {
	t.Helper()
	payload, err := json.Marshal(acquisition.GrabPayload{
		DesiredItemID: h.want, CandidateID: candidateID,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := GrabReleaseHandler(h.reg, h.cat, slog.New(slog.DiscardHandler))
	return handler(t.Context(), jobs.Job{Type: acquisition.GrabJobType, Payload: payload})
}

// selectedID is the candidate the want is currently acquiring.
func (h *grabHarness) selectedID(t *testing.T) string {
	t.Helper()
	sel, err := h.cat.SelectedCandidate(t.Context(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	return sel.CandidateID
}

// pendingGrabs is how many grab jobs the queue holds for this want.
func (h *grabHarness) pendingGrabs(t *testing.T) int {
	t.Helper()
	var n int
	err := h.db.Reader().QueryRow(
		`SELECT count(*) FROM jobs WHERE type = ?`, acquisition.GrabJobType).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// THE assertion #225 exists for: a search that selects a release also arranges
// for it to be fetched.
//
// Before this, the search handler stopped at SELECTED and no job type existed
// that could take it further — so a want that had decided what it wanted could
// never act on the decision, and rested in SELECTED looking exactly like a
// transfer in flight.
func TestASearchQueuesAGrabForTheReleaseItSelected(t *testing.T) {
	h := newGrabHarness(t)
	h.fake.Offer("Arrival",
		offer("plain", 1080, "h264"),
		offer("good", 2160, "hevc"),
		offer("tiny", 480, "hevc"))

	if got := h.pendingGrabs(t); got != 0 {
		t.Fatalf("start: %d grab jobs already queued", got)
	}
	if err := h.run(t); err != nil {
		t.Fatal(err)
	}
	if got := h.state(t).Name(); got != "SELECTED" {
		t.Fatalf("after a search the want is %s, want SELECTED", got)
	}
	if got := h.pendingGrabs(t); got != 1 {
		t.Fatalf("the search queued %d grabs, want exactly 1", got)
	}
}

// And the grab moves the want on to QUEUED, which nothing could do before.
func TestAGrabHandsTheReleaseToAClientAndReachesQueued(t *testing.T) {
	h := newGrabHarness(t)
	h.fake.Offer("Arrival",
		offer("plain", 1080, "h264"),
		offer("good", 2160, "hevc"))
	if err := h.run(t); err != nil {
		t.Fatal(err)
	}

	chosen := h.selectedID(t)
	if err := h.grab(t, chosen); err != nil {
		t.Fatal(err)
	}

	if got := h.state(t).Name(); got != "QUEUED" {
		t.Fatalf("after a grab the want is %s, want QUEUED", got)
	}

	// What reached the client is the SELECTED candidate's source, not merely
	// some source. Asserting only that Add was called would pass for a grab
	// that fetched the wrong release — and the fixture has more than one
	// candidate precisely so that can be told apart.
	added := h.client.Added()
	if len(added) != 1 {
		t.Fatalf("the client recorded %d adds, want 1", len(added))
	}
	if got, want := added[0].Reveal(), "magnet:?xt=urn:btih:"+chosen; got != want {
		t.Errorf("the client was handed %q, want the selected release's source %q", got, want)
	}
}

// The acquisition row is written, or the transfer is invisible to the poll.
//
// This is the half that is easy to leave out and impossible to notice: the want
// would reach QUEUED, the client would really be downloading, and
// poll_downloads would refuse to adopt the transfer because it has no row for
// it — by design, since adopting unknown labelled transfers is how one Heyarr
// would attach another's work to its own want. The want would then never leave
// QUEUED, which is the same silent resting state one phase further along.
func TestAGrabRecordsTheAcquisitionSoThePollCanFindIt(t *testing.T) {
	h := newGrabHarness(t)
	h.fake.Offer("Arrival", offer("good", 2160, "hevc"))
	if err := h.run(t); err != nil {
		t.Fatal(err)
	}
	if err := h.grab(t, h.selectedID(t)); err != nil {
		t.Fatal(err)
	}

	added := h.client.Added()
	if len(added) != 1 {
		t.Fatalf("the client recorded %d adds, want 1", len(added))
	}

	row, err := h.cat.AcquisitionFor(t.Context(), h.want)
	if err != nil {
		t.Fatalf("no acquisition row after a grab: %v", err)
	}
	if row.Provider != "client-a" {
		t.Errorf("the row names %q as the client, want client-a", row.Provider)
	}
	// The row's external id has to be the one the CLIENT returned, because
	// that is the value poll_downloads looks transfers up by. A row carrying
	// anything else would be a row the poll can never match.
	if row.ExternalID == "" {
		t.Fatal("the acquisition row carries no external id, so the poll cannot match it")
	}
	found, err := h.cat.AcquisitionByExternal(t.Context(), "client-a", row.ExternalID)
	if err != nil {
		t.Fatalf("the poll's own lookup cannot find the row it needs: %v", err)
	}
	if found.DesiredItemID != h.want {
		t.Errorf("the lookup found want %s, want %s", found.DesiredItemID, h.want)
	}
}

// A grab will be re-run (invariant 9), and a re-run must not start a second
// transfer or move the want twice.
func TestAGrabRunTwiceStartsOneTransfer(t *testing.T) {
	h := newGrabHarness(t)
	h.fake.Offer("Arrival", offer("good", 2160, "hevc"))
	if err := h.run(t); err != nil {
		t.Fatal(err)
	}
	chosen := h.selectedID(t)

	if err := h.grab(t, chosen); err != nil {
		t.Fatal(err)
	}
	// The second run is the one under test. It must succeed — a re-run that
	// errored would be retried forever on a backoff — and change nothing.
	if err := h.grab(t, chosen); err != nil {
		t.Fatalf("the second grab failed: %v", err)
	}

	if got := len(h.client.Added()); got != 1 {
		t.Errorf("the client was asked to add %d times, want 1", got)
	}
	if got := h.state(t).Name(); got != "QUEUED" {
		t.Errorf("after two grabs the want is %s, want QUEUED", got)
	}
}

// A candidate the indexer offered no way to fetch must not leave the want
// resting in SELECTED — which is the exact shape of the defect.
//
// Retrying cannot help: nothing about this candidate will change. So the want
// is failed, which both moves it out of SELECTED and blocks the release, so the
// next search does not choose the same unfetchable thing again.
func TestAReleaseWithNoSourceFailsTheWantRatherThanRestingInSelected(t *testing.T) {
	h := newGrabHarness(t)
	h.fake.Offer("Arrival", offerWithoutSource("good", 2160, "hevc"))
	if err := h.run(t); err != nil {
		t.Fatal(err)
	}
	if got := h.state(t).Name(); got != "SELECTED" {
		t.Fatalf("after a search the want is %s, want SELECTED", got)
	}

	if err := h.grab(t, h.selectedID(t)); err != nil {
		t.Fatalf("an unfetchable release failed the job: %v", err)
	}

	if got := h.state(t).Name(); got == "SELECTED" {
		t.Fatal("the want is still SELECTED, which is indistinguishable from a transfer in flight")
	}
	if got := len(h.client.Added()); got != 0 {
		t.Errorf("a client was asked %d times for a release with no source", got)
	}
}

// A client that is down leaves the want in SELECTED and FAILS the job, so the
// reason reaches the queue's last_error.
//
// The opposite treatment from the no-source case above, and deliberately so:
// this one will work on a retry, so the want keeps its selection and the job
// backs off. What must not happen is the quiet success that made #225 invisible
// — a handler that returned nil here would leave the want in SELECTED with
// nothing anywhere recording why.
func TestAGrabAgainstADownClientFailsLoudlyAndKeepsTheSelection(t *testing.T) {
	h := newGrabHarness(t)
	h.client.FailWith(errors.New("the client is restarting"))
	h.fake.Offer("Arrival", offer("good", 2160, "hevc"))
	if err := h.run(t); err != nil {
		t.Fatal(err)
	}

	err := h.grab(t, h.selectedID(t))
	if err == nil {
		t.Fatal("a grab against a down client reported success")
	}
	if !strings.Contains(err.Error(), "client-a") {
		t.Errorf("the error does not name the client that refused: %v", err)
	}
	if got := h.state(t).Name(); got != "SELECTED" {
		t.Errorf("the want is %s, want SELECTED so the retry still has its selection", got)
	}
}

// A grab enqueued for one release must not fetch a different one.
//
// The window is real: a search can supersede a selection between the grab being
// queued and being claimed. Fetching whatever is selected now would be
// defensible, but it would make the job's own record of what it was for untrue,
// and the grab queued for the NEW selection is the one that should do the work.
func TestAGrabSkipsWhenTheSelectionChangedUnderIt(t *testing.T) {
	h := newGrabHarness(t)
	h.fake.Offer("Arrival", offer("good", 2160, "hevc"))
	if err := h.run(t); err != nil {
		t.Fatal(err)
	}

	// A candidate id that is NOT the one selected — as if this job had been
	// queued for an earlier search's winner.
	if err := h.grab(t, "a-superseded-release"); err != nil {
		t.Fatalf("a superseded grab failed rather than standing down: %v", err)
	}

	if got := len(h.client.Added()); got != 0 {
		t.Errorf("the client was asked %d times for a superseded release", got)
	}
	if got := h.state(t).Name(); got != "SELECTED" {
		t.Errorf("the want is %s, want SELECTED", got)
	}
}

// The source is a credential and must not reach the job payload.
//
// The payload is stored, listed by the API and read by anybody debugging the
// queue. This is asserted against the row the search actually wrote rather than
// against the struct, because the struct having no field is a fact about today
// and the row is what would leak.
func TestAQueuedGrabDoesNotCarryTheSourceInItsPayload(t *testing.T) {
	h := newGrabHarness(t)
	const passkey = "DO-NOT-LEAK-4f2a"
	c := offer("good", 2160, "hevc")
	c.Source = secret.Value("magnet:?xt=urn:btih:good&passkey=" + passkey)
	h.fake.Offer("Arrival", c)

	if err := h.run(t); err != nil {
		t.Fatal(err)
	}

	var payload string
	err := h.db.Reader().QueryRow(
		`SELECT payload FROM jobs WHERE type = ?`, acquisition.GrabJobType).Scan(&payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, passkey) {
		t.Fatalf("the source reached the job payload: %s", payload)
	}
}

// And it must not reach the candidate listing the API returns.
//
// `source` is deliberately absent from candidateCols for this reason, so a
// candidate view cannot carry it even by accident.
func TestTheSourceIsNotInWhatTheAPIReturnsForACandidate(t *testing.T) {
	h := newGrabHarness(t)
	const passkey = "DO-NOT-LEAK-4f2a"
	c := offer("good", 2160, "hevc")
	c.Source = secret.Value("magnet:?xt=urn:btih:good&passkey=" + passkey)
	h.fake.Offer("Arrival", c)

	if err := h.run(t); err != nil {
		t.Fatal(err)
	}

	listed, err := h.cat.CandidatesFor(context.Background(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) == 0 {
		t.Fatal("no candidates were stored, so this asserts nothing")
	}
	encoded, err := json.Marshal(listed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), passkey) {
		t.Fatalf("the source reached a candidate listing: %s", encoded)
	}

	// The control: the source really was stored, so the assertion above is
	// about the listing rather than about a value that was never saved.
	_, stored, err := h.cat.SelectedSource(context.Background(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored.Reveal(), passkey) {
		t.Fatal("the source was not stored at all, so the leak assertion is vacuous")
	}
}
