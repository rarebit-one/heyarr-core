package acquisition

import "testing"

// A package is placed only as well as its worst file: one installer part at a
// second site is not a backup of the game.
func TestPackagePlacementIsItsWorstFile(t *testing.T) {
	required := []string{"peer-a", "peer-b"}
	held := func(peers ...string) []PeerReplica {
		out := make([]PeerReplica, 0, len(peers))
		for _, p := range peers {
			out = append(out, PeerReplica{PeerID: p, Present: true})
		}
		return out
	}
	cases := []struct {
		name    string
		replica map[string][]PeerReplica
		want    Satisfaction
	}{
		{"every file on every peer", map[string][]PeerReplica{
			"b1": held("peer-a", "peer-b"), "b2": held("peer-a", "peer-b"),
		}, SatisfactionSatisfied},
		{"one file only on one peer", map[string][]PeerReplica{
			"b1": held("peer-a", "peer-b"), "b2": held("peer-a"),
		}, SatisfactionConverging},
		{"one file nowhere", map[string][]PeerReplica{
			"b1": held("peer-a", "peer-b"), "b2": nil,
		}, SatisfactionNot},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluatePackagePlacement([]string{"b1", "b2"}, required,
				func(b string) []PeerReplica { return tc.replica[b] })
			if got.Satisfaction != tc.want {
				t.Errorf("placement = %s (%s), want %s", got.Satisfaction, got.Detail, tc.want)
			}
		})
	}
	if got := EvaluatePackagePlacement(nil, required, func(string) []PeerReplica { return nil }); got.Satisfaction == SatisfactionSatisfied {
		t.Error("an empty package was placed")
	}
}
