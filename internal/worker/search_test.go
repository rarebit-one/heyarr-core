package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The search job, end to end against the FAKE indexer (§60, §63, M3-12).
//
// This is the milestone's central claim made executable: Heyarr decides what
// should exist and explains its choice with no real indexer anywhere. If the
// job could not be driven by a fake, ADR-0026's values-in-values-out interface
// would have failed at its one job — a fake, a fixture replayer and a real
// client are supposed to be indistinguishable to every caller, and this is the
// caller that matters most.

type searchHarness struct {
	db *sqlite.DB
	// queue is REAL rather than a stub, so that "the search enqueued a grab"
	// is asserted against the same table the worker claims from. A stub would
	// prove the handler called something, which is a claim about the handler
	// rather than about the pipeline (#225).
	queue *jobs.Queue
	cat   *catalog.Catalog
	reg   *providers.Registry
	fake  *providers.Fake
	want  string
}

func newSearchHarness(t *testing.T) *searchHarness {
	t.Helper()
	ctx := t.Context()
	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(t.TempDir(), "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{
		DB: db, Events: eventLog, PeerName: "test", PeerSite: "test-site",
	})
	if err != nil {
		t.Fatal(err)
	}

	queue, err := jobs.New(jobs.Options{
		Writer: db.Writer(), Reader: db.Reader(), Events: eventLog,
	})
	if err != nil {
		t.Fatal(err)
	}

	h := &searchHarness{
		db: db, queue: queue, cat: cat, want: uuid.Must(uuid.NewV7()).String(),
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)

	profile := policy.Profile{
		Name: "living-room",
		Accept: []policy.Rule{
			{Attribute: policy.AttrResolution, Op: policy.OpGTE, Value: policy.Num(1080)},
		},
		Prefer: []policy.Rule{
			{Attribute: policy.AttrVideoCodec, Op: policy.OpEq, Value: policy.Text("hevc"), Weight: 20},
		},
	}
	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
	accept, _ := json.Marshal(profile.Accept)
	prefer, _ := json.Marshal(profile.Prefer)

	h.exec(t, `INSERT INTO quality_profiles
		(id, name, description, accept, prefer, terminal, seeded, created_at, updated_at)
		VALUES ('q1', 'living-room', '', ?, ?, '[]', 1, ?, ?)`,
		string(accept), string(prefer), stamp, stamp)
	h.exec(t, `INSERT INTO works
		(id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES ('w1', 'movie', 'movie:arrival:2016', 'Arrival', 'arrival', 2016, '{}', ?, ?)`,
		stamp, stamp)
	h.exec(t, `INSERT INTO desired_items
		(id, scope, work_id, edition_id, quality_profile_id, monitor, reason, created_at, updated_at)
		VALUES (?, 'work', 'w1', NULL, 'q1', 1, '', ?, ?)`, h.want, stamp, stamp)

	if _, err := cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}

	h.fake = providers.NewFake("fake-indexer", providers.CapabilityIndexer)
	h.reg = providers.New(nil)
	if err := h.reg.Register(h.fake); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *searchHarness) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := h.db.Writer().Exec(query, args...); err != nil {
		t.Fatalf("seeding (%s): %v", query, err)
	}
}

func (h *searchHarness) run(t *testing.T) error {
	t.Helper()
	payload, err := json.Marshal(acquisition.SearchPayload{DesiredItemID: h.want})
	if err != nil {
		t.Fatal(err)
	}
	handler := SearchHandler(h.reg, h.cat, h.queue, slog.New(slog.DiscardHandler))
	return handler(t.Context(), jobs.Job{Type: acquisition.SearchJobType, Payload: payload})
}

