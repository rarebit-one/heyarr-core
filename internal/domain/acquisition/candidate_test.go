package acquisition

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
)

// Release-candidate evaluation (§63).
//
// Every case asserts the REASONS, not only the verdict. A scorer that answers
// only with a number cannot answer "why did it not grab this release", and
// §60 keeps explainable rejection reasons among the things Heyarr retains.

// testProfile is the profile most cases run against: one gate, one exclusion,
// three preferences (including a penalty), two terminal conditions.
func testProfile() policy.Profile {
	p := policy.Profile{
		Name: "living-room",
		Accept: []policy.Rule{
			{Attribute: policy.AttrResolution, Op: policy.OpGTE, Value: policy.Num(1080)},
			{Attribute: policy.AttrSource, Op: policy.OpNotIn, Value: policy.Texts("cam", "telesync")},
		},
		Prefer: []policy.Rule{
			{Attribute: policy.AttrVideoCodec, Op: policy.OpEq, Value: policy.Text("hevc"), Weight: 20},
			{Attribute: policy.AttrHDR, Op: policy.OpEq, Value: policy.Flag(true), Weight: 10},
			{Attribute: policy.AttrSizeBytes, Op: policy.OpGTE, Value: policy.Num(64 << 30), Weight: -15},
		},
		Terminal: []policy.Rule{
			{Attribute: policy.AttrResolution, Op: policy.OpGTE, Value: policy.Num(2160)},
			{Attribute: policy.AttrSource, Op: policy.OpEq, Value: policy.Text("remux")},
		},
	}
	if err := p.Validate(); err != nil {
		panic("the test profile does not validate: " + err.Error())
	}
	return p
}

func candidate(id string, attrs Attributes) ReleaseCandidate {
	return ReleaseCandidate{ID: id, Title: id, Provider: "test", Attributes: attrs}
}

