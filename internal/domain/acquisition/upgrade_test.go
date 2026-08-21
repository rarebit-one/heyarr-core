package acquisition

import (
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
)

// The upgrade workflow (§60).
//
// Every case asserts the STATUS and not merely a boolean, because "no upgrade"
// has four different meanings and an operator asking "why is this not
// upgrading" needs to know which one they are looking at.

// upgradeProfile is testProfile() reused: accept resolution >= 1080 and a
// source exclusion; prefer hevc (20), hdr (10) and a size penalty (-15);
// terminal at 2160p remux.
//
// Reused deliberately rather than defined afresh — the whole claim of this
// file is that upgrades are decided by the SAME scorer under the SAME profile
// that decided acceptance, and a second profile here would quietly weaken it.
func upgradeProfile() policy.Profile { return testProfile() }

// score evaluates one attribute set, so a test can talk about an incumbent in
// terms of what it IS rather than what it scores.
func score(attrs Attributes) Evaluation {
	return Evaluate(ReleaseCandidate{ID: "incumbent", Attributes: attrs}, upgradeProfile())
}

// held1080 is an acceptable, non-terminal incumbent: it passes the gate and
// meets no preference, so it scores zero and has plenty of room to improve.
func held1080() Attributes {
	return Attributes{
		policy.AttrResolution: policy.Num(1080),
		policy.AttrSource:     policy.Text("web-dl"),
		policy.AttrVideoCodec: policy.Text("h264"),
		policy.AttrHDR:        policy.Flag(false),
		policy.AttrSizeBytes:  policy.Num(8 << 30),
	}
}

// heldTerminal meets every terminal condition — 2160p remux — so there is
// nothing left to want.
func heldTerminal() Attributes {
	return Attributes{
		policy.AttrResolution: policy.Num(2160),
		policy.AttrSource:     policy.Text("remux"),
		policy.AttrVideoCodec: policy.Text("hevc"),
		policy.AttrHDR:        policy.Flag(true),
		policy.AttrSizeBytes:  policy.Num(30 << 30),
	}
}

// better1080 is the same resolution and source as held1080 but with hevc, so
// it scores 20 rather than 0: strictly better without being terminal.
func better1080() Attributes {
	a := held1080()
	a[policy.AttrVideoCodec] = policy.Text("hevc")
	return a
}

