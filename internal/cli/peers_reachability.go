package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/peer/reachability"
)

// checkTimeout bounds the whole enrolment-time check.
//
// It is one dial and one small request against a machine an operator is
// standing in front of. Ten seconds is generous for that and short enough that
// `peers add` against a host that blackholes packets returns while they are
// still watching, rather than looking like a hang.
const checkTimeout = 10 * time.Second

// checkPairing observes both legs of a pairing before it is enrolled (#186,
// ADR-0037).
//
// # Why this is at enrolment, and why it is here rather than in the API
//
// Replication needs traffic in both directions and the two flows run opposite
// ways — inventory is pushed peer → controller, bytes are pulled destination →
// source. A pairing that carries only one of them deadlocks, and it does so
// silently: reconciliation correctly emits nothing, because nothing ever told
// it the far node holds anything. Enrolment is the last moment at which a
// human is looking at the two machines and can act on the answer.
//
// It runs in the CLI, not in POST /api/v1/peers, and that is deliberate. The
// API's enrolment is a control-plane write and must stay one: a peer may
// legitimately be enrolled by key before it has an address at all, `heyarr all`
// serves that route with no peer identity configured, and a handler that made
// an outbound mTLS dial would make the single writer's latency depend on a
// remote machine. The CLI is where the operator typed the endpoint and is the
// same place #169 put the endpoint check, for the same reason.
//
// It NEVER fails the enrolment by itself. Every local problem — no identity on
// disk, no configuration, a peer surface that does not serve the route —
// yields ResultUnknown, and the decision table refuses only on an observation
// it actually made.
func checkPairing(
	ctx context.Context, c *client.Client, configPath, name, endpoint, publicKey string,
) reachability.Pairing {
	pairing := reachability.Pairing{
		PeerName: name, Endpoint: endpoint,
		Outbound: reachability.ResultUnknown,
		Return:   reachability.ResultUnknown,
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		pairing.Detail = "this node's configuration could not be read: " + err.Error()
		return pairing
	}
	target := client.Peer{Name: name, Endpoint: &endpoint, PublicKey: &publicKey}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	// The same pinned dialler `peers ping` uses, against the key the operator
	// just typed. A check that accepted whatever answered would report a
	// reachable stranger as a working pairing.
	dial, err := pinnedConnection(ctx, c, cfg, target, endpoint)
	if err != nil {
		// A local failure: no identity on disk, a key that does not parse, no
		// self peer. None of it says anything about the network.
		pairing.Detail = err.Error()
		return pairing
	}

	// Leg one: this node → the peer. Identity rather than a bare connection,
	// so that "reachable" means a pinned peer answered and not merely that a
	// port is open.
	var seen peerapi.IdentityResponse
	if err := dial.call(ctx, http.MethodGet, "/identity", nil, &seen); err != nil {
		// Unreachable, refused, or refusing this node's key. All three mean
		// the same thing to the decision table — nothing was demonstrated —
		// because a peer that is off and a peer that is firewalled are
		// indistinguishable from here and neither is the fault this check is
		// for.
		pairing.Outbound = reachability.ResultUnreachable
		pairing.Detail = err.Error()
		return pairing
	}
	pairing.Outbound = reachability.ResultReachable

	// Leg two: the peer → this node. Not observable here at all — an attempt
	// that never leaves the far machine leaves no trace on this one — so it
	// is asked for, over the leg that just worked.
	var back peerapi.Reachback
	if err := dial.call(ctx, http.MethodGet, "/reachback", nil, &back); err != nil {
		pairing.Detail = "the peer does not answer a return-path probe: " + err.Error()
		return pairing
	}
	switch back.Result {
	case reachability.ResultReachable, reachability.ResultUnreachable, reachability.ResultUnknown:
		pairing.Return = back.Result
	default:
		// A value this build does not know. Unknown is the safe reading: a
		// newer peer's vocabulary must never be read as a fault.
		pairing.Return = reachability.ResultUnknown
	}
	pairing.ReturnTarget = back.Target
	if back.Detail != "" {
		pairing.Detail = back.Detail
	}
	return pairing
}

// reportPairing writes the one line an operator gets when the check found
// nothing conclusive, and nothing at all when it did.
//
// A bidirectional pairing is silent on purpose: it is the expected outcome and
// the command already prints the enrolled peer. An unproven one says so,
// because "the check ran and proved nothing" and "the check passed" must not
// look the same — that confusion is how an operator concludes a pairing was
// verified when it was not.
func reportPairing(out io.Writer, p reachability.Pairing) {
	if p.Verdict() != reachability.VerdictUnproven {
		return
	}
	detail := ""
	if p.Detail != "" {
		detail = "\n  " + p.Detail
	}
	fmt.Fprintf(out, "note: this pairing was not verified in both directions (%s → %s is %s, "+
		"the return path is %s).%s\n"+
		"  Replication needs both directions: a peer pushes its inventory to the controller and a "+
		"destination pulls bytes from the source (#186, ADR-0037).\n"+
		"  Enrolling anyway — an unreachable peer is usually one that is not up yet. "+
		"Re-run `heyarr peers add` with the same key once it is, to have the pairing checked.\n",
		"this node", p.PeerName, p.Outbound, p.Return, detail)
}