func TestEvaluate(t *testing.T) {
	profile := testProfile()

	cases := []struct {
		name         string
		attrs        Attributes
		wantAccepted bool
		wantScore    int
		wantTerminal bool
		// wantReasons maps a rule id to the result it must have carried.
		wantReasons map[string]Result
	}{
		{
			name: "every gate passes and every preference lands",
			attrs: Attributes{
				policy.AttrResolution: policy.Num(2160),
				policy.AttrSource:     policy.Text("remux"),
				policy.AttrVideoCodec: policy.Text("hevc"),
				policy.AttrHDR:        policy.Flag(true),
				policy.AttrSizeBytes:  policy.Num(40 << 30),
			},
			wantAccepted: true,
			wantScore:    30,
			wantTerminal: true,
			wantReasons: map[string]Result{
				"resolution.gte": ResultPass,
				"source.nin":     ResultPass,
				"video_codec.eq": ResultBonus,
				"hdr.eq":         ResultBonus,
				"size_bytes.gte": ResultMiss,
				"source.eq":      ResultPass,
			},
		},
		{
			name: "a gate fails, and the rejection names it",
			attrs: Attributes{
				policy.AttrResolution: policy.Num(720),
				policy.AttrSource:     policy.Text("web-dl"),
				policy.AttrVideoCodec: policy.Text("hevc"),
				policy.AttrHDR:        policy.Flag(false),
				policy.AttrSizeBytes:  policy.Num(4 << 30),
			},
			wantAccepted: false,
			// The preferences are still scored. The score is not USED — a
			// rejected candidate is never acquired — but it is computed, so
			// the reasons are complete.
			wantScore:    20,
			wantTerminal: false,
			wantReasons: map[string]Result{
				"resolution.gte": ResultFail,
				"source.nin":     ResultPass,
				"video_codec.eq": ResultBonus,
			},
		},
		{
			// The case that proves a gate is a gate: maximal preferences, one
			// failed gate, still rejected.
			name: "maximal preferences cannot buy past a failed gate",
			attrs: Attributes{
				policy.AttrResolution: policy.Num(480),
				policy.AttrSource:     policy.Text("bluray"),
				policy.AttrVideoCodec: policy.Text("hevc"),
				policy.AttrHDR:        policy.Flag(true),
				policy.AttrSizeBytes:  policy.Num(1 << 30),
			},
			wantAccepted: false,
			wantScore:    30,
			wantTerminal: false,
			wantReasons:  map[string]Result{"resolution.gte": ResultFail},
		},
		{
			name: "an excluded source is rejected by the exclusion rule",
			attrs: Attributes{
				policy.AttrResolution: policy.Num(2160),
				policy.AttrSource:     policy.Text("cam"),
				policy.AttrVideoCodec: policy.Text("h264"),
				policy.AttrHDR:        policy.Flag(false),
				policy.AttrSizeBytes:  policy.Num(2 << 30),
			},
			wantAccepted: false,
			wantScore:    0,
			wantTerminal: false,
			wantReasons: map[string]Result{
				"resolution.gte": ResultPass,
				"source.nin":     ResultFail,
			},
		},
		{
			// A penalty is a real thing to want, and it is not a rejection.
			name: "an enormous file is penalised, not rejected",
			attrs: Attributes{
				policy.AttrResolution: policy.Num(2160),
				policy.AttrSource:     policy.Text("remux"),
				policy.AttrVideoCodec: policy.Text("hevc"),
				policy.AttrHDR:        policy.Flag(true),
				policy.AttrSizeBytes:  policy.Num(80 << 30),
			},
			wantAccepted: true,
			wantScore:    15, // 20 + 10 - 15
			wantTerminal: true,
			wantReasons:  map[string]Result{"size_bytes.gte": ResultPenalty},
		},
		{
			// Accepted and not terminal: the whole gap the upgrade workflow
			// lives in.
			name: "acceptable but not as good as it gets",
			attrs: Attributes{
				policy.AttrResolution: policy.Num(1080),
				policy.AttrSource:     policy.Text("web-dl"),
				policy.AttrVideoCodec: policy.Text("h264"),
				policy.AttrHDR:        policy.Flag(false),
				policy.AttrSizeBytes:  policy.Num(8 << 30),
			},
			wantAccepted: true,
			wantScore:    0,
			wantTerminal: false,
			wantReasons: map[string]Result{
				"resolution.gte": ResultPass,
				"video_codec.eq": ResultMiss,
				"hdr.eq":         ResultMiss,
				"source.eq":      ResultMiss,
			},
		},
		{
			// Every terminal condition but one.
			name: "2160p but not a remux is not terminal",
			attrs: Attributes{
				policy.AttrResolution: policy.Num(2160),
				policy.AttrSource:     policy.Text("web-dl"),
				policy.AttrVideoCodec: policy.Text("hevc"),
				policy.AttrHDR:        policy.Flag(true),
				policy.AttrSizeBytes:  policy.Num(20 << 30),
			},
			wantAccepted: true,
			wantScore:    30,
			wantTerminal: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Evaluate(candidate("c1", tc.attrs), profile)

			if e.Accepted != tc.wantAccepted {
				t.Errorf("Accepted = %v, want %v (reasons: %s)",
					e.Accepted, tc.wantAccepted, renderReasons(e))
			}
			if e.Score != tc.wantScore {
				t.Errorf("Score = %d, want %d (reasons: %s)", e.Score, tc.wantScore, renderReasons(e))
			}
			if e.Terminal != tc.wantTerminal {
				t.Errorf("Terminal = %v, want %v", e.Terminal, tc.wantTerminal)
			}

			// Every rule in the profile produced a reason. A rule that ran
			// silently is a rule nobody can confirm ran.
			if len(e.Reasons) != len(profile.Rules()) {
				t.Errorf("%d reasons for %d rules — every rule considered must be reported",
					len(e.Reasons), len(profile.Rules()))
			}
			for rule, want := range tc.wantReasons {
				got, ok := e.Reason(rule)
				if !ok {
					t.Errorf("no reason for %s", rule)
					continue
				}
				if got.Result != want {
					t.Errorf("%s: result = %s, want %s (%s)", rule, got.Result, want, got.Detail)
				}
			}
			// Every reason carries prose. A code with no detail is half the
			// deliverable.
			for _, r := range e.Reasons {
				if strings.TrimSpace(r.Detail) == "" {
					t.Errorf("%s has a result but no explanation", r.Rule)
				}
			}
		})
	}
}

