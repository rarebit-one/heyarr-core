package peerapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/peer/inventory"
)

// InventorySink folds a peer's inventory report into the controller's
// `replicas` table.
//
// It is an interface here so this package does not import persistence. The
// peer surface's job is to establish WHO is reporting; deciding what a report
// means for the catalog is the control plane's, and a peer is not allowed to
// write control-plane rows directly (ADR-0029) — it hands the controller a
// report and the controller's single writer records it.
//
// The peerID argument is the acting peer, derived from the client certificate.
// It is passed separately from the report rather than read out of it, which is
// the whole of ADR-0033's third rule expressed as a function signature: an
// implementation of this interface CANNOT accidentally trust the body, because
// the body's declaration is not what it is given.
type InventorySink interface {
	ReconcileInventory(ctx context.Context, peerID string, report inventory.Report) (inventory.Outcome, error)
}

// maxInventoryBody bounds a report.
//
// Larger than maxAttachBody by four orders of magnitude, because a full report
// legitimately carries one entry per blob and a Full Peer's library is the
// whole canonical set (§19). At roughly 120 bytes an entry, 16 MiB is about
// 140,000 blobs — comfortably past any homelab library, and still a bound. An
// authenticated caller streaming an unbounded body into a JSON decoder is the
// cheapest denial of service in any fabric, and "it is a member" is not a
// reason to skip the limit.
//
// A peer with more blobs than this reports incrementally, which is the
// mechanism this issue delivers and the reason the limit is not a wall.
const maxInventoryBody = 16 << 20

// handleInventory answers POST /peer/v1/inventory.
//
// The peer reports what is on ITS disk. The controller records it against the
// peer the CERTIFICATE proved — never against the peer the body declares. The
// declaration is compared and then discarded, exactly as in handleAttach.
func (s *Server) handleInventory(w http.ResponseWriter, r *http.Request) {
	principal, ok := PrincipalFrom(r.Context())
	if !ok {
		// The identity middleware is the only path here, so this is a wiring
		// failure rather than a request failure.
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if s.inventory == nil {
		// A node with no catalog behind it cannot record a report, and
		// answering 200 would tell the peer its inventory landed when nothing
		// happened at all — which is the exact failure mode this endpoint
		// exists to close.
		httpapi.Fail(w, r, problem.New(http.StatusServiceUnavailable, problem.TypeInternal,
			"Service Unavailable", "this node is not accepting inventory reports: it has no catalog "+
				"to record them in. A Full Peer reports to the controller (ADR-0029)"))
		return
	}

	var report inventory.Report
	dec := json.NewDecoder(io.LimitReader(r.Body, maxInventoryBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(
			"the inventory body must be a JSON inventory report: "+err.Error()))
		return
	}
	if report.PeerID == "" {
		// Not defaulted to the certificate's peer id, for the reason
		// handleAttach gives: a body that may be omitted is a body that stops
		// being compared, and the comparison is the only reason it exists.
		httpapi.Fail(w, r, problem.BadRequest(
			"peer_id is required: it is the peer this node believes it is, and the controller "+
				"compares it against the identity the certificate proved (ADR-0033)"))
		return
	}

	// The one line this endpoint turns on, and the one the sabotage patch
	// targets. The acting peer is principal.PeerID() — taken from the
	// certificate — and the declaration in the body is only ever an argument
	// to Authorises. A surface that read the acting identity out of the body
	// would authenticate every peer perfectly and then let any of them
	// overwrite any other's inventory.
	if err := principal.Authorises(report.PeerID); err != nil {
		s.log.Warn("refused an inventory report filed under another peer",
			"request_id", httpapi.RequestIDFrom(r.Context()),
			"acting_peer_id", principal.PeerID(),
			"declared_peer_id", report.PeerID)
		httpapi.Fail(w, r, problem.Forbidden(
			"this connection authenticated as peer "+principal.PeerID()+", and the report declares "+
				"peer "+report.PeerID+". A peer reports its own inventory and only its own: the "+
				"acting peer is taken from the certificate and never from the request body (ADR-0033)"))
		return
	}

	if err := report.Validate(); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	}

	outcome, err := s.inventory.ReconcileInventory(r.Context(), principal.PeerID(), report)
	switch {
	case errors.Is(err, inventory.ErrUnknownPeer):
		// Authenticated and pinned, but with no row in the catalog. That is a
		// membership record and a catalog that disagree, which is an operator
		// problem rather than this peer's, and saying so is more useful than
		// a 500.
		httpapi.Fail(w, r, problem.Forbidden(
			"peer "+principal.PeerID()+" is a member of this fabric but has no catalog row, so "+
				"its replicas cannot be recorded"))
		return
	case errors.Is(err, inventory.ErrInvalidReport):
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return
	case err != nil:
		s.log.Error("recording an inventory report failed",
			"request_id", httpapi.RequestIDFrom(r.Context()),
			"peer_id", principal.PeerID(), "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}

	s.log.Info("a peer reported its inventory",
		"peer_id", principal.PeerID(), "peer_name", principal.Name(),
		"mode", outcome.Mode, "entries", outcome.Entries,
		"added", outcome.Added, "changed", outcome.Changed,
		"removed", outcome.Removed, "unknown", outcome.Unknown)
	s.writeJSON(w, r, outcome)
}
