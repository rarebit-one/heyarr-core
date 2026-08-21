package acquisition

import (
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
)

// Content and placement, evaluated separately (§56, §57).

func asset(id, class, blob string, attrs Attributes) AssetView {
	return AssetView{ID: id, SourceClass: class, BlobHash: blob, Attributes: attrs}
}

func managed(id string, resolution int64, source string) AssetView {
	return asset(id, "managed", "blake3:"+id, Attributes{
		policy.AttrResolution: policy.Num(resolution),
		policy.AttrSource:     policy.Text(source),
		policy.AttrVideoCodec: policy.Text("h264"),
		policy.AttrHDR:        policy.Flag(false),
		policy.AttrSizeBytes:  policy.Num(8 << 30),
	})
}

// The distinction that makes the upgrade workflow reachable: existing is not
// satisfying.
func TestContentPresentIsNotContentSatisfied(t *testing.T) {
	profile := testProfile() // accept resolution >= 1080

	t.Run("a 480p rip is present and not satisfying", func(t *testing.T) {
		v := EvaluateContent([]AssetView{managed("a1", 480, "web-dl")}, profile)
		if v.Satisfaction != SatisfactionNot {
			t.Fatalf("satisfaction = %s, want not_satisfied", v.Satisfaction)
		}
		if v.SatisfiedBy != "" {
			t.Errorf("SatisfiedBy = %q with nothing satisfying", v.SatisfiedBy)
		}
		// And it says why, which is what makes "I have this film, why does
		// Heyarr say it is missing" answerable.
		if len(v.Evaluations) != 1 {
			t.Fatalf("%d evaluations for 1 asset", len(v.Evaluations))
		}
		rejections := v.Evaluations[0].Evaluation.RejectedBy()
		if len(rejections) == 0 {
			t.Fatal("the asset was rejected with no reason")
		}
		if rejections[0].Rule != "resolution.gte" {
			t.Errorf("rejected by %s, want resolution.gte", rejections[0].Rule)
		}
	})

	t.Run("a 1080p web-dl satisfies", func(t *testing.T) {
		v := EvaluateContent([]AssetView{managed("a1", 1080, "web-dl")}, profile)
		if v.Satisfaction != SatisfactionSatisfied {
			t.Fatalf("satisfaction = %s, want satisfied", v.Satisfaction)
		}
		if v.SatisfiedBy != "a1" {
			t.Errorf("SatisfiedBy = %q", v.SatisfiedBy)
		}
	})
}

// No assets at all is UNSATISFIED, not unknown: a reconciliation pass is
// someone looking, and the answer is that there is nothing there.
func TestNoAssetsIsUnsatisfiedNotUnknown(t *testing.T) {
	v := EvaluateContent(nil, testProfile())
	if v.Satisfaction != SatisfactionNot {
		t.Errorf("satisfaction = %s, want not_satisfied — unknown is for \"nobody has "+
			"looked\", and this pass looked", v.Satisfaction)
	}
	if len(v.Evaluations) != 0 {
		t.Errorf("%d evaluations for no assets", len(v.Evaluations))
	}
}

// One acceptable asset among many satisfies, and the BEST one is named — by
// the same ranking the search uses, so "what am I watching" and "what would I
// have acquired" cannot disagree.
func TestTheBestAssetSatisfies(t *testing.T) {
	profile := testProfile() // prefers hevc (20) and hdr (10)

	best := asset("best", "managed", "blake3:best", Attributes{
		policy.AttrResolution: policy.Num(2160),
		policy.AttrSource:     policy.Text("remux"),
		policy.AttrVideoCodec: policy.Text("hevc"),
		policy.AttrHDR:        policy.Flag(true),
		policy.AttrSizeBytes:  policy.Num(30 << 30),
	})
	v := EvaluateContent([]AssetView{
		managed("plain", 1080, "web-dl"),
		best,
		managed("bad", 480, "cam"),
	}, profile)

	if v.Satisfaction != SatisfactionSatisfied {
		t.Fatalf("satisfaction = %s", v.Satisfaction)
	}
	if v.SatisfiedBy != "best" {
		t.Errorf("SatisfiedBy = %q, want best", v.SatisfiedBy)
	}
	// Every asset is reported, not only the winner — an operator asking why a
	// copy they own was passed over needs to see it.
	if len(v.Evaluations) != 3 {
		t.Errorf("%d evaluations for 3 assets", len(v.Evaluations))
	}
}