// A constraint nobody has watched reject anything is decoration. This is the
// §63 deliverable asserted directly: twelve candidates, none acceptable,
// twelve explanations naming the rule that failed.
func TestTwelveCandidatesNoneAcceptable(t *testing.T) {
	profile := testProfile()

	var candidates []ReleaseCandidate
	for i := range 12 {
		candidates = append(candidates, candidate(fmt.Sprintf("c%02d", i), Attributes{
			// Every one below the resolution gate, varying otherwise so they
			// are not twelve copies of one case.
			policy.AttrResolution: policy.Num(int64(480 + i*10)),
			policy.AttrSource:     policy.Text("web-dl"),
			policy.AttrVideoCodec: policy.Text("hevc"),
			policy.AttrHDR:        policy.Flag(i%2 == 0),
			policy.AttrSizeBytes:  policy.Num(int64(i+1) << 30),
		}))
	}

	ranked := EvaluateAll(candidates, profile)
	if len(ranked) != 12 {
		t.Fatalf("%d evaluations for 12 candidates", len(ranked))
	}
	if _, ok := Best(ranked); ok {
		t.Fatal("Best returned a candidate when none was acceptable — the first element " +
			"of a ranked list is the LEAST BAD, not necessarily an acceptable one")
	}
	for _, r := range ranked {
		if r.Evaluation.Accepted {
			t.Errorf("%s was accepted", r.Candidate.ID)
		}
		rejections := r.Evaluation.RejectedBy()
		if len(rejections) == 0 {
			t.Errorf("%s was rejected with no reason — the reasons ARE the deliverable",
				r.Candidate.ID)
			continue
		}
		var named bool
		for _, rej := range rejections {
			if rej.Rule == "resolution.gte" {
				named = true
			}
		}
		if !named {
			t.Errorf("%s was rejected for %v, but the failing gate is the resolution",
				r.Candidate.ID, rejections)
		}
	}
}

// Determinism. Two candidates with identical attributes produce identical
// scores, and a set produces a TOTAL order that does not move between runs.
func TestEvaluationIsDeterministic(t *testing.T) {
	profile := testProfile()
	attrs := Attributes{
		policy.AttrResolution: policy.Num(2160),
		policy.AttrSource:     policy.Text("remux"),
		policy.AttrVideoCodec: policy.Text("hevc"),
		policy.AttrHDR:        policy.Flag(true),
		policy.AttrSizeBytes:  policy.Num(30 << 30),
	}

	first := Evaluate(candidate("c1", attrs), profile)
	for range 200 {
		again := Evaluate(candidate("c1", attrs), profile)
		if again.Score != first.Score || again.Accepted != first.Accepted {
			t.Fatalf("the same candidate scored %d then %d", first.Score, again.Score)
		}
		if len(again.Reasons) != len(first.Reasons) {
			t.Fatalf("reason count moved: %d then %d", len(first.Reasons), len(again.Reasons))
		}
		for i := range again.Reasons {
			if again.Reasons[i] != first.Reasons[i] {
				t.Fatalf("reason %d moved: %+v then %+v", i, first.Reasons[i], again.Reasons[i])
			}
		}
	}
}

