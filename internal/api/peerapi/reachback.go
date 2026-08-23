package peerapi

import (
	"context"
	"net/http"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/peer/reachability"
)

// ReturnPathProber answers "can this node reach the peer that is asking?"
//
// It is an interface here for the same reason InventorySink is: the peer
// surface establishes WHO is asking, and looking up that peer's address is the
// control plane's job. The peerID argument is the acting peer, derived from
// the client certificate — never from the request, which carries nothing at
// all.
//
// # There is no target in the request, and there must never be one
//
// The obvious shape for this route is "dial the address I give you and tell me
// what happened", and it is the wrong one: it turns every member into a blind
// port scanner pointed wherever the caller likes. The target is instead read
// out of THIS node's own membership record for the calling peer. A caller can
// therefore cause exactly one dial, to an address an operator here already
// wrote down, which is also the address the return flows would really use — so
// the check is more honest as well as narrower.
type ReturnPathProber interface {
	// ProbeReturnPath dials this node's record for peerID and reports what
	// happened, along with the address it tried. An error is reserved for a
	// failure of this node — reading its own membership table — and never for
	// an unreachable peer, which is an ordinary answer.
	ProbeReturnPath(ctx context.Context, peerID string) (result reachability.Result, target string, err error)
}

// Reachback is what GET /peer/v1/reachback answers (#186, ADR-0037).
//
// It reports the RETURN leg of a pairing: whether the node answering can reach
// the node asking. That is a fact the asking node cannot observe for itself —
// an attempt that never leaves the other machine leaves no trace here — and
// asking for it is what lets `peers add` refuse a one-way pairing while an
// operator is still at the terminal, instead of leaving it to surface weeks
// later as a reconciliation that quietly emits nothing.
type Reachback struct {
	// Result is reachable, unreachable or unknown. Unknown is a real answer:
	// this node may hold no endpoint for the caller, or serve no prober.
	Result reachability.Result `json:"result"`
	// Target is the address this node tried, empty when it tried none. It is
	// this node's own record OF THE CALLER, so a caller learns whether the
	// address recorded for it here is still the address it lives at.
	Target string `json:"target"`
	// Detail explains an unknown or an unreachable in words, for a human.
	Detail string `json:"detail"`
}

// handleReachback answers GET /peer/v1/reachback.
func (s *Server) handleReachback(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFrom(r.Context())
	if !ok {
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if s.returnPath == nil {
		// Answered rather than 503'd, and this is the deliberate difference
		// from the inventory route. An inventory report that is not recorded
		// must fail loudly, because the peer would otherwise believe its
		// report landed. A reachback that cannot be performed has an honest
		// value in its own vocabulary — unknown — and the caller's decision
		// table already refuses to draw a conclusion from it.
		s.writeJSON(w, r, Reachback{
			Result: reachability.ResultUnknown,
			Detail: "this node cannot probe a return path: it has no membership table behind its peer surface",
		})
		return
	}

	result, target, err := s.returnPath.ProbeReturnPath(r.Context(), principal.PeerID())
	if err != nil {
		s.log.Error("probing a return path failed",
			"request_id", httpapi.RequestIDFrom(r.Context()),
			"peer_id", principal.PeerID(), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	detail := ""
	switch result {
	case reachability.ResultUnreachable:
		detail = "nothing answered at " + target
	case reachability.ResultUnknown:
		detail = "this node has no endpoint recorded for peer " + principal.PeerID() +
			", so it has nowhere to dial back to"
	case reachability.ResultReachable:
	}
	s.log.Info("answered a return-path probe",
		"peer_id", principal.PeerID(), "peer_name", principal.Name(),
		"result", result, "target", target)
	s.writeJSON(w, r, Reachback{Result: result, Target: target, Detail: detail})
}