// One scorer, one answer. An asset acceptable as a download must be acceptable
// once it is on disk, or the upgrade workflow acquires the same file forever.
func TestContentUsesTheSameScorerAsTheSearch(t *testing.T) {
	profile := testProfile()
	attrs := Attributes{
		policy.AttrResolution: policy.Num(1080),
		policy.AttrSource:     policy.Text("web-dl"),
		policy.AttrVideoCodec: policy.Text("hevc"),
		policy.AttrHDR:        policy.Flag(true),
		policy.AttrSizeBytes:  policy.Num(8 << 30),
	}

	asDownload := Evaluate(ReleaseCandidate{ID: "x", Attributes: attrs}, profile)
	onDisk := EvaluateContent([]AssetView{
		asset("x", "managed", "blake3:x", attrs),
	}, profile)

	if asDownload.Accepted != (onDisk.Satisfaction == SatisfactionSatisfied) {
		t.Fatalf("the same bytes are acceptable as a download (%v) and as an asset (%v)",
			asDownload.Accepted, onDisk.Satisfaction)
	}
	if onDisk.Evaluations[0].Evaluation.Score != asDownload.Score {
		t.Errorf("scored %d as a download and %d as an asset",
			asDownload.Score, onDisk.Evaluations[0].Evaluation.Score)
	}
}

// Placement (§56). Every case below is UNPROVEN against a real second peer —
// ADR-0010 puts exactly one in the model — so these are synthetic peer sets
// and nothing more.
func TestEvaluatePlacement(t *testing.T) {
	const blob = "blake3:aaaa"

	cases := []struct {
		name        string
		blobHash    string
		required    []string
		replicas    []PeerReplica
		want        Satisfaction
		wantMissing []string
		wantDetail  string
	}{
		{
			name: "every required peer holds it", blobHash: blob,
			required: []string{"peer-a", "peer-b"},
			replicas: []PeerReplica{{"peer-a", true}, {"peer-b", true}},
			want:     SatisfactionSatisfied,
		},
		{
			// §56's own example: content exists, Site A yes, Site B no.
			name: "one of two peers holds it", blobHash: blob,
			required:    []string{"peer-a", "peer-b"},
			replicas:    []PeerReplica{{"peer-a", true}},
			want:        SatisfactionConverging,
			wantMissing: []string{"peer-b"},
			wantDetail:  "1 of 2",
		},
		{
			// Nowhere at all is NOT converging — converging means replication
			// is closing a gap, and a blob on no peer is not closing anything.
			name: "no peer holds it", blobHash: blob,
			required:    []string{"peer-a", "peer-b"},
			replicas:    nil,
			want:        SatisfactionNot,
			wantMissing: []string{"peer-a", "peer-b"},
		},
		{
			// A pending or corrupt replica is not a replica for placement:
			// §56 asks whether the content is replicated, and bytes that
			// failed verification are not.
			name: "an unverified replica does not count", blobHash: blob,
			required:    []string{"peer-a", "peer-b"},
			replicas:    []PeerReplica{{"peer-a", true}, {"peer-b", false}},
			want:        SatisfactionConverging,
			wantMissing: []string{"peer-b"},
		},
		{
			name: "the single-peer deployment", blobHash: blob,
			required: []string{"peer-a"},
			replicas: []PeerReplica{{"peer-a", true}},
			want:     SatisfactionSatisfied,
		},
		{
			// ADR-0020's fifth site.
			name: "a linked asset has nothing to place", blobHash: "",
			required:   []string{"peer-a"},
			want:       SatisfactionNotApplicable,
			wantDetail: "ADR-0020",
		},
		{
			// A policy naming no peers cannot be met, and reporting success
			// would hide the misconfiguration.
			name: "no required peers is a misconfiguration", blobHash: blob,
			required: nil,
			replicas: []PeerReplica{{"peer-a", true}},
			want:     SatisfactionNot,
		},
		{
			// A peer holding it that was not required is not an error.
			name: "an extra peer is harmless", blobHash: blob,
			required: []string{"peer-a"},
			replicas: []PeerReplica{{"peer-a", true}, {"somewhere-else", true}},
			want:     SatisfactionSatisfied,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluatePlacement(tc.blobHash, tc.required, tc.replicas)
			if got.Satisfaction != tc.want {
				t.Fatalf("satisfaction = %s, want %s (%s)", got.Satisfaction, tc.want, got.Detail)
			}
			if strings.Join(got.Missing, ",") != strings.Join(tc.wantMissing, ",") {
				t.Errorf("missing = %v, want %v", got.Missing, tc.wantMissing)
			}
			if tc.wantDetail != "" && !strings.Contains(got.Detail, tc.wantDetail) {
				t.Errorf("detail = %q, want it to mention %q", got.Detail, tc.wantDetail)
			}
			// "Converging" with no list of what is missing is a status nobody
			// can act on.
			if got.Satisfaction == SatisfactionConverging && len(got.Missing) == 0 {
				t.Error("converging must name the peers that are missing")
			}
			if strings.TrimSpace(got.Detail) == "" {
				t.Error("every verdict carries an explanation")
			}
		})
	}
}