func TestConsiderUpgrade(t *testing.T) {
	profile := upgradeProfile()

	cases := []struct {
		name        string
		req         UpgradeRequest
		wantStatus  UpgradeStatus
		wantDetail  string
		wantImprove int
	}{
		{
			name: "a strictly better candidate is an upgrade",
			req: UpgradeRequest{
				Monitor: true, Satisfied: true, Profile: profile,
				Incumbent:  Incumbent{AssetID: "a1", Evaluation: score(held1080())},
				Candidates: []ReleaseCandidate{{ID: "c1", Attributes: better1080()}},
			},
			wantStatus:  UpgradeAvailable,
			wantDetail:  "scores 20 against the 0 that is held",
			wantImprove: 20,
		},
		{
			// The case #98 says matters most and is most likely to be omitted:
			// unmonitored, satisfied, and a strictly better candidate right
			// there on the table.
			name: "an unmonitored want is finished even with a better candidate available",
			req: UpgradeRequest{
				Monitor: false, Satisfied: true, Profile: profile,
				Incumbent:  Incumbent{AssetID: "a1", Evaluation: score(held1080())},
				Candidates: []ReleaseCandidate{{ID: "c1", Attributes: heldTerminal()}},
			},
			wantStatus: UpgradeNotMonitored,
			wantDetail: "not monitored",
		},
		{
			name: "a terminal incumbent has nothing left to want",
			req: UpgradeRequest{
				Monitor: true, Satisfied: true, Profile: profile,
				Incumbent: Incumbent{AssetID: "a1", Evaluation: score(heldTerminal())},
				// A candidate that would otherwise be an upgrade: bigger, so
				// it takes the size penalty and scores LOWER, but the point is
				// that terminality short-circuits before any of that runs.
				Candidates: []ReleaseCandidate{{ID: "c1", Attributes: better1080()}},
			},
			wantStatus: UpgradeTerminal,
			wantDetail: "every terminal condition",
		},
		{
			name: "a want with nothing acceptable held is an acquisition, not an upgrade",
			req: UpgradeRequest{
				Monitor: true, Satisfied: false, Profile: profile,
				Candidates: []ReleaseCandidate{{ID: "c1", Attributes: heldTerminal()}},
			},
			wantStatus: UpgradeNotSatisfied,
			wantDetail: "acquisition rather than an upgrade",
		},
		{
			name: "nothing on offer is the normal answer for a healthy library",
			req: UpgradeRequest{
				Monitor: true, Satisfied: true, Profile: profile,
				Incumbent:  Incumbent{AssetID: "a1", Evaluation: score(held1080())},
				Candidates: nil,
			},
			wantStatus: UpgradeNoBetterCandidate,
			wantDetail: "no candidate is acceptable",
		},
		{
			name: "a candidate that fails the gate is not an upgrade whatever it scores",
			req: UpgradeRequest{
				Monitor: true, Satisfied: true, Profile: profile,
				Incumbent: Incumbent{AssetID: "a1", Evaluation: score(held1080())},
				Candidates: []ReleaseCandidate{{ID: "c1", Attributes: Attributes{
					// 480p fails the gate but scores 30 on preferences.
					policy.AttrResolution: policy.Num(480),
					policy.AttrSource:     policy.Text("bluray"),
					policy.AttrVideoCodec: policy.Text("hevc"),
					policy.AttrHDR:        policy.Flag(true),
					policy.AttrSizeBytes:  policy.Num(2 << 30),
				}}},
			},
			wantStatus: UpgradeNoBetterCandidate,
			wantDetail: "no candidate is acceptable",
		},
		{
			name: "a worse candidate is not an upgrade",
			req: UpgradeRequest{
				Monitor: true, Satisfied: true, Profile: profile,
				Incumbent: Incumbent{AssetID: "a1", Evaluation: score(better1080())},
				Candidates: []ReleaseCandidate{
					{ID: "c1", Attributes: held1080()},
				},
			},
			wantStatus: UpgradeNoBetterCandidate,
			wantDetail: "is worse than",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ConsiderUpgrade(tc.req)
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %s, want %s (%s)", got.Status, tc.wantStatus, got.Detail)
			}
			if !strings.Contains(got.Detail, tc.wantDetail) {
				t.Errorf("detail = %q, want it to mention %q", got.Detail, tc.wantDetail)
			}
			if got.Improvement != tc.wantImprove {
				t.Errorf("improvement = %d, want %d", got.Improvement, tc.wantImprove)
			}
			if strings.TrimSpace(got.Detail) == "" {
				t.Error("every verdict carries an explanation")
			}
			// Only an available upgrade names a candidate. A verdict that
			// carried one otherwise would invite a caller to act on it.
			if got.Status != UpgradeAvailable && got.Candidate.ID != "" {
				t.Errorf("a %s verdict names candidate %q", got.Status, got.Candidate.ID)
			}
		})
	}
}

// THE churn test. A tie is not an upgrade — strictly better, or nothing.
//
// Without this, two equivalent releases replace each other forever: each scan
// finds the other, swaps, and the next scan swaps back. It is a bug that
// presents as bandwidth rather than as an error, so nothing goes red and
// somebody notices in a month when their transfer cap does.
func TestATieIsNotAnUpgradeAndDoesNotChurn(t *testing.T) {
	profile := upgradeProfile()
	incumbent := score(held1080())

	// A DIFFERENT release with IDENTICAL attributes — a real tie, not a
	// contrived worse candidate. This is what two encodes of the same source
	// from two indexers look like.
	tie := ReleaseCandidate{ID: "a-different-release", Attributes: held1080()}

	got := ConsiderUpgrade(UpgradeRequest{
		Monitor: true, Satisfied: true, Profile: profile,
		Incumbent:  Incumbent{AssetID: "the-incumbent", Evaluation: incumbent},
		Candidates: []ReleaseCandidate{tie},
	})
	if got.Status == UpgradeAvailable {
		t.Fatalf("an equal-scoring candidate was reported as an upgrade (%s) — the library "+
			"will now swap between two equivalent releases forever", got.Detail)
	}
	if got.Status != UpgradeNoBetterCandidate {
		t.Fatalf("status = %s, want no_better_candidate", got.Status)
	}
	if !strings.Contains(got.Detail, "matches") {
		t.Errorf("detail = %q; a tie should say so rather than reporting the "+
			"candidate as worse", got.Detail)
	}

	// And the loop actually terminates. Simulate the swap: if a tie were an
	// upgrade, the replacement becomes the incumbent, the old one becomes the
	// candidate, and we are back where we started. Run it and assert nothing
	// ever moves.
	held := incumbent
	heldID := "the-incumbent"
	offered := tie
	for round := range 10 {
		v := ConsiderUpgrade(UpgradeRequest{
			Monitor: true, Satisfied: true, Profile: profile,
			Incumbent:  Incumbent{AssetID: heldID, Evaluation: held},
			Candidates: []ReleaseCandidate{offered},
		})
		if v.Status == UpgradeAvailable {
			t.Fatalf("round %d: the scan swapped to %q; a tie must leave the incumbent alone",
				round, v.Candidate.ID)
		}
		// The swap that would happen if it were an upgrade.
		held, heldID, offered = Evaluate(offered, profile), offered.ID,
			ReleaseCandidate{ID: heldID, Attributes: held1080()}
	}
}

