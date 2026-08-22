package acquisition

import (
	"fmt"
	"sort"

	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
)

// Satisfaction reconciliation (§56, §57).
//
// §57 says Heyarr uses reconciliation consistently, and lists five kinds. Two
// of them belong here:
//
//	content reconciliation   DesiredItems vs Assets, qualified by the profile
//	peer convergence         the desired Blob set vs peer inventory
//
// They are evaluated SEPARATELY (§56) and combined by nobody: the two answers
// are stored as two axes and §64's name is derived from them (ADR-0027).
//
// # Both are pure functions over values
//
// Which is what makes the interesting cases — a profile that rejects
// everything you own, a blob on some peers and not others, a linked asset with
// no blob at all — testable without a database, a peer, or a library. The
// queries that produce the values live at the edge.

// AssetView is what content reconciliation needs to know about one asset.
//
// Deliberately not the persistence row: this package cannot import
// persistence, and the evaluator needs a handful of facts rather than
// everything an asset carries.
type AssetView struct {
	ID string
	// SourceClass is ADR-0020's managed / linked / vault.
	SourceClass string
	// BlobHash is empty for a linked asset, which has NO blob by design.
	BlobHash string
	// Attributes is what is known about the bytes — from the probe, the blob
	// size and the edition. A missing key means "could not be determined",
	// exactly as it does for a release candidate, and the evaluator treats it
	// the same way.
	Attributes Attributes
}

// Linked reports whether this asset has no blob (ADR-0020).
func (a AssetView) Linked() bool { return a.SourceClass == "linked" || a.BlobHash == "" }

// ContentVerdict is the content axis's answer plus why.
type ContentVerdict struct {
	Satisfaction Satisfaction
	// SatisfiedBy is the asset that satisfies, when one does. The BEST one, by
	// the same ranking the search uses — so "what am I watching" and "what
	// would I have acquired" cannot disagree.
	SatisfiedBy string
	// Evaluations is every asset that was considered, with its full §63
	// reasons. This is what makes "I have this film, why does Heyarr say it is
	// missing" answerable, and it is the question this axis will be asked most
	// often.
	Evaluations []AssetEvaluation
}

// AssetEvaluation is one asset scored against the profile.
type AssetEvaluation struct {
	AssetID    string
	Evaluation Evaluation
}

// EvaluateContent answers "do we hold bytes the quality profile accepts?" (§56)
//
// # Existing is not satisfying
//
// A 480p rip under a 1080p-minimum profile is content PRESENT and content NOT
// SATISFIED. Conflating the two makes the upgrade workflow unreachable — there
// would be nothing for it to improve on — and it is why §64 lists AVAILABLE
// and CONTENT_SATISFIED as different names.
//
// # No assets at all is unsatisfied, not unknown
//
// Something looked, and the answer is that there is nothing there. Unknown is
// for "nobody has looked", and a reconciliation pass IS someone looking.
func EvaluateContent(assets []AssetView, profile policy.Profile) ContentVerdict {
	verdict := ContentVerdict{Satisfaction: SatisfactionNot}
	if len(assets) == 0 {
		return verdict
	}

	// Reuse the release-candidate scorer rather than writing a second one.
	// "Is this good enough" must have ONE answer, or an asset can be
	// acceptable as a download and unacceptable once it is on disk — which
	// would make the upgrade workflow acquire the same file forever.
	candidates := make([]ReleaseCandidate, 0, len(assets))
	byID := make(map[string]AssetView, len(assets))
	for _, a := range assets {
		candidates = append(candidates, ReleaseCandidate{ID: a.ID, Attributes: a.Attributes})
		byID[a.ID] = a
	}

	ranked := EvaluateAll(candidates, profile)
	verdict.Evaluations = make([]AssetEvaluation, 0, len(ranked))
	for _, r := range ranked {
		verdict.Evaluations = append(verdict.Evaluations, AssetEvaluation{
			AssetID: r.Candidate.ID, Evaluation: r.Evaluation,
		})
	}
	if best, ok := Best(ranked); ok {
		verdict.Satisfaction = SatisfactionSatisfied
		verdict.SatisfiedBy = best.Candidate.ID
	}
	return verdict
}

