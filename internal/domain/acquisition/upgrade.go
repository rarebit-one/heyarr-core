package acquisition

import (
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
)

// The upgrade workflow (§60).
//
// # Satisfied is not the same as finished
//
// §60 retains upgrades among the things Heyarr keeps from *arr, and the whole
// feature lives in the gap between three states:
//
//	not accepted   MISSING, keep searching             (§56's content axis)
//	accepted       satisfied, and still improvable     <- this file
//	terminal       done, stop looking                  (§62's stop condition)
//
// M3-01 drew `terminal` as a stop condition rather than a top preference
// precisely so this loop has somewhere to stop, and M3-04 reports terminality
// as a distinct fact so this loop does not recompute it. Both decisions are
// spent here.
//
// # "Better" is the same function that decided acceptance
//
// A candidate is an upgrade only if Evaluate scores it above the incumbent
// under the same profile. Not a different comparison, not a heuristic, not
// "newer" — the same deterministic scorer, so the explanation for an upgrade
// is the same kind of artifact as the explanation for a rejection.
//
// Introducing a second notion of better is how the two drift, and how "why did
// it replace my file" stops being answerable.
//
// # A tie is not an upgrade
//
// Strictly better, or nothing. With a deterministic scorer and a stable
// tie-break (M3-04), an equal score means the incumbent stays — otherwise a
// library churns forever between two equivalent releases, each replacing the
// other, which is a bug that presents as bandwidth rather than as an error.
//
// # This is a pure function
//
// No database, no queue, no clock. It takes the incumbent's evaluation, the
// candidates and the profile, and returns a verdict with its reasons. The
// queries that produce those values live at the edge, which is what makes the
// interesting cases — a tie, an unmonitored want, a terminal incumbent —
// testable without a library.

// UpgradeStatus is why an upgrade scan reached the answer it did.
//
// It is an enumerated reason rather than a bare boolean because "no upgrade"
// has four completely different meanings, and an operator asking "why is this
// not upgrading" needs to know which. A boolean answers a question nobody was
// asking.
type UpgradeStatus string

const (
	// UpgradeAvailable is a strictly better candidate.
	UpgradeAvailable UpgradeStatus = "available"
	// UpgradeNotMonitored is a want the operator said to leave alone.
	//
	// §60 keeps monitored and wanted as two words. "Get me this" is not "keep
	// improving this", and running the loop over unmonitored items is how *arr
	// installations re-download libraries nobody asked them to touch.
	UpgradeNotMonitored UpgradeStatus = "not_monitored"
	// UpgradeTerminal is an incumbent that already meets every terminal
	// condition. There is nothing left to want (§62).
	UpgradeTerminal UpgradeStatus = "terminal"
	// UpgradeNotSatisfied is a want with no acceptable incumbent at all.
	//
	// That is not an upgrade, it is an ACQUISITION — the want is MISSING and
	// the search job owns it. Reporting it as "upgradable" would make the
	// upgrade scan and the ordinary search fight over the same rows.
	UpgradeNotSatisfied UpgradeStatus = "not_satisfied"
	// UpgradeNoBetterCandidate is a satisfied, monitored, non-terminal want
	// where nothing on offer beats what is held.
	//
	// This is the NORMAL answer for a healthy library and it must be cheap and
	// silent: most wants are in this state most of the time.
	UpgradeNoBetterCandidate UpgradeStatus = "no_better_candidate"
)

// Upgradable reports whether this status means work is available.
func (s UpgradeStatus) Upgradable() bool { return s == UpgradeAvailable }

// Incumbent is what is currently held for a want.
//
// The Evaluation is the one EvaluateContent already produced. Passing the
// evaluation rather than the asset is deliberate: it makes it structurally
// impossible for this package to re-score the incumbent under different rules
// from the ones that decided it was satisfying in the first place.
type Incumbent struct {
	// AssetID is the asset that currently satisfies the want.
	AssetID string
	// Evaluation is that asset's score under the want's profile, as computed
	// by EvaluateContent.
	Evaluation Evaluation
}

// UpgradeVerdict is an upgrade scan's answer plus why.
type UpgradeVerdict struct {
	Status UpgradeStatus
	// Detail is the prose half, for a human reading a status page.
	Detail string
	// Candidate is the strictly better release, when there is one.
	Candidate ReleaseCandidate
	// Evaluation is that candidate's score, so the explanation for an upgrade
	// is §63's reasons and not a separate prose string invented here.
	Evaluation Evaluation
	// Improvement is how much better, which is what makes an upgrade
	// reviewable: "score 30 to 45" tells an operator whether the replacement
	// is worth the bandwidth in a way "an upgrade is available" does not.
	Improvement int
}

// UpgradeRequest is everything an upgrade decision needs.
type UpgradeRequest struct {
	// Monitor is the want's monitored flag (§60). False short-circuits
	// everything: an unmonitored want is finished the moment it is satisfied.
	Monitor bool
	// Incumbent is what is held. A zero AssetID means nothing acceptable is
	// held, which is an acquisition rather than an upgrade.
	Incumbent Incumbent
	// Satisfied is §56's content axis. It is passed rather than inferred from
	// the incumbent because "satisfied" is reconciliation's answer, and
	// re-deriving it here would be a second opinion about the same question.
	Satisfied bool
	// Candidates are the releases on offer.
	Candidates []ReleaseCandidate
	// Profile is the standard both sides are measured against — the SAME one,
	// which is the point.
	Profile policy.Profile
}

