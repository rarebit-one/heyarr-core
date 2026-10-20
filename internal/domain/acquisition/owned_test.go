package acquisition

import (
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
)

func ownedProfile(t *testing.T) policy.Profile {
	t.Helper()
	for _, p := range policy.Defaults() {
		if p.Name == "owned" {
			return p
		}
	}
	t.Fatal("policy.Defaults() does not seed `owned`, the game profile (ADR-0118)")
	return policy.Profile{}
}

// The seeded game profile judges what a game copy actually carries. A GOG
// offline installer is owned and DRM-free: accepted and finished. A Steam depot
// is owned but DRM-bound: kept, and still open to a DRM-free copy. An
// indexer's repack carries no store at all and is refused — as UNDETERMINED,
// with the rule named, never as a confident "not owned" (§63).
func TestTheOwnedProfileJudgesGameCopies(t *testing.T) {
	owned := ownedProfile(t)

	gog := Evaluate(ReleaseCandidate{ID: "gog", Attributes: Attributes{
		policy.AttrStore: policy.Text("gog"), policy.AttrDRMFree: policy.Flag(true),
		policy.AttrPlatform: policy.Text("windows"), policy.AttrSizeBytes: policy.Num(70 << 30),
	}}, owned)
	if !gog.Accepted || !gog.Terminal {
		t.Errorf("a GOG installer: accepted=%v terminal=%v, want both — reasons %+v", gog.Accepted, gog.Terminal, gog.Reasons)
	}

	steam := Evaluate(ReleaseCandidate{ID: "steam", Attributes: Attributes{
		policy.AttrStore: policy.Text("steam"), policy.AttrSizeBytes: policy.Num(70 << 30),
	}}, owned)
	if !steam.Accepted {
		t.Errorf("a Steam depot is an owned copy and must be accepted; reasons %+v", steam.Reasons)
	}
	if steam.Terminal {
		t.Error("a Steam depot, whose DRM-freeness is unknown, ended the want; it must stay open to a DRM-free copy")
	}
	if gog.Score <= steam.Score {
		t.Errorf("GOG scored %d, Steam %d; the DRM-free copy must rank first", gog.Score, steam.Score)
	}

	repack := Evaluate(ReleaseCandidate{ID: "repack", Title: "Some.Game-REPACK", Attributes: Attributes{
		policy.AttrSizeBytes: policy.Num(30 << 30),
	}}, owned)
	if repack.Accepted {
		t.Fatal("an indexer release with no store was accepted as an owned game copy")
	}
	r, ok := repack.Reason("store.in")
	if !ok || r.Result != ResultUndetermined {
		t.Errorf("the repack's store gate = %+v (found %v), want an undetermined store.in", r, ok)
	}
}

// Holding a Steam depot and then a GOG installer of the same game: the want is
// satisfied by the GOG copy, the one the profile ranks best.
func TestAHeldGOGInstallerSatisfiesAGameWant(t *testing.T) {
	owned := ownedProfile(t)
	v := EvaluateContent([]AssetView{
		{ID: "depot", BlobHash: "b1", Attributes: Attributes{
			policy.AttrStore: policy.Text("steam"), policy.AttrSizeBytes: policy.Num(1 << 30),
		}},
		{ID: "installer", BlobHash: "b2", Attributes: Attributes{
			policy.AttrStore: policy.Text("gog"), policy.AttrDRMFree: policy.Flag(true), policy.AttrSizeBytes: policy.Num(1 << 30),
		}},
	}, owned)
	if v.Satisfaction != SatisfactionSatisfied || v.SatisfiedBy != "installer" {
		t.Errorf("satisfaction %q by %q, want satisfied by the GOG installer", v.Satisfaction, v.SatisfiedBy)
	}
}
