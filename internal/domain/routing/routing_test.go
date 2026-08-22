package routing_test

import (
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/routing"
)

// The router is a pure function, so the combinatorics of locality × health ×
// replica state are table-testable exhaustively here, and the API layer's
// tests can then be about the wiring rather than about re-proving the rules.

func local(id string) routing.Candidate {
	return routing.Candidate{
		PeerID: id, Name: id, Site: "site-a", Endpoint: "http://" + id + ":7777",
		SameSite: true, Reachable: true, HealthState: "reachable", ReplicaState: "present",
	}
}

func remote(id string) routing.Candidate {
	c := local(id)
	c.Site = "site-b"
	c.SameSite = false
	return c
}

func codesOf(reasons []routing.Reason) []string {
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		out = append(out, r.Code)
	}
	return out
}

// rejectionCodes maps peer id to the codes it was rejected for.
func rejectionCodes(d routing.Decision) map[string][]string {
	out := map[string][]string{}
	for _, r := range d.Rejected {
		out[r.PeerID] = codesOf(r.Reasons)
	}
	return out
}

func hasCode(codes []string, want string) bool {
	for _, c := range codes {
		if c == want {
			return true
		}
	}
	return false
}

// The whole preference, as one table. Each row asserts the SELECTED PEER ID
// rather than that something was selected: "a source was found" passes on an
// implementation that picks the first row of the table.
func TestSelectionPrefersLocalityAndFiltersOnHealthAndReplicaState(t *testing.T) {
	t.Parallel()

	unhealthy := func(c routing.Candidate) routing.Candidate {
		c.Reachable, c.HealthState = false, "unreachable"
		return c
	}
	replica := func(c routing.Candidate, state string) routing.Candidate {
		c.ReplicaState = state
		return c
	}

	for _, tc := range []struct {
		name       string
		candidates []routing.Candidate
		want       string
		wantReason string
	}{
		{
			name:       "a local peer beats a remote one listed first",
			candidates: []routing.Candidate{remote("b"), local("a")},
			want:       "a", wantReason: routing.SelectedSiteLocal,
		},
		{
			// The id tie-break favours the REMOTE peer here, so nothing but
			// §31 can produce this answer. The row above would still pass on
			// an implementation that had stopped looking at sites and fallen
			// through to the tie-break.
			name:       "a local peer beats a remote one that sorts first by id",
			candidates: []routing.Candidate{remote("a"), local("z")},
			want:       "z", wantReason: routing.SelectedSiteLocal,
		},
		{
			name:       "a remote peer is used rather than refusing",
			candidates: []routing.Candidate{remote("b")},
			want:       "b", wantReason: routing.SelectedCrossSiteFallback,
		},
		{
			name:       "an unhealthy local peer loses to a healthy remote one",
			candidates: []routing.Candidate{unhealthy(local("a")), remote("b")},
			want:       "b", wantReason: routing.SelectedCrossSiteFallback,
		},
		{
			name:       "a pending local replica is not a source",
			candidates: []routing.Candidate{replica(local("a"), "pending"), remote("b")},
			want:       "b", wantReason: routing.SelectedCrossSiteFallback,
		},
		{
			name:       "a corrupt local replica is not a source",
			candidates: []routing.Candidate{replica(local("a"), "corrupt"), remote("b")},
			want:       "b", wantReason: routing.SelectedCrossSiteFallback,
		},
		{
			name:       "a missing local replica is not a source",
			candidates: []routing.Candidate{replica(local("a"), "missing"), remote("b")},
			want:       "b", wantReason: routing.SelectedCrossSiteFallback,
		},
		{
			name:       "a peer with no replica row at all is not a source",
			candidates: []routing.Candidate{replica(local("a"), ""), remote("b")},
			want:       "b", wantReason: routing.SelectedCrossSiteFallback,
		},
		{
			name: "a remote peer with no endpoint cannot be routed to",
			candidates: []routing.Candidate{
				func() routing.Candidate { c := remote("b"); c.Endpoint = ""; return c }(),
				remote("c"),
			},
			want: "c", wantReason: routing.SelectedCrossSiteFallback,
		},
		{
			name: "this node wins among peers at the same site",
			candidates: []routing.Candidate{
				local("a"),
				func() routing.Candidate { c := local("z"); c.IsSelf = true; c.Endpoint = ""; return c }(),
			},
			want: "z", wantReason: routing.SelectedSiteLocal,
		},
		{
			name:       "same-site peers tie-break deterministically by id",
			candidates: []routing.Candidate{local("m"), local("d")},
			want:       "d", wantReason: routing.SelectedSiteLocal,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := routing.Select(tc.candidates)
			if !got.Found {
				t.Fatalf("no source selected; refusal was: %s", got.Refusal())
			}
			if got.Source.PeerID != tc.want {
				t.Errorf("selected %q, want %q", got.Source.PeerID, tc.want)
			}
			if got.Reason.Code != tc.wantReason {
				t.Errorf("reason = %q, want %q (%s)", got.Reason.Code, tc.wantReason, got.Reason.Detail)
			}
		})
	}
}

