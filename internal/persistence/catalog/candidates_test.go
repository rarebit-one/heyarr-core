package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// Release candidates and their evaluations, against a real database
// (§63, M3-12).
//
// The property these exist for is the one §63 turns on: what was decided is
// recoverable AFTERWARDS, exactly as it was decided. Everything else here is
// in service of that.

// candidateProfile gates on 1080p and prefers hevc, so acceptance, rejection
// and scoring are all reachable with three candidates.
func candidateProfile() policy.Profile {
	p := policy.Profile{
		Name: "living-room",
		Accept: []policy.Rule{
			{Attribute: policy.AttrResolution, Op: policy.OpGTE, Value: policy.Num(1080)},
		},
		Prefer: []policy.Rule{
			{Attribute: policy.AttrVideoCodec, Op: policy.OpEq, Value: policy.Text("hevc"), Weight: 20},
		},
	}
	if err := p.Validate(); err != nil {
		panic("the test profile does not validate: " + err.Error())
	}
	return p
}

func release(id string, resolution int64, codec string) acquisition.ReleaseCandidate {
	return acquisition.ReleaseCandidate{
		ID: id, Title: "Release " + id, Provider: "fake-indexer",
		Attributes: acquisition.Attributes{
			policy.AttrResolution: policy.Num(resolution),
			policy.AttrVideoCodec: policy.Text(codec),
		},
	}
}

func rankThree() []acquisition.Ranked {
	return acquisition.EvaluateAll([]acquisition.ReleaseCandidate{
		release("good", 2160, "hevc"),
		release("plain", 1080, "h264"),
		release("tiny", 480, "hevc"),
	}, candidateProfile())
}

// THE property. The stored evaluation is byte-for-byte what the evaluator
// returned — not a summary, not a re-derivation.
//
// A stored explanation that might not be the real one is worse than none,
// because it will be believed.
func TestTheStoredEvaluationIsByteIdenticalToTheEvaluators(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}

	ranked := rankThree()
	if _, err := h.cat.RecordSearch(ctx, h.want, ranked); err != nil {
		t.Fatal(err)
	}

	// What the evaluator produced, encoded.
	wantJSON := map[string][]byte{}
	for _, r := range ranked {
		raw, err := json.Marshal(r.Evaluation)
		if err != nil {
			t.Fatal(err)
		}
		wantJSON[r.Candidate.ID] = raw
	}

	// What the database holds, read as raw text rather than through the
	// decoder — decoding and re-encoding would launder exactly the difference
	// this test exists to catch.
	rows, err := h.db.Reader().QueryContext(ctx,
		`SELECT candidate_id, evaluation FROM release_candidates WHERE desired_item_id = ?`,
		h.want)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	var seen int
	for rows.Next() {
		var id, stored string
		if err := rows.Scan(&id, &stored); err != nil {
			t.Fatal(err)
		}
		seen++
		want, ok := wantJSON[id]
		if !ok {
			t.Errorf("stored a candidate %q the evaluator never produced", id)
			continue
		}
		if stored != string(want) {
			t.Errorf("%s: the stored evaluation is not what the evaluator returned\n"+
				" stored: %s\n   want: %s", id, stored, want)
		}
	}
	if seen != len(ranked) {
		t.Fatalf("stored %d candidates for %d evaluated", seen, len(ranked))
	}
}