// The tie-break is what makes the order TOTAL. Without it two equally good
// releases swap places between runs and the upgrade workflow churns between
// them forever — a bug that presents as bandwidth.
func TestTiesAreBrokenByAStableKey(t *testing.T) {
	profile := testProfile()
	identical := Attributes{
		policy.AttrResolution: policy.Num(2160),
		policy.AttrSource:     policy.Text("remux"),
		policy.AttrVideoCodec: policy.Text("hevc"),
		policy.AttrHDR:        policy.Flag(true),
		policy.AttrSizeBytes:  policy.Num(30 << 30),
	}

	// Deliberately supplied in a different order each time, which is what a
	// provider that iterates a map would do.
	orders := [][]string{
		{"aaa", "bbb", "ccc", "ddd"},
		{"ddd", "ccc", "bbb", "aaa"},
		{"ccc", "aaa", "ddd", "bbb"},
		{"bbb", "ddd", "aaa", "ccc"},
	}
	var want []string
	for _, order := range orders {
		var cs []ReleaseCandidate
		for _, id := range order {
			cs = append(cs, candidate(id, identical))
		}
		ranked := EvaluateAll(cs, profile)
		got := make([]string, len(ranked))
		for i, r := range ranked {
			got[i] = r.Candidate.ID
		}
		if want == nil {
			want = got
			continue
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("input order changed the ranking: %v then %v — the tie-break is not stable",
				want, got)
		}
	}
	// And the stable key is the id, ascending, so the winner is predictable
	// rather than merely consistent.
	if want[0] != "aaa" {
		t.Errorf("ties break to %q; the documented key is the candidate id ascending", want[0])
	}
}

// Accepted candidates always outrank rejected ones, whatever they scored: a
// rejected candidate's score is a number about something that will not be used.
func TestAcceptedAlwaysOutranksRejected(t *testing.T) {
	profile := testProfile()

	// Rejected, but scoring maximally.
	rejected := candidate("a-rejected-but-brilliant", Attributes{
		policy.AttrResolution: policy.Num(480),
		policy.AttrSource:     policy.Text("bluray"),
		policy.AttrVideoCodec: policy.Text("hevc"),
		policy.AttrHDR:        policy.Flag(true),
		policy.AttrSizeBytes:  policy.Num(1 << 30),
	})
	// Accepted, but scoring nothing.
	accepted := candidate("z-accepted-but-dull", Attributes{
		policy.AttrResolution: policy.Num(1080),
		policy.AttrSource:     policy.Text("web-dl"),
		policy.AttrVideoCodec: policy.Text("h264"),
		policy.AttrHDR:        policy.Flag(false),
		policy.AttrSizeBytes:  policy.Num(8 << 30),
	})

	ranked := EvaluateAll([]ReleaseCandidate{rejected, accepted}, profile)
	if ranked[0].Candidate.ID != accepted.ID {
		t.Fatalf("ranked %q first; an accepted candidate outranks a rejected one whatever "+
			"it scored", ranked[0].Candidate.ID)
	}
	best, ok := Best(ranked)
	if !ok || best.Candidate.ID != accepted.ID {
		t.Fatalf("Best = (%v, %v)", best.Candidate.ID, ok)
	}
	// The id ordering is deliberately against us here: "a-..." sorts before
	// "z-...", so a ranking that ignored acceptance would put the rejected one
	// first.
	if rejected.ID >= accepted.ID {
		t.Fatal("this test only means something if the rejected candidate sorts first by id")
	}
}

