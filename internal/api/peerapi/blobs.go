package peerapi

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

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
	peer, ok := PeerFrom(r.Context())
	if !ok {
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
	rec := httpapi.Record(w)
	s.blobs.Content(rec, r)

	// Who read which bytes off this node, and over which surface.
	//
	// A Full Peer had no record of this at all. Everything else in the fabric
	// is written from the READER's side — the destination logs what it pulled,
	// the catalog records the replica it claimed — so a source being drained by
	// a peer, or serving a blob it did not expect to be asked for, was visible
	// nowhere on the machine actually sending the bytes. For a system whose
	// premise is that beliefs and bytes diverge, "what left here" is not an
	// optional line.
	//
	// It is also the only thing that tells the two surfaces apart from outside.
	// The client API's blob route and this one share a handler by design
	// (ADR-0013), and its metrics label them identically, so a read that
	// arrived on a bearer token and a read that arrived on a pinned certificate
	// are indistinguishable in every other record this node keeps — which makes
	// "the controller carried no bytes" unmeasurable rather than merely
	// unmeasured.
	//
	// GET is logged at info and HEAD at debug, because the volume should track
	// the bytes. A GET is a transfer and there is one per blob replicated; a
	// HEAD is the durability precondition asking whether a blob is here
	// (ADR-0018), which happens once per candidate blob per sweep and carries
	// no body at all.
	level := slog.LevelInfo
	if r.Method == http.MethodHead {
		level = slog.LevelDebug
	}
	// HOW MANY BYTES, which is the half of "what left here" that was missing.
	//
	// Since Milestone 5 a replication read is not necessarily the whole blob:
	// a destination holding a verified partial, or holding another blob that
	// shares chunks with this one, asks for RANGES and asks for several. So
	// "site-b read this blob" no longer implies "this blob crossed the wire",
	// and a source that cannot say how much it sent cannot answer the only
	// question the milestone is about. `ranged` is recorded beside it because
	// the two together are what distinguish a cheap transfer from a failed one
	// — both move few bytes.
	//
	// Counted on the SOURCE, which is the point: a destination's account of
	// what it fetched is a claim about itself, and a transfer that fetched
	// nothing and published the wrong file would report a very good number.
	s.log.Log(r.Context(), level, "served blob content to a peer",
		"blob_hash", chi.URLParam(r, "hash"), "method", r.Method,
		"bytes", rec.Written(), "status", rec.Status(),
		"ranged", r.Header.Get("Range") != "",
		"peer_id", peer.PeerID, "peer_name", peer.Name)
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