// The missing list is stable, because it appears in an event payload and in the
// API, and an order that depends on map iteration is one nobody can diff.
func TestMissingPeersAreOrdered(t *testing.T) {
	required := []string{"zulu", "alpha", "mike"}
	for range 50 {
		got := EvaluatePlacement("blake3:x", required, []PeerReplica{{"mike", true}})
		if strings.Join(got.Missing, ",") != "alpha,zulu" {
			t.Fatalf("missing = %v, want [alpha zulu]", got.Missing)
		}
	}
}

// The two axes are evaluated independently, which is the whole of §56. Content
// can be satisfied while placement is not, and neither computation can see the
// other's inputs.
func TestTheAxesAreIndependent(t *testing.T) {
	profile := testProfile()
	content := EvaluateContent([]AssetView{managed("a1", 1080, "web-dl")}, profile)
	if content.Satisfaction != SatisfactionSatisfied {
		t.Fatal("setup")
	}

	placement := EvaluatePlacement("blake3:a1", []string{"peer-a", "peer-b"},
		[]PeerReplica{{"peer-a", true}})
	if placement.Satisfaction != SatisfactionConverging {
		t.Fatal("setup")
	}

	// And they combine into the state §64 names, without either being derived
	// from the other.
	state := State{
		Phase: PhaseIdle, Managed: true,
		Content: content.Satisfaction, Placement: placement.Satisfaction,
	}
	if err := state.Validate(); err != nil {
		t.Fatal(err)
	}
	if state.Name() != "PLACEMENT_CONVERGING" {
		t.Errorf("Name() = %s, want PLACEMENT_CONVERGING", state.Name())
	}
}

// A linked asset satisfies content and can never satisfy placement, so it rests
// at CONTENT_SATISFIED permanently. That is honest, and it needs no new name.
func TestALinkedAssetRestsAtContentSatisfied(t *testing.T) {
	linked := asset("linked", "linked", "", Attributes{
		policy.AttrResolution: policy.Num(2160),
		policy.AttrSource:     policy.Text("remux"),
		policy.AttrVideoCodec: policy.Text("hevc"),
		policy.AttrHDR:        policy.Flag(true),
		policy.AttrSizeBytes:  policy.Num(30 << 30),
	})
	if !linked.Linked() {
		t.Fatal("an asset with no blob is linked")
	}

	content := EvaluateContent([]AssetView{linked}, testProfile())
	if content.Satisfaction != SatisfactionSatisfied {
		t.Fatal("a linked asset is playable and can satisfy content — telling an operator " +
			"to re-acquire something they already have is wrong")
	}

	placement := EvaluatePlacement(linked.BlobHash, []string{"peer-a"}, nil)
	if placement.Satisfaction != SatisfactionNotApplicable {
		t.Fatalf("placement = %s, want not_applicable", placement.Satisfaction)
	}

	state := State{
		Phase: PhaseIdle, Managed: true,
		Content: content.Satisfaction, Placement: placement.Satisfaction,
	}
	if state.Name() != "CONTENT_SATISFIED" {
		t.Errorf("Name() = %s", state.Name())
	}
	if state.Name() == "FULLY_SATISFIED" {
		t.Error("a linked asset can never be fully satisfied — there is nothing to replicate")
	}
}

// Determinism, because these answers drive events and an unstable answer would
// emit on every pass.
func TestContentEvaluationIsDeterministic(t *testing.T) {
	profile := testProfile()
	assets := []AssetView{
		managed("b", 1080, "web-dl"),
		managed("a", 1080, "web-dl"),
		managed("c", 480, "cam"),
	}
	first := EvaluateContent(assets, profile)
	for range 100 {
		again := EvaluateContent(assets, profile)
		if again.SatisfiedBy != first.SatisfiedBy {
			t.Fatalf("satisfied by %q then %q", first.SatisfiedBy, again.SatisfiedBy)
		}
		for i := range again.Evaluations {
			if again.Evaluations[i].AssetID != first.Evaluations[i].AssetID {
				t.Fatalf("evaluation order moved at %d", i)
			}
		}
	}
	// Tied assets resolve by id, so the winner is predictable.
	if first.SatisfiedBy != "a" {
		t.Errorf("SatisfiedBy = %q; ties break on the id ascending", first.SatisfiedBy)
	}
}