// Twelve candidates, none acceptable: twelve durable, explained refusals, and
// nothing selected. This is §63's deliverable, and §60's reason for keeping it.
func TestTwelveRejectionsArePersisted(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}

	var offered []acquisition.ReleaseCandidate
	for i := range 12 {
		// All below the 1080p gate, varying otherwise so they are not twelve
		// copies of one case.
		offered = append(offered, release(fmt.Sprintf("c%02d", i), int64(480+i*10), "hevc"))
	}
	ranked := acquisition.EvaluateAll(offered, candidateProfile())

	outcome, err := h.cat.RecordSearch(ctx, h.want, ranked)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Found != 12 {
		t.Fatalf("found %d, want 12", outcome.Found)
	}
	if outcome.Acceptable != 0 {
		t.Fatalf("%d acceptable; every candidate is below the gate", outcome.Acceptable)
	}
	if outcome.SelectedCandidateID != "" {
		t.Fatalf("selected %q when nothing was acceptable — the first of a ranked list "+
			"is the LEAST BAD, not an acceptable one", outcome.SelectedCandidateID)
	}

	stored, err := h.cat.CandidatesFor(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 12 {
		t.Fatalf("stored %d rejections for 12 candidates", len(stored))
	}
	for _, c := range stored {
		if c.Evaluation.Accepted {
			t.Errorf("%s was accepted", c.CandidateID)
		}
		if c.Selected {
			t.Errorf("%s was selected despite being rejected", c.CandidateID)
		}
		rejections := c.Evaluation.RejectedBy()
		if len(rejections) == 0 {
			t.Errorf("%s was rejected with no reason — the reasons ARE the deliverable",
				c.CandidateID)
			continue
		}
		if rejections[0].Rule != "resolution.gte" {
			t.Errorf("%s rejected by %s, but the failing gate is the resolution",
				c.CandidateID, rejections[0].Rule)
		}
		if strings.TrimSpace(rejections[0].Detail) == "" {
			t.Errorf("%s rejected with a code and no prose", c.CandidateID)
		}
	}
}

// The best ACCEPTABLE candidate is selected, and exactly one is.
func TestTheBestAcceptableCandidateIsSelected(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	outcome, err := h.cat.RecordSearch(ctx, h.want, rankThree())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.SelectedCandidateID != "good" {
		t.Fatalf("selected %q, want good", outcome.SelectedCandidateID)
	}
	if outcome.Acceptable != 2 {
		t.Errorf("%d acceptable, want 2", outcome.Acceptable)
	}

	stored, err := h.cat.CandidatesFor(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	var selected int
	for _, c := range stored {
		if c.Selected {
			selected++
		}
	}
	if selected != 1 {
		t.Errorf("%d candidates are selected; exactly one may be", selected)
	}
	// Ordered best first, the same total order the scorer ranks by.
	if stored[0].CandidateID != "good" || stored[len(stored)-1].CandidateID != "tiny" {
		var ids []string
		for _, c := range stored {
			ids = append(ids, c.CandidateID)
		}
		t.Errorf("order = %v; accepted before rejected, then score descending", ids)
	}
}

// A search REPLACES its predecessor. Keeping both would make "what are the
// candidates for this want" a question with an ORDER BY and a LIMIT.
func TestASearchReplacesThePreviousSet(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}

	first, err := h.cat.RecordSearch(ctx, h.want, rankThree())
	if err != nil {
		t.Fatal(err)
	}

	second, err := h.cat.RecordSearch(ctx, h.want, acquisition.EvaluateAll(
		[]acquisition.ReleaseCandidate{release("later", 2160, "hevc")}, candidateProfile()))
	if err != nil {
		t.Fatal(err)
	}
	if second.SearchID == first.SearchID {
		t.Fatal("two searches share a search id")
	}

	stored, err := h.cat.CandidatesFor(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].CandidateID != "later" {
		var ids []string
		for _, c := range stored {
			ids = append(ids, c.CandidateID)
		}
		t.Fatalf("after a second search the set is %v; it must be replaced, not appended", ids)
	}
}

// Re-running the same search produces the same rows, not duplicates. The job
// WILL be re-run (invariant 9).
func TestRecordingTheSameSearchTwiceDoesNotDuplicate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := h.cat.RecordSearch(ctx, h.want, rankThree()); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := h.cat.CandidatesFor(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 3 {
		t.Errorf("three identical searches left %d rows for 3 candidates", len(stored))
	}
}