func (h *searchHarness) state(t *testing.T) acquisition.State {
	t.Helper()
	rec, err := h.cat.Acquisition(context.Background(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	return rec.State
}

func offer(id string, resolution int64, codec string) acquisition.ReleaseCandidate {
	c := offerWithoutSource(id, resolution, codec)
	// Every ordinary candidate carries a source, because every real one does:
	// an indexer that lists a release also says where to get it. The
	// sourceless case is a real but exceptional shape and has its own helper,
	// so that a test needing it has to ask for it rather than getting it by
	// forgetting a field (#225).
	c.Source = secret.Value("magnet:?xt=urn:btih:" + id)
	return c
}

// offerWithoutSource is a candidate the indexer listed and offered no way to
// fetch — a catalogue entry rather than a download.
func offerWithoutSource(id string, resolution int64, codec string) acquisition.ReleaseCandidate {
	return acquisition.ReleaseCandidate{
		ID: id, Title: "Arrival " + id, Provider: "fake-indexer",
		Attributes: acquisition.Attributes{
			policy.AttrResolution: policy.Num(resolution),
			policy.AttrVideoCodec: policy.Text(codec),
		},
	}
}

// THE assertion this issue exists for: MISSING → SEARCHING → CANDIDATES_FOUND
// → SELECTED, with no real indexer anywhere.
func TestASearchWalksTheMachineToSelected(t *testing.T) {
	h := newSearchHarness(t)
	h.fake.Offer("Arrival",
		offer("good", 2160, "hevc"),
		offer("plain", 1080, "h264"),
		offer("tiny", 480, "hevc"))

	if got := h.state(t).Name(); got != "MISSING" {
		t.Fatalf("start = %s", got)
	}
	if err := h.run(t); err != nil {
		t.Fatal(err)
	}
	if got := h.state(t).Name(); got != "SELECTED" {
		t.Fatalf("after a search the want is %s, want SELECTED", got)
	}
	if h.fake.Searches() != 1 {
		t.Errorf("the indexer was asked %d times", h.fake.Searches())
	}

	sel, err := h.cat.SelectedCandidate(t.Context(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	if sel.CandidateID != "good" {
		t.Errorf("selected %s, want good", sel.CandidateID)
	}
	if !sel.Evaluation.Accepted {
		t.Error("the selected candidate must be one the profile accepts")
	}
}

// Zero candidates is a modelled edge, not a failure — and the handler must not
// return an error, or the job backs off and an unavailable release becomes an
// indexer hammering loop.
func TestAnEmptySearchIsNotAJobFailure(t *testing.T) {
	h := newSearchHarness(t)
	// The fake is offered nothing, so it answers nothing.

	if err := h.run(t); err != nil {
		t.Fatalf("an empty search must not fail the job: %v", err)
	}
	if got := h.state(t).Name(); got != "MISSING" {
		t.Errorf("after an empty search the want is %s, want MISSING", got)
	}
}

// Twelve candidates, none acceptable: the rejections stay, the want returns to
// rest, and the job succeeds.
func TestTwelveUnacceptableCandidatesLeaveTwelveExplanations(t *testing.T) {
	h := newSearchHarness(t)
	var offers []acquisition.ReleaseCandidate
	for i := range 12 {
		offers = append(offers, offer(fmt.Sprintf("c%02d", i), int64(480+i*10), "hevc"))
	}
	h.fake.Offer("Arrival", offers...)

	if err := h.run(t); err != nil {
		t.Fatalf("finding nothing acceptable must not fail the job: %v", err)
	}
	if got := h.state(t).Name(); got != "MISSING" {
		t.Errorf("state = %s; nothing was acceptable, so the want returns to rest", got)
	}

	stored, err := h.cat.CandidatesFor(t.Context(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 12 {
		t.Fatalf("%d explanations for 12 candidates", len(stored))
	}
	for _, c := range stored {
		if len(c.Evaluation.RejectedBy()) == 0 {
			t.Errorf("%s was rejected with no reason", c.CandidateID)
		}
	}
}

// One indexer being down must not discard what the others returned, and must
// not be silent either (§60 keeps operational visibility).
func TestOneFailingIndexerDoesNotDiscardTheOthers(t *testing.T) {
	h := newSearchHarness(t)
	h.fake.Offer("Arrival", offer("good", 2160, "hevc"))

	broken := providers.NewFake("broken-indexer", providers.CapabilityIndexer)
	broken.FailWith(fmt.Errorf("connection refused"))
	if err := h.reg.Register(broken); err != nil {
		t.Fatal(err)
	}

	if err := h.run(t); err != nil {
		t.Fatalf("a partial answer is not a failure: %v", err)
	}
	if got := h.state(t).Name(); got != "SELECTED" {
		t.Errorf("state = %s; the working indexer's answer should still be used", got)
	}
}

// The job WILL be re-run (invariant 9). Re-running it over the same answers
// produces the same rows rather than duplicates.
func TestReRunningTheSearchDoesNotDuplicate(t *testing.T) {
	h := newSearchHarness(t)
	h.fake.Offer("Arrival", offer("good", 2160, "hevc"), offer("plain", 1080, "h264"))

	if err := h.run(t); err != nil {
		t.Fatal(err)
	}
	first, err := h.cat.CandidatesFor(t.Context(), h.want)
	if err != nil {
		t.Fatal(err)
	}

	// A second run is refused by the state machine — the want is SELECTED, and
	// searching from there is not a legal transition — so it is a no-op rather
	// than a duplicate. That refusal is the idempotency.
	if err := h.run(t); err != nil {
		t.Fatalf("a re-run must not fail: %v", err)
	}
	second, err := h.cat.CandidatesFor(t.Context(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != len(first) {
		t.Errorf("a re-run changed the candidate count from %d to %d", len(first), len(second))
	}
}

// A want that is already acquiring must not be searched over the top of itself.
func TestASearchSkipsAWantThatIsAlreadyInFlight(t *testing.T) {
	h := newSearchHarness(t)
	h.fake.Offer("Arrival", offer("good", 2160, "hevc"))

	// Drive it into DOWNLOADING by hand.
	for _, tr := range []acquisition.Transition{
		acquisition.TransitionSearch,
		acquisition.TransitionCandidatesFound,
		acquisition.TransitionSelect,
		acquisition.TransitionQueue,
		acquisition.TransitionStartDownload,
	} {
		if _, err := h.cat.AdvanceAcquisition(t.Context(), h.want, tr, ""); err != nil {
			t.Fatal(err)
		}
	}

	if err := h.run(t); err != nil {
		t.Fatalf("skipping is not a failure: %v", err)
	}
	if got := h.state(t).Name(); got != "DOWNLOADING" {
		t.Errorf("state = %s; a search must not disturb a transfer in flight", got)
	}
	if h.fake.Searches() != 0 {
		t.Errorf("the indexer was asked %d times for a want already downloading",
			h.fake.Searches())
	}
}

// The stored evaluation is the evaluator's, driven all the way through the job
// rather than only through the catalog.
func TestTheJobStoresTheEvaluatorsOwnAnswer(t *testing.T) {
	h := newSearchHarness(t)
	offers := []acquisition.ReleaseCandidate{
		offer("good", 2160, "hevc"),
		offer("plain", 1080, "h264"),
	}
	h.fake.Offer("Arrival", offers...)
	if err := h.run(t); err != nil {
		t.Fatal(err)
	}

	sc, err := h.cat.SearchContextFor(t.Context(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]acquisition.Evaluation{}
	for _, r := range acquisition.EvaluateAll(offers, sc.Profile) {
		want[r.Candidate.ID] = r.Evaluation
	}

	stored, err := h.cat.CandidatesFor(t.Context(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range stored {
		expected, ok := want[c.CandidateID]
		if !ok {
			t.Errorf("stored an unexpected candidate %s", c.CandidateID)
			continue
		}
		gotJSON, _ := json.Marshal(c.Evaluation)
		wantJSON, _ := json.Marshal(expected)
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("%s: stored evaluation differs from the evaluator's\n got: %s\nwant: %s",
				c.CandidateID, gotJSON, wantJSON)
		}
	}
}

// A malformed payload is a programming error and must fail loudly rather than
// searching for nothing.
func TestAnUndecodablePayloadFails(t *testing.T) {
	h := newSearchHarness(t)
	handler := SearchHandler(h.reg, h.cat, h.queue, slog.New(slog.DiscardHandler))
	err := handler(t.Context(), jobs.Job{
		Type: acquisition.SearchJobType, Payload: []byte(`{"desired_item_id":`),
	})
	if err == nil || !strings.Contains(err.Error(), "not decodable") {
		t.Fatalf("expected a decode failure, got %v", err)
	}

	err = handler(t.Context(), jobs.Job{
		Type: acquisition.SearchJobType, Payload: []byte(`{}`),
	})
	if err == nil || !strings.Contains(err.Error(), "needs a desired item") {
		t.Fatalf("expected a refusal naming the missing field, got %v", err)
	}
}

// detailOf is the want's durable explanation — what an operator reads days
// later, as opposed to the log line, which is gone.
func (h *searchHarness) detailOf(t *testing.T) string {
	t.Helper()
	rec, err := h.cat.Acquisition(t.Context(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	return rec.Detail
}

// "Nothing was found" and "we could not look" must not read the same.
//
// One indexer answered with nothing; the other could not be reached at all. The
// want used to record "2 indexer(s) had nothing", because Consulted counts
// every indexer that was ASKED, including the ones that failed — so a want
// whose only healthy indexer came up empty was indistinguishable from a want
// where two looked properly and the content genuinely is not out there.
//
// The two lead to opposite actions: wait, or go fix the indexer.
func TestAWantSaysWhenAnIndexerCouldNotBeReached(t *testing.T) {
	h := newSearchHarness(t)
	// The healthy one answers with nothing; it has no offer for this title.
	down := providers.NewFake("down-indexer", providers.CapabilityIndexer).
		FailWith(errors.New("context deadline exceeded"))
	if err := h.reg.Register(down); err != nil {
		t.Fatal(err)
	}

	if err := h.run(t); err != nil {
		t.Fatal(err)
	}

	detail := h.detailOf(t)
	if !strings.Contains(detail, "could not be reached") {
		t.Errorf("the want records %q, which reads as a definitive negative "+
			"answer when one indexer never answered at all", detail)
	}
	// And it still says what the working one found, or the operator cannot
	// tell this from a total outage.
	if !strings.Contains(detail, "1 of 2") {
		t.Errorf("the want records %q, which does not say how many actually looked", detail)
	}
}

// A search where every indexer answered still reads the way it always did.
//
// The control: without it, a fix that always mentioned failures would pass the
// test above while making the ordinary case wrong.
func TestAWantWithHealthyIndexersRecordsAPlainEmptySearch(t *testing.T) {
	h := newSearchHarness(t)

	if err := h.run(t); err != nil {
		t.Fatal(err)
	}

	detail := h.detailOf(t)
	if strings.Contains(detail, "could not be reached") {
		t.Errorf("the want records %q, but every indexer answered", detail)
	}
	if !strings.Contains(detail, "had nothing") {
		t.Errorf("the want records %q, want a plain empty-search detail", detail)
	}
}

// A search where NO indexer could be reached is not a search that found
// nothing.
//
// Recording TransitionNoCandidates there advances the want's search schedule on
// the strength of a look nobody took, so a want whose indexers are all down
// waits out the full `missing` cadence having learned nothing (#130).
func TestASearchThatReachedNobodyIsNotAnEmptySearch(t *testing.T) {
	h := newSearchHarness(t)
	h.fake.FailWith(errors.New("context deadline exceeded"))

	err := h.run(t)
	if err == nil {
		t.Fatal("a search that reached no indexer reported success, so the job " +
			"will not retry and the want waits out its full cadence")
	}

	detail := h.detailOf(t)
	if !strings.Contains(detail, "no indexer could be reached") {
		t.Errorf("the want records %q, want it to say nobody was reached", detail)
	}
	// It must still LEAVE searching, or it sticks there forever.
	if got := h.state(t).Phase; got == acquisition.PhaseSearching {
		t.Error("the want is stuck in SEARCHING")
	}
}

// seedHeldAsset puts one asset under the want's work and gives it a probe, so
// that reconciliation can evaluate it against the profile.
//
// The probe is the point: §62's resolution and codec attributes come from
// blob_probes, not from the filename, so an asset without one is UNDETERMINED
// against every rule and can never satisfy anything. A fixture that skipped it
// would produce a want that looks held and reconciles to not_satisfied, which
// is a confusing way to fail.
func (h *searchHarness) seedHeldAsset(t *testing.T, id, codec string, height int64) {
	t.Helper()
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	// A real-shaped digest: the blobs table CHECKs `blake3:` plus 64 hex
	// characters, which is the invariant-1 constraint doing its job on a
	// fixture. Derived from the id so two seeded assets differ.
	sum := sha256.Sum256([]byte(id))
	hash := "blake3:" + hex.EncodeToString(sum[:])
	h.exec(t, `INSERT INTO blobs (hash, size, mime, first_seen_at)
		VALUES (?, 8589934592, 'video/x-matroska', ?)`, hash, stamp)
	h.exec(t, `INSERT INTO editions
			(id, work_id, label, edition_key, edition_type, language, attributes, created_at)
		VALUES (?, 'w1', ?, ?, 'remux', 'en', '{}', ?)`,
		"e-"+id, id, id, stamp)
	h.exec(t, `INSERT INTO assets (id, edition_id, library_id, source_class, blob_hash,
			source_path, role, filename, mime, identification_source, created_at, updated_at)
		VALUES (?, ?, NULL, 'managed', ?, ?, 'primary', 'held.mkv',
			'video/x-matroska', 'path-heuristic', ?, ?)`,
		"a-"+id, "e-"+id, hash, "/srv/"+id+".mkv", stamp, stamp)
	h.exec(t, `INSERT INTO blob_probes
			(blob_hash, container, format_long, duration_seconds, bitrate_bps, streams,
			 bytes_read, materialised, probed_at)
		VALUES (?, 'matroska,webm', '', 7200.0, 8000000, ?, 1024, 0, ?)`,
		hash, `[{"type":"video","codec":"`+codec+`","height":`+
			strconv.FormatInt(height, 10)+`,"profile":"High"},`+
			`{"type":"audio","codec":"aac","channels":6}]`, stamp)
}

// A satisfied want is not dragged backwards by its own next search.
//
// # The sequence this reproduces
//
// A want becomes satisfied. Twenty-seven seconds later the search beat looks at
// it — correctly, it is monitored — and the indexer still offers a release
// scoring exactly what is held. Before this, the evaluator accepted that
// candidate, the handler selected it, and the want left CONTENT_SATISFIED for
// SELECTED: §64's name for "a release has been chosen and not yet fetched",
// reported about bytes that are already on disk.
//
// Every ingredient was individually right. The beat is right to schedule
// monitored wants; the evaluator is right that the candidate is acceptable; the
// satisfaction axes are right that content is satisfied. The regression appears
// only when a want is satisfied AND monitored AND re-searched — the ordinary
// steady state of every want after its first success, and a combination nothing
// in CI exercised.
func TestASatisfiedWantIsNotDraggedBackwardsByItsOwnNextSearch(t *testing.T) {
	h := newSearchHarness(t)
	h.seedHeldAsset(t, "held", "hevc", 2160)
	if _, err := h.cat.ReconcileDesired(t.Context(), h.want); err != nil {
		t.Fatal(err)
	}
	before := h.state(t)
	if before.Content != acquisition.SatisfactionSatisfied {
		t.Fatalf("the want is %s before the search, so this asserts nothing", before.Name())
	}

	// On offer: a release scoring exactly what is held. Not worse, not better —
	// equivalent, which is what an indexer returns on the next pass for the
	// release the want was satisfied by in the first place.
	h.fake.Offer("Arrival", offer("equivalent", 2160, "hevc"))
	if err := h.run(t); err != nil {
		t.Fatal(err)
	}

	after := h.state(t)
	if after.Phase == acquisition.PhaseSelected {
		t.Errorf("the want regressed to SELECTED — it now reports a release chosen " +
			"and not yet fetched, about bytes it already holds")
	}
	if after.Content != acquisition.SatisfactionSatisfied {
		t.Errorf("content went from satisfied to %s", after.Content)
	}

	// And the ROW agrees with the phase.
	//
	// This assertion exists because the first version of this fix did not
	// satisfy it. Suppressing only the state transition left the candidate
	// recorded with selected = 1, so the want's phase said "not acquiring
	// anything" while release_candidates said "acquiring this" — and that
	// column's entire meaning is "what this want is currently acquiring".
	//
	// A sabotage found it: making a satisfied want never select again broke no
	// test, because the row was being written either way and the test read the
	// row. The two facts have to be decided together, which is why the rule
	// moved into the selection itself.
	if sel, err := h.cat.SelectedCandidate(t.Context(), h.want); err == nil {
		t.Errorf("release_candidates still marks %q as selected, so the row claims "+
			"this want is acquiring a release its phase says it is not", sel.CandidateID)
	}
}

// A genuinely better release IS still selected.
//
// The control for the test above, and the assertion that fails if "do not go
// backwards" is implemented as "never select again" — which would break §60's
// upgrade workflow entirely while making the other test pass.
func TestASatisfiedWantStillTakesAGenuineUpgrade(t *testing.T) {
	h := newSearchHarness(t)
	// Held: 2160p h264 — accepted by the gate, and it misses the hevc
	// preference, so there is real room above it.
	h.seedHeldAsset(t, "held", "h264", 2160)
	if _, err := h.cat.ReconcileDesired(t.Context(), h.want); err != nil {
		t.Fatal(err)
	}
	if got := h.state(t).Content; got != acquisition.SatisfactionSatisfied {
		t.Fatalf("content = %s before the search, so this asserts nothing", got)
	}

	h.fake.Offer("Arrival", offer("better", 2160, "hevc"))
	if err := h.run(t); err != nil {
		t.Fatal(err)
	}

	sel, err := h.cat.SelectedCandidate(t.Context(), h.want)
	if err != nil {
		t.Fatalf("a strictly better release was not selected: %v", err)
	}
	if sel.CandidateID != "better" {
		t.Errorf("selected %q, want the higher-scoring release", sel.CandidateID)
	}
}
