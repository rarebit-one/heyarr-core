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
			if rep := got.Reportable(); rep != (want == reachability.VerdictReturnPathUnreachable) {
				t.Errorf("Decide(%s, %s).Reportable() = %v", outbound, ret, rep)
			}
		}
	}
}

// Only one verdict is reported, and it is the one that was actually observed.
//
// Nothing here refuses an enrolment any more: ADR-0038 makes a one-way peer an
// ordinary participant, so the observation is information rather than a fault
// (ADR-0037).
func TestOnlyTheObservedOneWayPairingIsReported(t *testing.T) {
	oneWay := reachability.Pairing{
		PeerName: "peer-b", Endpoint: "https://peer-b.invalid:8385",
		Outbound: reachability.ResultReachable, Return: reachability.ResultUnreachable,
		ReturnTarget: "https://peer-a.invalid:8385", Detail: "connection refused",
	}
	if got := oneWay.Verdict(); got != reachability.VerdictReturnPathUnreachable {
		t.Fatalf("verdict = %s, want %s", got, reachability.VerdictReturnPathUnreachable)
	}
	if got := oneWay.Failed(); got != reachability.DirectionReturn {
		t.Fatalf("failed direction = %s, want %s", got, reachability.DirectionReturn)
	}
	advisory := oneWay.Advisory()
	if advisory == "" {
		t.Fatal("a return-path-unreachable pairing produced no advisory")
	}
	// The advisory has to be actionable on its own: the direction, both
	// addresses, and what it does NOT mean.
	for _, want := range []string{
		"peer-b", "https://peer-b.invalid:8385", "https://peer-a.invalid:8385",
		"connection refused", "was enrolled", "not a fault",
	} {
		if !strings.Contains(advisory, want) {
			t.Errorf("the advisory does not mention %q:\n%s", want, advisory)
		}
	}
	// 🔴 It must not read as a refusal. The whole point of ADR-0037's rewrite
	// is that this pairing WORKS, so language implying otherwise would send an
	// operator to fix a network that does not need fixing.
	for _, forbidden := range []string{"was not enrolled", "cannot replicate", "--skip-"} {
		if strings.Contains(advisory, forbidden) {
			t.Errorf("the advisory still reads as a refusal — it contains %q:\n%s", forbidden, advisory)
		}
	}

	// And the two verdicts that must say nothing at all.
	for _, p := range []reachability.Pairing{
		{Outbound: reachability.ResultReachable, Return: reachability.ResultReachable},
		{Outbound: reachability.ResultUnreachable, Return: reachability.ResultUnknown},
	} {
		if got := p.Advisory(); got != "" {
			t.Errorf("a %s pairing produced an advisory: %v", p.Verdict(), got)
		}
		if got := p.Failed(); got != "" {
			t.Errorf("a %s pairing named %q as the failed direction", p.Verdict(), got)
		}
	}
}