// An attribute the provider could not determine is NOT the same as a failing
// one, and the difference decides where an operator looks.
func TestUndeterminedAttributes(t *testing.T) {
	profile := testProfile()

	t.Run("an undetermined gate rejects, and says the provider could not tell", func(t *testing.T) {
		e := Evaluate(candidate("c1", Attributes{
			// No resolution at all.
			policy.AttrSource:     policy.Text("remux"),
			policy.AttrVideoCodec: policy.Text("hevc"),
			policy.AttrHDR:        policy.Flag(true),
			policy.AttrSizeBytes:  policy.Num(20 << 30),
		}), profile)

		if e.Accepted {
			t.Error("a gate that cannot be shown to hold must not pass — otherwise a " +
				"profile saying \"1080p minimum\" accepts a release nobody could measure")
		}
		got, ok := e.Reason("resolution.gte")
		if !ok {
			t.Fatal("no reason for the undetermined gate")
		}
		if got.Result != ResultUndetermined {
			t.Errorf("result = %s, want undetermined — \"it is 720p\" and \"nobody could "+
				"tell\" are different situations", got.Result)
		}
		if !strings.Contains(got.Detail, "could not determine") {
			t.Errorf("the reason should say the provider could not tell: %q", got.Detail)
		}
	})

	t.Run("an undetermined preference contributes nothing and does not reject", func(t *testing.T) {
		e := Evaluate(candidate("c1", Attributes{
			policy.AttrResolution: policy.Num(1080),
			policy.AttrSource:     policy.Text("web-dl"),
			policy.AttrSizeBytes:  policy.Num(8 << 30),
			// No video_codec, no hdr.
		}), profile)

		if !e.Accepted {
			t.Error("§62's prefer is a score, never a gate — an undetermined preference " +
				"must not reject")
		}
		if e.Score != 0 {
			t.Errorf("Score = %d, want 0", e.Score)
		}
		for _, rule := range []string{"video_codec.eq", "hdr.eq"} {
			got, _ := e.Reason(rule)
			if got.Result != ResultUndetermined {
				t.Errorf("%s: result = %s, want undetermined", rule, got.Result)
			}
		}
	})

	t.Run("an undetermined terminal condition keeps the want upgradable", func(t *testing.T) {
		e := Evaluate(candidate("c1", Attributes{
			policy.AttrResolution: policy.Num(2160),
			policy.AttrVideoCodec: policy.Text("hevc"),
			policy.AttrHDR:        policy.Flag(true),
			policy.AttrSizeBytes:  policy.Num(20 << 30),
			// No source: one accept rule and one terminal rule both need it.
		}), profile)

		if e.Terminal {
			t.Error("a terminal condition that cannot be shown to hold does not hold — " +
				"keeping looking is the safe direction")
		}
	})
}

// A profile with no terminal rules is NEVER terminal. Vacuous truth over an
// empty set would turn "never stop looking" into "stop immediately".
func TestAProfileWithNoTerminalRulesIsNeverTerminal(t *testing.T) {
	openEnded := policy.Profile{
		Name:   "archival",
		Accept: []policy.Rule{{Attribute: policy.AttrSource, Op: policy.OpNotIn, Value: policy.Texts("cam")}},
	}
	if err := openEnded.Validate(); err != nil {
		t.Fatal(err)
	}
	e := Evaluate(candidate("perfect", Attributes{
		policy.AttrResolution: policy.Num(4320),
		policy.AttrSource:     policy.Text("remux"),
		policy.AttrVideoCodec: policy.Text("av1"),
		policy.AttrHDR:        policy.Flag(true),
		policy.AttrSizeBytes:  policy.Num(200 << 30),
	}), openEnded)

	if !e.Accepted {
		t.Fatal("this candidate passes the only gate")
	}
	if e.Terminal {
		t.Error("a profile with no terminal condition is never finished — that is what " +
			"\"never stop looking\" means")
	}
}

// A rejected candidate is never terminal, whatever it scores: terminality says
// "this is as good as it gets and we are done", which is meaningless about
// something that will not be acquired.
func TestARejectedCandidateIsNeverTerminal(t *testing.T) {
	profile := testProfile()
	e := Evaluate(candidate("c1", Attributes{
		// Meets both terminal conditions and fails the source gate.
		policy.AttrResolution: policy.Num(2160),
		policy.AttrSource:     policy.Text("remux"),
		policy.AttrVideoCodec: policy.Text("hevc"),
		policy.AttrHDR:        policy.Flag(true),
		policy.AttrSizeBytes:  policy.Num(20 << 30),
	}), profile)
	if !e.Accepted || !e.Terminal {
		t.Fatal("setup: this candidate should be accepted and terminal")
	}

	rejected := Evaluate(candidate("c2", Attributes{
		policy.AttrResolution: policy.Num(2160),
		policy.AttrSource:     policy.Text("cam"), // excluded
		policy.AttrVideoCodec: policy.Text("hevc"),
		policy.AttrHDR:        policy.Flag(true),
		policy.AttrSizeBytes:  policy.Num(20 << 30),
	}), profile)
	if rejected.Accepted {
		t.Fatal("setup: this candidate should be rejected")
	}
	if rejected.Terminal {
		t.Error("a rejected candidate must never be terminal")
	}
}