// ConsiderUpgrade decides whether a want should be upgraded (§60).
//
// The order of the checks is the cheapness order and also the meaning order:
// monitoring is the operator's instruction and outranks everything; a want
// with no acceptable incumbent is not an upgrade at all; a terminal incumbent
// is finished; and only then is it worth scoring anything.
func ConsiderUpgrade(req UpgradeRequest) UpgradeVerdict {
	// The operator's instruction, first and cheapest. An unmonitored want is
	// finished, terminal or not — checked BEFORE terminality so that
	// "not_monitored" is the reported reason even for a want that happens to
	// also be terminal. The operator's decision is the more useful answer.
	if !req.Monitor {
		return UpgradeVerdict{
			Status: UpgradeNotMonitored,
			Detail: "this want is not monitored, so it is finished once it is satisfied",
		}
	}

	// No acceptable incumbent means the want is MISSING, which the search job
	// owns. Reporting it here would make two jobs fight over the same row.
	if !req.Satisfied || req.Incumbent.AssetID == "" {
		return UpgradeVerdict{
			Status: UpgradeNotSatisfied,
			Detail: "nothing acceptable is held, so this is an acquisition rather than an upgrade",
		}
	}

	// Terminality is READ from the incumbent's evaluation, never recomputed.
	// One implementation of "is this as good as it gets", in Evaluate.
	if req.Incumbent.Evaluation.Terminal {
		return UpgradeVerdict{
			Status: UpgradeTerminal,
			Detail: "what is held already meets every terminal condition, so there is nothing left to want",
		}
	}

	incumbentScore := req.Incumbent.Evaluation.Score
	ranked := EvaluateAll(req.Candidates, req.Profile)
	best, ok := Best(ranked)
	if !ok {
		return UpgradeVerdict{
			Status: UpgradeNoBetterCandidate,
			Detail: fmt.Sprintf("no candidate is acceptable; what is held scores %d", incumbentScore),
		}
	}

	// STRICTLY better. A tie leaves the incumbent alone — otherwise two
	// equivalent releases replace each other forever.
	if best.Evaluation.Score <= incumbentScore {
		verb := "matches"
		if best.Evaluation.Score < incumbentScore {
			verb = "is worse than"
		}
		return UpgradeVerdict{
			Status: UpgradeNoBetterCandidate,
			Detail: fmt.Sprintf("the best candidate scores %d, which %s what is held (%d)",
				best.Evaluation.Score, verb, incumbentScore),
		}
	}

	return UpgradeVerdict{
		Status:      UpgradeAvailable,
		Candidate:   best.Candidate,
		Evaluation:  best.Evaluation,
		Improvement: best.Evaluation.Score - incumbentScore,
		Detail: fmt.Sprintf("a candidate scores %d against the %d that is held",
			best.Evaluation.Score, incumbentScore),
	}
}

// UpgradableVerdict answers the narrower question §71's `get_upgrade_candidates`
// asks: is this want in a state where an upgrade COULD happen, without knowing
// what is on offer.
//
// It is separate from ConsiderUpgrade because the two are asked at different
// times by different things. "Which of my wants might improve" is a listing
// question, answerable from state alone and cheap enough to run over a whole
// library; "is this particular release better" needs a search first. Fusing
// them would make the listing perform a search per row.
func UpgradableVerdict(monitor, satisfied bool, incumbent Evaluation) UpgradeVerdict {
	return ConsiderUpgrade(UpgradeRequest{
		Monitor:   monitor,
		Satisfied: satisfied,
		Incumbent: Incumbent{AssetID: incumbentAssetID(satisfied, incumbent), Evaluation: incumbent},
		// No candidates: this asks whether the want is ELIGIBLE, not whether
		// anything better exists. With none on offer an eligible want reports
		// no_better_candidate, which the caller reads as "eligible, nothing
		// found yet".
	})
}

// incumbentAssetID gives ConsiderUpgrade an incumbent when one is claimed, so
// the eligibility question does not fall through the "nothing held" branch for
// a want that is genuinely satisfied.
func incumbentAssetID(satisfied bool, e Evaluation) string {
	if !satisfied {
		return ""
	}
	if e.CandidateID != "" {
		return e.CandidateID
	}
	// Satisfied with an evaluation carrying no id is possible only in a
	// caller's synthetic value. A placeholder keeps eligibility answerable
	// rather than mis-reporting the want as unsatisfied.
	return "unknown"
}

// Eligible reports whether a want is in a state where an upgrade could happen:
// monitored, satisfied, and not yet terminal.
//
// This is the predicate `GET /api/v1/desired?upgradable=true` filters on, and
// §71's get_upgrade_candidates will expose.
func Eligible(monitor, satisfied bool, incumbent Evaluation) bool {
	v := UpgradableVerdict(monitor, satisfied, incumbent)
	// Eligible means "nothing about the want's STATE rules an upgrade out".
	// With no candidates supplied, that is exactly the no_better_candidate
	// answer: every disqualifying reason has its own status.
	return v.Status == UpgradeNoBetterCandidate || v.Status == UpgradeAvailable
}
