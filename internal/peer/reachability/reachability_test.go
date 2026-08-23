package reachability_test

import (
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/peer/reachability"
)

// The decision table, enumerated exhaustively.
//
// Every assertion compares the verdict VALUE — never a substring of it. A
// substring test would let `return_path_unreachable` satisfy a check for
// `unreachable` and quietly turn "the peer is off" into "refuse this
// enrolment", which is the one confusion this table exists to prevent.
func TestDecide(t *testing.T) {
	all := []reachability.Result{
		reachability.ResultReachable,
		reachability.ResultUnreachable,
		reachability.ResultUnknown,
	}
	cases := map[reachability.Result]map[reachability.Result]reachability.Verdict{
		reachability.ResultReachable: {
			reachability.ResultReachable:   reachability.VerdictBidirectional,
			reachability.ResultUnreachable: reachability.VerdictReturnPathUnreachable,
			reachability.ResultUnknown:     reachability.VerdictUnproven,
		},
		// The outbound leg failed, so nothing could be asked of the peer and
		// the return leg is not observable. A node that is simply off looks
		// exactly like this, and refusing it would be the check doing more
		// harm than the fault it is for.
		reachability.ResultUnreachable: {
			reachability.ResultReachable:   reachability.VerdictUnproven,
			reachability.ResultUnreachable: reachability.VerdictUnproven,
			reachability.ResultUnknown:     reachability.VerdictUnproven,
		},
		reachability.ResultUnknown: {
			reachability.ResultReachable:   reachability.VerdictUnproven,
			reachability.ResultUnreachable: reachability.VerdictUnproven,
			reachability.ResultUnknown:     reachability.VerdictUnproven,
		},
	}
	for _, outbound := range all {
		for _, ret := range all {
			want := cases[outbound][ret]
			got := reachability.Decide(outbound, ret)
			if got != want {
				t.Errorf("Decide(%s, %s) = %s, want %s", outbound, ret, got, want)
			}
			if refuses := got.Refuses(); refuses != (want == reachability.VerdictReturnPathUnreachable) {
				t.Errorf("Decide(%s, %s).Refuses() = %v", outbound, ret, refuses)
			}
		}
	}
}

// Only one verdict refuses, and it is the one that was actually observed.
func TestOnlyTheObservedOneWayPairingRefuses(t *testing.T) {
	refusing := reachability.Pairing{
		PeerName: "peer-b", Endpoint: "https://peer-b.invalid:8385",
		Outbound: reachability.ResultReachable, Return: reachability.ResultUnreachable,
		ReturnTarget: "https://peer-a.invalid:8385", Detail: "connection refused",
	}
	if got := refusing.Verdict(); got != reachability.VerdictReturnPathUnreachable {
		t.Fatalf("verdict = %s, want %s", got, reachability.VerdictReturnPathUnreachable)
	}
	if got := refusing.Failed(); got != reachability.DirectionReturn {
		t.Fatalf("failed direction = %s, want %s", got, reachability.DirectionReturn)
	}
	err := refusing.Refusal()
	if err == nil {
		t.Fatal("a return-path-unreachable pairing produced no refusal")
	}
	// The refusal has to be actionable on its own: the direction, both
	// addresses, and the escape hatch.
	for _, want := range []string{
		string(reachability.DirectionReturn), "peer-b",
		"https://peer-b.invalid:8385", "https://peer-a.invalid:8385",
		"connection refused", "--skip-reachability-check",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}

	// And the two verdicts that must never refuse.
	for _, p := range []reachability.Pairing{
		{Outbound: reachability.ResultReachable, Return: reachability.ResultReachable},
		{Outbound: reachability.ResultUnreachable, Return: reachability.ResultUnknown},
	} {
		if err := p.Refusal(); err != nil {
			t.Errorf("a %s pairing refused an enrolment: %v", p.Verdict(), err)
		}
		if got := p.Failed(); got != "" {
			t.Errorf("a %s pairing named %q as the failed direction", p.Verdict(), got)
		}
	}
}