// The self peer needs no endpoint and every other peer does. Without the
// second half, a remote peer with an empty endpoint would be handed back as a
// RELATIVE url — which resolves against the controller, which is precisely
// what §32 forbids.
func TestThisNodeNeedsNoEndpointAndEveryOtherPeerDoes(t *testing.T) {
	t.Parallel()

	self := local("self")
	self.IsSelf = true
	self.Endpoint = ""
	if d := routing.Select([]routing.Candidate{self}); !d.Found || d.Source.PeerID != "self" {
		t.Errorf("this node was not routed to without an endpoint: %s", d.Refusal())
	}

	stranger := remote("b")
	stranger.Endpoint = ""
	d := routing.Select([]routing.Candidate{stranger})
	if d.Found {
		t.Fatal("a peer with no endpoint was selected; the client has nowhere to go")
	}
	if got := rejectionCodes(d)["b"]; !hasCode(got, routing.RejectNoEndpoint) {
		t.Errorf("codes = %v, want %q", got, routing.RejectNoEndpoint)
	}
}

// A refusal names every peer considered and every reason each failed. One
// reason per peer would send an operator to fix the first of two problems and
// produce a second question when it did not help.
func TestARefusalNamesEveryPeerAndEveryReason(t *testing.T) {
	t.Parallel()

	down := local("a")
	down.Reachable, down.HealthState = false, "unreachable"
	down.ReplicaState = "corrupt"

	empty := remote("b")
	empty.ReplicaState = ""

	unprobed := remote("c")
	unprobed.Reachable, unprobed.HealthState = false, "unknown"

	d := routing.Select([]routing.Candidate{down, empty, unprobed})
	if d.Found {
		t.Fatalf("a source was selected from nothing usable: %+v", d.Source)
	}
	if len(d.Rejected) != 3 {
		t.Fatalf("rejected %d peers, want all 3 considered: %+v", len(d.Rejected), d.Rejected)
	}
	codes := rejectionCodes(d)
	for peer, want := range map[string][]string{
		"a": {routing.RejectReplicaNotUsable, routing.RejectPeerUnhealthy},
		"b": {routing.RejectNoReplica},
		"c": {routing.RejectPeerUnhealthy},
	} {
		for _, code := range want {
			if !hasCode(codes[peer], code) {
				t.Errorf("peer %s codes = %v, want %q among them", peer, codes[peer], code)
			}
		}
	}
	if len(codes["a"]) != 2 {
		t.Errorf("peer a was rejected for %v; both problems must be named", codes["a"])
	}

	// And the prose form carries the same content, for the channels that only
	// carry a string.
	refusal := d.Refusal()
	for _, want := range []string{"a", "b", "c", "corrupt", "unreachable", "no replica", "unknown"} {
		if !strings.Contains(refusal, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, refusal)
		}
	}
}

// A cross-site peer that lost to a local one is rejected WITH NO FAULT IN IT,
// and saying so is how an operator sees the fallback existed.
func TestAnEligibleRunnerUpIsRejectedWithTheLocalityReason(t *testing.T) {
	t.Parallel()

	d := routing.Select([]routing.Candidate{local("a"), remote("b")})
	if !d.Found || d.Source.PeerID != "a" {
		t.Fatalf("selected %+v, want a", d.Source)
	}
	if got := rejectionCodes(d)["b"]; !hasCode(got, routing.RejectSiteLocalPreferred) {
		t.Errorf("codes = %v, want %q", got, routing.RejectSiteLocalPreferred)
	}
}

// No peers at all is a refusal that says so, rather than an empty one.
func TestNoPeersAtAllStillExplainsItself(t *testing.T) {
	t.Parallel()

	d := routing.Select(nil)
	if d.Found {
		t.Fatal("a source was selected from no candidates")
	}
	if d.Refusal() == "" {
		t.Error("the refusal is empty; 'unavailable' and nothing else is the three-hour outage")
	}
}
