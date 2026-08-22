package resources

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/domain/playback"
	"github.com/rarebit-one/heyarr-core/internal/domain/routing"
	"github.com/rarebit-one/heyarr-core/internal/media/probe"
	"github.com/rarebit-one/heyarr-core/internal/peer/health"
)

// Read routing, wired (§31, §32, M4-14).
//
// # Two decisions, in order
//
// §32 asks WHERE the bytes come from and §68 asks WHAT the client should do
// with them. They were one function while there was one peer (ADR-0010),
// because with one peer the first question has one answer. With two they
// separate: routing picks the source, and the planner then decides direct,
// remux or transcode over the source that was picked. Fusing them again would
// mean the planner's codec logic and the fabric's health logic changing
// together, and they have nothing to do with each other.
//
// # Why the health verdict is filtered through health.Sources
//
// M4-10 decided what "healthy enough to read from" means, and it decided it
// asymmetrically: reads filter, writes do not (see health.Destinations, whose
// comment is the load-bearing one). Re-deriving the read half here — `health =
// 'reachable'` in the SQL, say — would be a second definition of the same
// thing, and the two would disagree the first time either moved. So the query
// reads the stored column, and health.Sources is what decides.
//
// # This node is reachable by inspection
//
// The self peer's stored health is written by the reconciliation beat's sweep,
// which may not have run yet on a node that has just started — and 'unknown'
// is deliberately not a synonym for reachable. But the process answering this
// very request IS the evidence for the one peer we are certain about, which is
// exactly why health.Sweep marks the self row Answered rather than probing its
// own loopback. Routing applies the same reasoning to the same row, so that a
// node one second after boot can still play its own library.

// candidatesFor assembles every peer the controller could route a read of this
// blob to, in a stable order.
//
// One query, LEFT JOINed on the replica, so that a peer holding NO row appears
// as a candidate with an empty replica state rather than vanishing. That is the
// difference between "peer-b was rejected because it does not have the bytes"
// and a refusal that never mentions peer-b at all — and the second is the
// three-hour outage the reasons exist to prevent.
func (a *API) candidatesFor(r *http.Request, blobHash string) ([]routing.Candidate, error) {
	rows, err := a.reader.QueryContext(r.Context(), `
		SELECT p.id, p.name, p.site, COALESCE(p.endpoint, ''), p.is_self, p.health,
		       COALESCE(r.state, ''),
		       (SELECT site FROM peers WHERE is_self = 1)
		FROM peers p
		LEFT JOIN replicas r ON r.peer_id = p.id AND r.blob_hash = ?
		ORDER BY p.is_self DESC, p.name ASC, p.id ASC`, blobHash)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	type row struct {
		candidate routing.Candidate
		health    health.Peer
	}
	var (
		scanned  []row
		peers    []health.Peer
		selfSite sql.NullString
	)
	for rows.Next() {
		var (
			c        routing.Candidate
			isSelf   int
			hs       string
			endpoint string
		)
		if err := rows.Scan(&c.PeerID, &c.Name, &c.Site, &endpoint, &isSelf, &hs,
			&c.ReplicaState, &selfSite); err != nil {
			return nil, err
		}
		c.Endpoint = endpoint
		c.IsSelf = isSelf == 1
		c.HealthState = hs
		// "The client's site" is the site this controller serves. A device has
		// no site of its own in the schema, and inventing one would be a
		// client-supplied claim about where it is — which is a routing input
		// no client should get to assert. A Full Peer is controller-attached
		// (ADR-0029), so the controller's site IS the site of the client
		// talking to it.
		c.SameSite = selfSite.Valid && c.Site == selfSite.String

		hp := health.Peer{PeerID: c.PeerID, Name: c.Name, IsSelf: c.IsSelf, State: health.State(hs)}
		if hp.IsSelf {
			// See the file comment: the process answering this request is the
			// evidence, and it is the same reasoning health.Sweep applies to
			// this row.
			hp.State = health.StateReachable
			c.HealthState = string(health.StateReachable)
		}
		scanned = append(scanned, row{candidate: c, health: hp})
		peers = append(peers, hp)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	reachable := map[string]bool{}
	for _, p := range health.Sources(peers) {
		reachable[p.PeerID] = true
	}
	out := make([]routing.Candidate, 0, len(scanned))
	for _, sc := range scanned {
		sc.candidate.Reachable = reachable[sc.candidate.PeerID]
		out = append(out, sc.candidate)
	}
	return out, nil
}

// routeBlob selects a source for a blob, or refuses with every reason.
//
// A blob-less asset (linked, ADR-0020) routes to nothing and is not a database
// error: there is nothing to hold a replica of, and the planner's no_replica
// answer is the honest one.
func (a *API) routeBlob(r *http.Request, blobHash string) (routing.Decision, error) {
	if blobHash == "" {
		return routing.Decision{}, nil
	}
	candidates, err := a.candidatesFor(r, blobHash)
	if err != nil {
		return routing.Decision{}, err
	}
	return routing.Select(candidates), nil
}

// replicasOf hands the planner exactly the source routing chose.
//
// The planner still takes a list and still prefers a local replica over a
// remote one — that logic is unit-tested and is what a caller with no routing
// information (an offline planner, a future degraded peer under §53) needs.
// Here the list is one element long, because §32 has already answered the
// question §68 would otherwise have to guess at.
func replicasOf(decision routing.Decision) []playback.Replica {
	if !decision.Found {
		return nil
	}
	return []playback.Replica{{
		PeerID: decision.Source.PeerID,
		Local:  decision.Source.SameSite,
	}}
}

// contentURLFor is §32's scoped direct URL: it points at the SELECTED PEER,
// and never at the controller.
//
// # The controller is not in the data path
//
// A controller that proxies is a controller whose availability becomes
// playback's availability, which is the coupling §53's degraded-operation model
// exists to avoid and the decoupling ADR-0030 names as the reason the
// controller never carries bytes. So a cross-site selection returns an ABSOLUTE
// URL at the chosen peer's endpoint, and the client connects to that peer
// directly.
//
// # A relative URL for this node is not an exception to that
//
// When the selected source is this node, the peer the client should talk to is
// the origin it is already talking to (ADR-0029: a Full Peer is
// controller-attached). A relative URL says exactly that, and it says it
// without handing the client an address out of the peers table which may be a
// container-internal one the client cannot resolve. The peer with no endpoint
// that is NOT this node is rejected before it can reach here, precisely so that
// a relative URL can never silently mean "the controller".
//
// # What is deferred, recorded rather than discovered later
//
// The URL is direct and bounded — the credential accompanying it expires (see
// playbackTokenTTL) — and it is NOT scoped to one blob. Milestone 1 already
// flagged this at playback.go's file comment: scoping is a GRANT (§77), and
// §54's lease shape — principal, resource, capabilities, expiry — is the real
// answer, arriving in M7 alongside degraded authorisation. Inventing a lesser
// per-blob scope here would be a second authorisation model to reconcile with
// the real one later.
//
// REVISIT WHEN: the first peer class arrives whose access is a strict subset of
// the canonical set (§6, §19). That is the same trigger ADR-0030 records for
// per-blob capabilities in replication, and it is the same question — until
// then, denying a member individual blobs is vacuous, because a Full Peer's
// desired set is everything.
func contentURLFor(decision routing.Decision, blobHash string) string {
	if !decision.Found || blobHash == "" {
		return ""
	}
	if decision.Source.IsSelf {
		return probe.BlobURL("", blobHash)
	}
	return probe.BlobURL(strings.TrimSuffix(decision.Source.Endpoint, "/"), blobHash)
}
