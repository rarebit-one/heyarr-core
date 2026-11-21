package peerapi

import (
	"context"
	"net/http"
	"strings"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
)

// FanOutPath is where a sibling asks what this node did with the acquire ops
// the sibling signed (ADR-0119 #924 amendment). A constant so the asker and
// the route cannot disagree about it.
const FanOutPath = Prefix + "/fan-out"

// maxFanOutOpParam bounds the `op` query parameter. An op hash is a short
// digest; anything longer is not one.
const maxFanOutOpParam = 128

// MaxFanOutApplications bounds one answer. The origin asks about one op at a
// time; an unfiltered ask is a diagnostic and gets the newest this many.
const MaxFanOutApplications = 500

// FanOutAnswer is this node's outcomes for the acquire ops one sibling signed.
//
// It is a read, for display, and nothing acts on it: the origin shows it in
// acquisition status and decides nothing from it. A decision that has to
// survive the sibling being unreachable rides the op-log (the `unsourced`
// report), not this.
type FanOutAnswer struct {
	// Site is this node's configured site, so the origin can name who
	// answered without a second lookup.
	Site string `json:"site"`
	// Applications are this node's outcomes for the caller's acquire ops,
	// most recently applied first. Empty, never null.
	Applications []FanOutApplication `json:"applications"`
}

// FanOutApplication is what this node did with one of the caller's acquire ops.
type FanOutApplication struct {
	OpHash string `json:"op_hash"`
	// Outcome is applied, held, deferred, tombstoned, scope_unresolved or
	// no_site_source.
	Outcome   string `json:"outcome"`
	Detail    string `json:"detail,omitempty"`
	AppliedAt string `json:"applied_at"`
	// State is the §64 state of the want the op reached here ("DOWNLOADING",
	// "AVAILABLE", ...), absent when it reached none.
	State string `json:"state,omitempty"`
	// ReportedUnsourced is whether this node signed an `unsourced` op for it.
	ReportedUnsourced bool `json:"reported_unsourced"`
}

// FanOutSource answers the fan-out question. The controller adapts its
// catalog to it.
//
// nil is a legitimate state: a node that evaluates no siblings' acquire ops
// mounts the route and answers 503, like every other optional capability here.
type FanOutSource interface {
	// FanOutFrom lists this node's outcomes for the acquire ops signed by
	// signer (a peer public key as an op's `by` carries it), narrowed to one
	// op when opHash is not empty, at most limit of them.
	FanOutFrom(ctx context.Context, signer, opHash string, limit int) (FanOutAnswer, error)
}

// handleFanOut answers a sibling asking what this node did with the acquire
// ops it signed. The signer is the CERTIFICATE's key, never a parameter, so a
// member sees its own ops' outcomes and nobody else's.
func (s *Server) handleFanOut(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFrom(r.Context())
	if !ok {
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if s.fanOut == nil {
		httpapi.Fail(w, r, problem.New(http.StatusServiceUnavailable, problem.TypeInternal,
			"Service Unavailable", "this node evaluates no siblings' acquire ops (ADR-0119)"))
		return
	}
	op := strings.TrimSpace(r.URL.Query().Get("op"))
	if len(op) > maxFanOutOpParam {
		httpapi.Fail(w, r, problem.BadRequest("`op` is not an op hash"))
		return
	}

	out, err := s.fanOut.FanOutFrom(r.Context(), principal.PublicKey(), op, MaxFanOutApplications)
	if err != nil {
		s.log.Error("answering a fan-out ask failed",
			"request_id", httpapi.RequestIDFrom(r.Context()),
			"peer_id", principal.PeerID(), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if out.Applications == nil {
		out.Applications = []FanOutApplication{}
	}
	s.writeJSON(w, r, out)
}