// PeerReplica is one peer's holding of one blob.
type PeerReplica struct {
	PeerID string
	// Present is whether the peer holds VERIFIED bytes. A pending or corrupt
	// replica is not a replica for placement purposes: §56 asks whether the
	// content is replicated, and bytes that failed verification are not.
	Present bool
}

// PlacementVerdict is the placement axis's answer plus why.
type PlacementVerdict struct {
	Satisfaction Satisfaction
	// Missing lists the peers that should hold the bytes and do not, in a
	// stable order. It is the actionable half: "converging" with no list of
	// what is missing is a status nobody can act on.
	Missing []string
	Detail  string
}

// EvaluatePlacement answers "are those bytes on every Full Peer that should
// hold them?" (§56)
//
// ## PROVEN, and what the proof was
//
// This carried an UNPROVEN block from Milestone 1 to Milestone 4: nothing had
// ever run against a second peer, so `required` had one member, that member was
// this node, and SatisfactionConverging — the state this entire distinction
// exists to express — was unreachable outside a test with a synthetic peer set.
//
// It is reachable now. Milestone 4 stood up a second Full Peer and moved real
// bytes to it (M4-09), and `make demo` observes this function return
// SatisfactionConverging mid-transfer, from the API, on a blob that one of two
// required peers holds — then SatisfactionSatisfied once the transfer lands.
// The logic below did not change to earn that; what was missing was never the
// logic, it was a second peer.
//
// What is still true, and is now said on the wire rather than here: a
// deployment whose target set is this node alone gets `satisfied` the moment
// content is, and that is not evidence that replication works. That condition
// is reported per response as `unproven` (ADR-0027, resources.
// PlacementSatisfaction.Unproven) instead of being asserted as a blanket
// caveat, because it is a fact about a deployment and no longer a fact about
// this code.
func EvaluatePlacement(blobHash string, required []string, replicas []PeerReplica) PlacementVerdict {
	// ADR-0020: a linked asset has no blob, so there is nothing to replicate
	// and placement is not a question that can be answered about it.
	//
	// Calling that satisfied — zero required blobs are all present, vacuously
	// true — would make FULLY_SATISFIED mean "one copy, on one disk, with no
	// integrity guarantee and no way to verify it", which is the opposite of
	// what the name promises. This is the fifth site of the same gap; see
	// ADR-0020's consequences.
	if blobHash == "" {
		return PlacementVerdict{
			Satisfaction: SatisfactionNotApplicable,
			Detail: "the satisfying asset is linked and has no blob (ADR-0020), " +
				"so there is nothing to replicate",
		}
	}
	if len(required) == 0 {
		// No target set. Not satisfied and not vacuously true: a placement
		// policy naming no peers is a configuration that cannot be met, and
		// reporting success for it would hide the misconfiguration.
		return PlacementVerdict{
			Satisfaction: SatisfactionNot,
			Detail:       "no Full Peer is required to hold this, which cannot be right",
		}
	}

	present := make(map[string]bool, len(replicas))
	for _, r := range replicas {
		if r.Present {
			present[r.PeerID] = true
		}
	}

	var missing []string
	for _, peer := range required {
		if !present[peer] {
			missing = append(missing, peer)
		}
	}
	sort.Strings(missing)

	switch {
	case len(missing) == 0:
		return PlacementVerdict{
			Satisfaction: SatisfactionSatisfied,
			Detail: fmt.Sprintf("every required peer holds verified bytes (%d of %d)",
				len(required), len(required)),
		}
	case len(missing) == len(required):
		// Nowhere at all. Distinct from converging: converging means
		// replication is closing a gap, and a blob on no peer is not
		// converging on anything.
		return PlacementVerdict{
			Satisfaction: SatisfactionNot,
			Missing:      missing,
			Detail:       "no required peer holds verified bytes",
		}
	default:
		return PlacementVerdict{
			Satisfaction: SatisfactionConverging,
			Missing:      missing,
			Detail: fmt.Sprintf("%d of %d required peers hold verified bytes",
				len(required)-len(missing), len(required)),
		}
	}
}
