package peerapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// The piece availability route (§23, §26, ADR-0042, ADR-0043).
//
// # The question replication could never ask
//
// Every existing route answers about a blob a peer holds WHOLE: the inventory
// lists them, the content route serves them, the manifest describes them. A
// peer that holds part of a blob has, until now, been indistinguishable from a
// peer that holds none of it — so two peers both still fetching the same blob
// had nothing to say to each other, and the only shape available was
// `Internet → A`, then `A → B`.
//
// This route is the third answer: "I have some of it, and here is which."
// Everything else in M6's cooperative path is built on being able to ask it.
//
// # It is a READ, and can only be a read
//
// The same rule the manifest route states, for the same reason: nothing
// reachable through here may generate anything. A GET that computed a
// blob's pieces by reading 20 GB to answer would be a remote denial of
// service, and the geometry is derivable from the size without touching the
// bytes at all — so there is nothing to compute even if somebody wanted to.
//
// # The answer is a CLAIM
//
// A peer saying it holds a piece is a peer saying so. The destination verifies
// every piece it receives against that piece's hash, and the whole object
// against its BLAKE3 digest before it becomes a blob (invariant 1). A peer that
// overstates wastes a request and is walked past, exactly as ADR-0036 has
// repair treat a source that serves bytes it cannot back.

// PieceSource answers what a node holds of a blob, whole or in part.
//
// An interface here for the same reason ManifestSource is one: this package
// does not import the CAS, and the fabric does not reach up into the API.
type PieceSource interface {
	// PieceAvailability returns the encoded availability of a blob and whether
	// this node knows anything about it at all.
	//
	// The encoding is opaque to this package — ADR-0041 keeps a piece a
	// transport detail, and a peer surface that could parse one would be a
	// surface that had learned what a piece is. It is carried, not read.
	//
	// found=false means this node has neither the blob nor a transfer of it in
	// progress, which is an ordinary answer and not an error.
	PieceAvailability(ctx context.Context, blob hashing.Hash) (encoded string, found bool, err error)
}

// PieceAvailabilityResponse is what GET /peer/v1/blobs/{hash}/pieces answers.
type PieceAvailabilityResponse struct {
	// BlobHash is the blob asked about, echoed so a response cannot be
	// mistaken for one about a different blob if it is cached or logged.
	BlobHash string `json:"blob_hash"`
	// Available is the encoded availability, carrying its own piece count so
	// that a peer computing a different geometry is refused rather than
	// half-believed (ADR-0043).
	//
	// Empty means this node holds none of it. That is distinct from a 404,
	// which means it knows nothing about the blob at all — the difference
	// between "I am fetching this and have nothing yet" and "never heard of
	// it", which are different things to a session choosing sources.
	Available string `json:"available"`
}

// PieceAvailabilityPath is the route for a blob's availability.
func PieceAvailabilityPath(hash string) string {
	return Prefix + "/blobs/" + strings.TrimSpace(hash) + "/pieces"
}

// handleBlobPieces answers GET /peer/v1/blobs/{hash}/pieces.
func (s *Server) handleBlobPieces(w http.ResponseWriter, r *http.Request) {
	peer, ok := PeerFrom(r.Context())
	if !ok {
		// The identity middleware is the only path here, so this is a wiring
		// failure rather than a request failure — and the assertion that
		// notices a route mounted outside the chain, which is the mistake a
		// new route on an existing listener invites.
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if s.pieces == nil {
		// The same 503 the content and manifest routes give, and for the same
		// reason: this node is not serving content on the peer fabric at all,
		// so there is nothing here to try again for. A 404 would have a
		// destination conclude "no pieces here, try elsewhere for the rest"
		// and keep coming back for the bytes.
		httpapi.Fail(w, r, problem.New(http.StatusServiceUnavailable, problem.TypeInternal,
			"Service Unavailable", "this node is not serving blob content on the peer fabric: it has "+
				"no content store behind its peer surface, so it holds no pieces to report"))
		return
	}

	raw := chi.URLParam(r, "hash")
	blob, err := hashing.Parse(raw)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(
			"that is not a blob identifier: it must be blake3: followed by 64 hex characters"))
		return
	}

	encoded, found, err := s.pieces.PieceAvailability(r.Context(), blob)
	if err != nil {
		s.log.Error("could not report piece availability",
			"blob", blob.String(), "peer_id", peer.PeerID, "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if !found {
		httpapi.Fail(w, r, problem.NotFound(
			"this node holds no part of that blob and has no transfer of it in progress"))
		return
	}

	// What was asked for and by whom, at debug: availability is polled while a
	// transfer converges, so at info it would be a heartbeat — and an event
	// stream that is mostly noise is one nobody follows.
	s.log.Debug("reported piece availability to a peer",
		"blob", blob.String(), "peer_id", peer.PeerID, "available", encoded)

	s.writeJSON(w, r, PieceAvailabilityResponse{
		BlobHash: blob.String(), Available: encoded,
	})
}