// An empty search still emits. It leaves no rows to explain itself, so without
// the event a want that found nothing is indistinguishable from a want nobody
// searched — which is exactly the silence §60 keeps rejection reasons to avoid.
func TestAnEmptySearchStillEmits(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	before := h.eventCount(t)

	outcome, err := h.cat.RecordSearch(ctx, h.want, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Found != 0 || outcome.SelectedCandidateID != "" {
		t.Fatalf("outcome = %+v", outcome)
	}
	if got := h.eventCount(t); got != before+1 {
		t.Errorf("an empty search emitted %d event(s); it must emit exactly one, "+
			"because it leaves no rows behind to say it happened", got-before)
	}
}

// Manual override (§60). It records that a person disagreed, and what the
// scorer had said — an override that left no trace would look exactly like an
// ordinary selection.
func TestOverrideRecordsTheDisagreement(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cat.RecordSearch(ctx, h.want, rankThree()); err != nil {
		t.Fatal(err)
	}
	before := h.eventCount(t)

	// "plain" is acceptable and scores lower than "good".
	chosen, err := h.cat.OverrideSelection(ctx, h.want, "plain")
	if err != nil {
		t.Fatal(err)
	}
	if !chosen.Selected || !chosen.Overridden {
		t.Fatalf("chosen = %+v; it must be both selected and marked an override", chosen)
	}
	for _, want := range []string{"good", "chosen by hand"} {
		if !strings.Contains(chosen.OverrideDetail, want) {
			t.Errorf("the detail should record what the scorer chose (%q); got %q",
				want, chosen.OverrideDetail)
		}
	}
	if got := h.eventCount(t); got != before+1 {
		t.Errorf("an override emitted %d event(s), want 1", got-before)
	}

	// And exactly one row is selected afterwards — the previous selection was
	// cleared rather than joined.
	stored, err := h.cat.CandidatesFor(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	var selected []string
	for _, c := range stored {
		if c.Selected {
			selected = append(selected, c.CandidateID)
		}
	}
	if len(selected) != 1 || selected[0] != "plain" {
		t.Errorf("selected = %v, want [plain]", selected)
	}
}

// Overriding to the candidate the scorer already chose is a selection, not a
// disagreement. Recording it as one would put a departure in the audit trail
// that never happened.
func TestReSelectingTheScorersChoiceIsNotAnOverride(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cat.RecordSearch(ctx, h.want, rankThree()); err != nil {
		t.Fatal(err)
	}
	before := h.eventCount(t)

	chosen, err := h.cat.OverrideSelection(ctx, h.want, "good")
	if err != nil {
		t.Fatal(err)
	}
	if chosen.Overridden {
		t.Error("re-selecting what the scorer chose is not a disagreement")
	}
	if got := h.eventCount(t); got != before {
		t.Errorf("it emitted %d event(s); there was no departure to record", got-before)
	}
}

// The refusal. §62's gates are the operator's own statement of what is
// acceptable, and an override that could ignore them would turn `accept` into
// a suggestion.
func TestOverrideRefusesARejectedCandidate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cat.RecordSearch(ctx, h.want, rankThree()); err != nil {
		t.Fatal(err)
	}

	// "tiny" is 480p, below the gate.
	if _, err := h.cat.OverrideSelection(ctx, h.want, "tiny"); !errors.Is(err, catalog.ErrNotAcceptable) {
		t.Fatalf("expected ErrNotAcceptable, got %v", err)
	}
	// And the scorer's choice is untouched.
	sel, err := h.cat.SelectedCandidate(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if sel.CandidateID != "good" {
		t.Errorf("a refused override changed the selection to %s", sel.CandidateID)
	}
}

func TestOverridingAnUnknownCandidateIsTyped(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cat.RecordSearch(ctx, h.want, rankThree()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cat.OverrideSelection(ctx, h.want, "never-offered"); !errors.Is(err, catalog.ErrNoCandidate) {
		t.Errorf("expected ErrNoCandidate, got %v", err)
	}
}

// The prune is global and spares what is in flight. A selected candidate
// explains what a want is currently acquiring; removing it would leave an
// acquisition with nothing to say why.
func TestPruneSparesTheSelectedCandidate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cat.RecordSearch(ctx, h.want, rankThree()); err != nil {
		t.Fatal(err)
	}

	// Everything is stamped with the fixed clock, so a cutoff after it prunes
	// everything prunable.
	cutoff := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	n, err := h.cat.PruneCandidates(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("pruned %d rows; the two unselected ones should go", n)
	}

	stored, err := h.cat.CandidatesFor(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || !stored[0].Selected {
		t.Fatalf("after pruning, %d rows remain; only the selected one should", len(stored))
	}
}

// A prune with a cutoff before anything was searched removes nothing.
func TestPruneLeavesRecentCandidates(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cat.RecordSearch(ctx, h.want, rankThree()); err != nil {
		t.Fatal(err)
	}
	n, err := h.cat.PruneCandidates(ctx, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("pruned %d recent rows", n)
	}
}

// Candidates do not outlive the want they explain.
func TestCandidatesCascadeFromTheWant(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cat.RecordSearch(ctx, h.want, rankThree()); err != nil {
		t.Fatal(err)
	}
	h.exec(t, `DELETE FROM desired_items WHERE id = ?`, h.want)

	var n int
	if err := h.db.Reader().QueryRow(`SELECT count(*) FROM release_candidates`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d candidate row(s) outlived their want", n)
	}
}

// SearchContextFor gathers what a query needs in one read.
func TestSearchContextCarriesTheQueryAndTheProfile(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}
	sc, err := h.cat.SearchContextFor(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Title != "Arrival" || sc.Year != 2016 || sc.ContentType != "movie" {
		t.Errorf("query = %q %d %q", sc.Title, sc.Year, sc.ContentType)
	}
	if sc.Profile.Name == "" {
		t.Error("no profile — a search with no standard cannot be evaluated")
	}
	if sc.State.Phase != acquisition.PhaseIdle {
		t.Errorf("phase = %s, want idle", sc.State.Phase)
	}
}

// The listing breaks ties on the CANDIDATE ID, which is the evaluator's own
// tie-break — not on anything this layer invented.
//
// This exists because a sabotage that replaced the tie-break with `title ASC`
// passed every other test in this file: the fixtures above happen to have
// titles that sort the same way as their ids, so the two orders were
// indistinguishable. A test that cannot tell two orderings apart is not
// testing the ordering.
//
// So these candidates have titles that sort OPPOSITE to their ids, at equal
// scores. Any second opinion about ranking shows up immediately.
func TestTheListingBreaksTiesOnTheEvaluatorsKeyAndNoOther(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, h.want); err != nil {
		t.Fatal(err)
	}

	// Same attributes, so identical scores; ids ascend while titles descend.
	tied := []acquisition.ReleaseCandidate{
		{ID: "aaa", Title: "zzz", Provider: "fake-indexer", Attributes: acquisition.Attributes{
			policy.AttrResolution: policy.Num(2160), policy.AttrVideoCodec: policy.Text("hevc"),
		}},
		{ID: "bbb", Title: "yyy", Provider: "fake-indexer", Attributes: acquisition.Attributes{
			policy.AttrResolution: policy.Num(2160), policy.AttrVideoCodec: policy.Text("hevc"),
		}},
		{ID: "ccc", Title: "xxx", Provider: "fake-indexer", Attributes: acquisition.Attributes{
			policy.AttrResolution: policy.Num(2160), policy.AttrVideoCodec: policy.Text("hevc"),
		}},
	}
	ranked := acquisition.EvaluateAll(tied, candidateProfile())
	if _, err := h.cat.RecordSearch(ctx, h.want, ranked); err != nil {
		t.Fatal(err)
	}

	stored, err := h.cat.CandidatesFor(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range stored {
		got = append(got, c.CandidateID)
	}
	if strings.Join(got, ",") != "aaa,bbb,ccc" {
		t.Errorf("order = %v; ties break on the candidate id, which is the evaluator's "+
			"own key — anything else is a second opinion about what is better", got)
	}

	// And the listing agrees with what the evaluator ranked first, which is
	// what stops "what am I acquiring" and "what is best" disagreeing.
	best, ok := acquisition.Best(ranked)
	if !ok {
		t.Fatal("all three are acceptable")
	}
	if stored[0].CandidateID != best.Candidate.ID {
		t.Errorf("the listing puts %s first and the evaluator chose %s",
			stored[0].CandidateID, best.Candidate.ID)
	}
	if !stored[0].Selected {
		t.Error("the first row is the selected one")
	}
}