// A candidate is evaluated against every rule even after a gate fails: an
// operator asking why a release was rejected wants the whole picture, not the
// first thing that went wrong.
func TestAFailedGateDoesNotStopTheReporting(t *testing.T) {
	profile := testProfile()
	e := Evaluate(candidate("c1", Attributes{
		policy.AttrResolution: policy.Num(480), // fails the first gate
		policy.AttrSource:     policy.Text("cam"),
		policy.AttrVideoCodec: policy.Text("h264"),
		policy.AttrHDR:        policy.Flag(false),
		policy.AttrSizeBytes:  policy.Num(1 << 30),
	}), profile)

	if len(e.Reasons) != len(profile.Rules()) {
		t.Fatalf("%d reasons for %d rules — evaluation stopped at the first failure",
			len(e.Reasons), len(profile.Rules()))
	}
	if len(e.RejectedBy()) != 2 {
		t.Errorf("both gates failed; RejectedBy reported %d", len(e.RejectedBy()))
	}
}

// A profile with no rules accepts everything and scores nothing. It is legal,
// and the behaviour should be boring rather than surprising.
func TestAnEmptyProfileAcceptsEverything(t *testing.T) {
	empty := policy.Profile{Name: "anything"}
	if err := empty.Validate(); err != nil {
		t.Fatal(err)
	}
	e := Evaluate(candidate("c1", Attributes{policy.AttrResolution: policy.Num(240)}), empty)
	if !e.Accepted || e.Score != 0 || e.Terminal || len(e.Reasons) != 0 {
		t.Errorf("an empty profile should accept with no score and no reasons, got %+v", e)
	}
}

// A candidate with no attributes at all — a provider that determined nothing.
func TestACandidateWithNoAttributes(t *testing.T) {
	e := Evaluate(candidate("c1", Attributes{}), testProfile())
	if e.Accepted {
		t.Error("gates that cannot be shown to hold must not pass")
	}
	for _, r := range e.Reasons {
		if r.Result != ResultUndetermined {
			t.Errorf("%s: result = %s, want undetermined for every rule", r.Rule, r.Result)
		}
	}
}

// A provider that supplied the wrong KIND for an attribute is a provider bug,
// and the safe reading is that the rule does not hold.
func TestAWrongKindedAttributeDoesNotHold(t *testing.T) {
	e := Evaluate(candidate("c1", Attributes{
		policy.AttrResolution: policy.Text("2160p"), // text where a number belongs
		policy.AttrSource:     policy.Text("remux"),
	}), testProfile())
	if e.Accepted {
		t.Error("a mistyped attribute must not satisfy a gate")
	}
	got, _ := e.Reason("resolution.gte")
	if got.Result != ResultFail {
		t.Errorf("result = %s, want fail", got.Result)
	}
}

func TestEvaluateAllOnAnEmptySet(t *testing.T) {
	ranked := EvaluateAll(nil, testProfile())
	if len(ranked) != 0 {
		t.Fatalf("%d results for no candidates", len(ranked))
	}
	if _, ok := Best(ranked); ok {
		t.Error("Best found something in an empty set")
	}
}

func renderReasons(e Evaluation) string {
	parts := make([]string, 0, len(e.Reasons))
	for _, r := range e.Reasons {
		parts = append(parts, fmt.Sprintf("%s=%s", r.Rule, r.Result))
	}
	return strings.Join(parts, " ")
}