// The improvement is reported, because "score 0 to 20" tells an operator
// whether a replacement is worth the bandwidth in a way "an upgrade is
// available" does not.
func TestTheUpgradeCarriesItsSizeAndItsReasons(t *testing.T) {
	profile := upgradeProfile()
	got := ConsiderUpgrade(UpgradeRequest{
		Monitor: true, Satisfied: true, Profile: profile,
		Incumbent:  Incumbent{AssetID: "a1", Evaluation: score(held1080())},
		Candidates: []ReleaseCandidate{{ID: "c1", Attributes: heldTerminal()}},
	})
	if got.Status != UpgradeAvailable {
		t.Fatalf("status = %s: %s", got.Status, got.Detail)
	}
	// heldTerminal scores hevc 20 + hdr 10 = 30 and takes no size penalty
	// (30 GiB is under the profile's 64 GiB threshold); held1080 scores 0.
	if got.Improvement != 30 {
		t.Errorf("improvement = %d, want 30", got.Improvement)
	}
	if got.Improvement <= 0 {
		t.Fatalf("an upgrade must improve something, got %d", got.Improvement)
	}
	if got.Evaluation.Score-score(held1080()).Score != got.Improvement {
		t.Errorf("improvement %d does not match the score difference", got.Improvement)
	}

	// The explanation is §63's reasons, not a separate prose string invented
	// by this package.
	if len(got.Evaluation.Reasons) == 0 {
		t.Fatal("the upgrade carries no reasons")
	}
	var sawBonus bool
	for _, r := range got.Evaluation.Reasons {
		if r.Result == ResultBonus {
			sawBonus = true
		}
	}
	if !sawBonus {
		t.Error("the upgrade's reasons should show what it scores on")
	}
}

// Monitoring outranks terminality. A want that is both unmonitored and
// terminal reports the operator's decision, because that is the more useful
// answer: "you told me to stop" beats "it happens to be perfect".
func TestMonitoringIsCheckedBeforeTerminality(t *testing.T) {
	got := ConsiderUpgrade(UpgradeRequest{
		Monitor: false, Satisfied: true, Profile: upgradeProfile(),
		Incumbent: Incumbent{AssetID: "a1", Evaluation: score(heldTerminal())},
	})
	if got.Status != UpgradeNotMonitored {
		t.Errorf("status = %s, want not_monitored", got.Status)
	}
}

// Terminality is READ from the incumbent's evaluation, never recomputed. One
// implementation of "is this as good as it gets", or the two drift.
func TestTerminalityIsNotRecomputed(t *testing.T) {
	profile := upgradeProfile()

	// An evaluation that CLAIMS terminal against attributes that are not.
	// Nothing in the real system produces this; the point is that
	// ConsiderUpgrade believes the evaluation rather than re-deriving.
	claimed := score(held1080())
	if claimed.Terminal {
		t.Fatal("setup: held1080 is not terminal")
	}
	claimed.Terminal = true

	got := ConsiderUpgrade(UpgradeRequest{
		Monitor: true, Satisfied: true, Profile: profile,
		Incumbent:  Incumbent{AssetID: "a1", Evaluation: claimed},
		Candidates: []ReleaseCandidate{{ID: "c1", Attributes: heldTerminal()}},
	})
	if got.Status != UpgradeTerminal {
		t.Errorf("status = %s; the incumbent's evaluation is the authority on "+
			"terminality, and re-deriving it here would be a second opinion", got.Status)
	}
}

