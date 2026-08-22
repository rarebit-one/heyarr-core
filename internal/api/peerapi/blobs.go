package peerapi

import (
	"net/http"
	"strings"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
)

// Byte serving on the peer fabric (§21, §28, ADR-0013, ADR-0030, M4-09).
//
// # The same handler as the client API's, on a different trust root
//
// ADR-0013 said this in advance: the blob endpoint is "deliberately the same
// endpoint that Milestone 4 replication reads from". [BlobServer] is satisfied
// by *blobs.Handler, so the ranges, the validators, the immutable caching and
// the flat memory use are one implementation rather than two — a replication
// copy would drift from the playback one and would drift silently, because
// both would keep passing their own tests.
//
// What is not shared is the credential, and that is why the route is mounted
// here rather than reached through the client API. The client API authenticates
// a bearer token (ADR-0011); a peer holds no token and must not be issued one,
// and the peer surface authenticates a pinned key in a client certificate
// (ADR-0012). Two listeners, two trust roots, one contract.
//
// # The source makes no decision
//
// ADR-0030: the source "serves an ordinary blob read and makes no decision
// about what the destination needs". There is nothing replication-shaped here —
// no manifest, no "what am I missing" negotiation, no per-blob capability. A
// pinned member may read any blob this deployment holds, because a Full Peer's
// desired blob set is the complete canonical set (§19) and denying it
// individual blobs would be vacuous.

// BlobServer serves blob content over HTTP. *blobs.Handler is the
// implementation; it is an interface here so that this package does not import
// the CAS, which the client API's blob handler already owns.
type BlobServer interface {
	Content(w http.ResponseWriter, r *http.Request)
}

// handleBlobContent answers GET|HEAD /peer/v1/blobs/{hash}/content.
//
// A node with no content store behind its peer surface answers 503 rather than
// 404, exactly as the inventory route does. The distinction is the one a
// puller acts on: 404 means "that is a hash and this peer does not have it", so
// try another source, and 503 means this peer is not serving bytes at all, so
// there is nothing here to try again for. Collapsing them would have a
// destination hunting for a blob on a node that has no store.
func (s *Server) handleBlobContent(w http.ResponseWriter, r *http.Request) {
	if _, ok := PeerFrom(r.Context()); !ok {
		// The identity middleware is the only path here, so this is a wiring
		// failure rather than a request failure.
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if s.blobs == nil {
		httpapi.Fail(w, r, problem.New(http.StatusServiceUnavailable, problem.TypeInternal,
			"Service Unavailable", "this node is not serving blob content on the peer fabric: it has "+
				"no content store behind its peer surface"))
		return
	}
	s.blobs.Content(w, r)
}

// BlobContentPath is where a peer reads another peer's bytes.
//
// A constant rather than a string built at each end, for the reason
// inventory.Path is one: the two ends of this exchange are in different
// packages and one day in different processes, and a path spelled twice is a
// path that can be spelled differently.
//
// The hash is placed verbatim. It is validated by the caller before it gets
// here — a destination pulls a digest it read out of its own catalog, not a
// string a peer handed it — and by the handler at the far end before it reaches
// a store that turns identifiers into paths.
func BlobContentPath(hash string) string {
	return Prefix + "/blobs/" + strings.TrimSpace(hash) + "/content"
}
