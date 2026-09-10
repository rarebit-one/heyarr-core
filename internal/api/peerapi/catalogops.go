package peerapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/catalogtomb"
)

// maxCatalogOpsBody bounds a pushed op batch. Catalog ops are small signed
// tokens, and a G-set converges however it is chunked, so a peer with a large
// backlog pushes it in batches rather than one body — the same reasoning as the
// inventory limit, and the same reason it is not a wall.
const maxCatalogOpsBody = 8 << 20

// CatalogOpSource is the editorial catalog op log a sibling reads and pushes to,
// so the two sites converge on which works are logically deleted (ADR-0073,
// #449). It is the same G-set both sides hold; catalogtomb.Store satisfies it.
//
// nil is a legitimate state, not a wiring bug: a node not configured for
// two-site catalog convergence mounts the routes but answers 503, exactly like
// every other optional capability on this surface.
type CatalogOpSource interface {
	// Ops returns every catalog op this node holds — the state a sibling merges.
	Ops(ctx context.Context) ([]string, error)
	// RecordOps merges pushed ops into the log and re-materialises the
	// tombstones. A token that does not verify is catalogtomb.ErrMalformedOp.
	RecordOps(ctx context.Context, ops []string) error
}

// catalogOpsBody is both the GET response and the POST request/response body:
// the op tokens, opaque here. A peer verifies each against the ISSUER's pinned
// key (ADR-0012), never against the peer that served or relayed them — a delete
// op is a statement signed by the deleting peer and stays its statement however
// it travelled, the caution handleLeases states for lease tokens.
type catalogOpsBody struct {
	Ops []string `json:"ops"`
}

// handleCatalogOps serves this node's catalog op log to an authenticated
// sibling, which merges it into its own — the pull half of convergence. Behind
// requirePeerIdentity, so only a member reaches it; the ops carry their own
// signatures regardless.
func (s *Server) handleCatalogOps(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFrom(r.Context())
	if !ok {
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if s.catalogOps == nil {
		httpapi.Fail(w, r, problem.New(http.StatusServiceUnavailable, problem.TypeInternal,
			"Service Unavailable", "this node does not converge a catalog with a sibling; "+
				"catalog ops flow between the two peers of a two-site pair (§49, ADR-0073)"))
		return
	}

	ops, err := s.catalogOps.Ops(r.Context())
	if err != nil {
		s.log.Error("serving catalog ops failed",
			"request_id", httpapi.RequestIDFrom(r.Context()),
			"peer_id", principal.PeerID(), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if ops == nil {
		ops = []string{}
	}
	s.log.Info("served catalog ops", "peer_id", principal.PeerID(), "count", len(ops))
	s.writeJSON(w, r, catalogOpsBody{Ops: ops})
}

// handleCatalogOpsPush records the ops a sibling pushes and answers with this
// node's full log, so one round trip converges both directions: the pusher's
// ops land here, and the response carries anything the pusher was missing. A
// G-set makes this safe to repeat — recording is idempotent by op hash.
func (s *Server) handleCatalogOpsPush(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFrom(r.Context())
	if !ok {
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if s.catalogOps == nil {
		httpapi.Fail(w, r, problem.New(http.StatusServiceUnavailable, problem.TypeInternal,
			"Service Unavailable", "this node does not converge a catalog with a sibling; "+
				"catalog ops flow between the two peers of a two-site pair (§49, ADR-0073)"))
		return
	}

	var body catalogOpsBody
	dec := json.NewDecoder(io.LimitReader(r.Body, maxCatalogOpsBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(
			"the body must be a JSON object with an `ops` array of catalog op tokens: "+err.Error()))
		return
	}

	if err := s.catalogOps.RecordOps(r.Context(), body.Ops); err != nil {
		if errors.Is(err, catalogtomb.ErrMalformedOp) {
			// A signature that does not verify is the pusher's error, not this
			// node's — name it so the operator on the other side can act.
			httpapi.Fail(w, r, problem.BadRequest(
				"a pushed catalog op did not verify: "+err.Error()))
			return
		}
		s.log.Error("recording pushed catalog ops failed",
			"request_id", httpapi.RequestIDFrom(r.Context()),
			"peer_id", principal.PeerID(), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}

	ops, err := s.catalogOps.Ops(r.Context())
	if err != nil {
		s.log.Error("reading catalog ops after a push failed",
			"request_id", httpapi.RequestIDFrom(r.Context()),
			"peer_id", principal.PeerID(), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if ops == nil {
		ops = []string{}
	}
	s.log.Info("recorded pushed catalog ops",
		"peer_id", principal.PeerID(), "pushed", len(body.Ops), "held", len(ops))
	s.writeJSON(w, r, catalogOpsBody{Ops: ops})
}