// Eligibility is the listing question §71's get_upgrade_candidates asks, and
// it is answerable from state alone — without a search per row.
func TestEligible(t *testing.T) {
	satisfied := score(held1080())
	terminal := score(heldTerminal())

	cases := []struct {
		name           string
		monitor, isSat bool
		incumbent      Evaluation
		want           bool
	}{
		{"monitored, satisfied, not terminal", true, true, satisfied, true},
		{"unmonitored", false, true, satisfied, false},
		{"terminal", true, true, terminal, false},
		{"not satisfied", true, false, Evaluation{}, false},
		{"unmonitored AND terminal", false, true, terminal, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Eligible(tc.monitor, tc.isSat, tc.incumbent); got != tc.want {
				v := UpgradableVerdict(tc.monitor, tc.isSat, tc.incumbent)
				t.Errorf("Eligible = %v, want %v (status %s: %s)", got, tc.want, v.Status, v.Detail)
			}
		})
	}
}

// Every disqualifying reason has its own status, so a listing can say WHY a
// want is not upgradable rather than only that it is not.
func TestEveryDisqualificationHasItsOwnStatus(t *testing.T) {
	seen := map[UpgradeStatus]bool{}
	for _, v := range []UpgradeVerdict{
		UpgradableVerdict(false, true, score(held1080())),
		UpgradableVerdict(true, true, score(heldTerminal())),
		UpgradableVerdict(true, false, Evaluation{}),
		UpgradableVerdict(true, true, score(held1080())),
	} {
		if seen[v.Status] {
			t.Errorf("two different situations both report %s, so a listing cannot "+
				"tell them apart", v.Status)
		}
		seen[v.Status] = true
	}
	if len(seen) != 4 {
		t.Errorf("%d distinct statuses for four distinct situations", len(seen))
	}
}

// Determinism, because this drives events and an unstable answer would emit on
// every pass.
func TestUpgradeDecisionIsDeterministic(t *testing.T) {
	profile := upgradeProfile()
	req := UpgradeRequest{
		Monitor: true, Satisfied: true, Profile: profile,
		Incumbent: Incumbent{AssetID: "a1", Evaluation: score(held1080())},
		Candidates: []ReleaseCandidate{
			{ID: "zzz", Attributes: better1080()},
			{ID: "aaa", Attributes: better1080()},
			{ID: "mmm", Attributes: better1080()},
		},
	}
	first := ConsiderUpgrade(req)
	if first.Status != UpgradeAvailable {
		t.Fatalf("setup: %s", first.Detail)
	}
	for range 100 {
		again := ConsiderUpgrade(req)
		if again.Candidate.ID != first.Candidate.ID || again.Improvement != first.Improvement {
			t.Fatalf("chose %q then %q", first.Candidate.ID, again.Candidate.ID)
		}
	}
	// Ties among the candidates break on the id, so the chosen upgrade is
	// predictable rather than merely consistent.
	if first.Candidate.ID != "aaa" {
		t.Errorf("chose %q; candidate ties break on the id ascending", first.Candidate.ID)
	}
}

// A profile that is never terminal never stops looking, which is what the
// seeded `archival` profile is. The upgrade loop must stay open for it
// forever rather than deciding it is done.
func TestAnOpenEndedProfileIsAlwaysEligible(t *testing.T) {
	openEnded := policy.Profile{
		Name:   "archival",
		Accept: []policy.Rule{{Attribute: policy.AttrSource, Op: policy.OpNotIn, Value: policy.Texts("cam")}},
	}
	if err := openEnded.Validate(); err != nil {
		t.Fatal(err)
	}
	// Even a perfect release is not terminal under a profile with no terminal
	// rules.
	best := Evaluate(ReleaseCandidate{ID: "perfect", Attributes: heldTerminal()}, openEnded)
	if best.Terminal {
		t.Fatal("setup: a profile with no terminal rules is never terminal")
	}
	if !Eligible(true, true, best) {
		t.Error("a never-terminal profile must stay upgradable forever — that is what " +
			"\"never stop looking\" means")
	}
}
