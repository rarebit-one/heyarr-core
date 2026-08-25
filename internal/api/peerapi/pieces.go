package peerapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
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

	// ReadPiece returns the bytes of one piece, from a blob this node holds
	// whole or in part.
	//
	// # It must refuse a piece it has not recorded as landed
	//
	// This is the one place in the design where believing the availability
	// record wrongly sends bad bytes to SOMEBODY ELSE rather than failing
	// locally. Everywhere else a wrong bitset costs a wasted request and is
	// caught by the destination's own verification; here it would put
	// half-written or absent bytes on the wire under the name of a piece.
	//
	// ErrNoSuchPiece is the refusal, and it is the correct answer rather than
	// an error condition: a peer asking for a piece this node does not have is
	// ordinary, and it happens constantly while two peers converge.
	ReadPiece(ctx context.Context, blob hashing.Hash, index int) ([]byte, error)
}

// ErrNoSuchPiece is a piece this node does not hold.
//
// Ordinary rather than exceptional: two peers converging ask each other for
// pieces constantly, and a "not yet" is most of the conversation.
var ErrNoSuchPiece = errors.New("peerapi: this node does not hold that piece")

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

// PiecePath is the route for one piece of a blob.
func PiecePath(hash string, index int) string {
	return Prefix + "/blobs/" + strings.TrimSpace(hash) + "/pieces/" + strconv.Itoa(index)
}

// handleBlobPiece answers GET /peer/v1/blobs/{hash}/pieces/{index}.
//
// # Why a piece route rather than a Range on the content route
//
// ADR-0042 said fetching a piece would be a ranged GET against the existing
// content route, and for a blob held WHOLE that is exactly right — that route
// serves ranges, and §27's web seed needs nothing more.
//
// It does not work for a blob held in PART, and that is the case §23 is about.
// The content route promises the blob: it answers with a strong ETag naming the
// whole-object digest, a length that is the blob's length, and a 404 meaning
// "this peer does not have it". A node holding a third of the bytes can honour
// none of those, and making it try would mean either lying about the ETag or
// inventing a partial-content semantics on a route whose contract is
// deliberately simple (ADR-0013).
//
// So the piece route is new, and it is the ONE way the transport fetches a
// piece — from a whole blob or a partial one alike. The content route is left
// exactly as it is, for whole-blob pulls, which is what replication already
// does.
func (s *Server) handleBlobPiece(w http.ResponseWriter, r *http.Request) {
	peer, ok := PeerFrom(r.Context())
	if !ok {
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if s.pieces == nil {
		httpapi.Fail(w, r, problem.New(http.StatusServiceUnavailable, problem.TypeInternal,
			"Service Unavailable", "this node is not serving blob content on the peer fabric: it has "+
				"no content store behind its peer surface, so it holds no pieces to serve"))
		return
	}

	blob, err := hashing.Parse(chi.URLParam(r, "hash"))
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(
			"that is not a blob identifier: it must be blake3: followed by 64 hex characters"))
		return
	}
	index, err := strconv.Atoi(chi.URLParam(r, "index"))
	if err != nil || index < 0 {
		httpapi.Fail(w, r, problem.BadRequest("the piece index must be a non-negative integer"))
		return
	}

	body, err := s.pieces.ReadPiece(r.Context(), blob, index)
	switch {
	case errors.Is(err, ErrNoSuchPiece):
		// The ordinary answer while two peers converge, not a fault. It is a
		// 404 rather than a 409 because from the caller's side it is exactly
		// "not here": try another peer, or ask again later.
		httpapi.Fail(w, r, problem.NotFound(
			"this node does not hold that piece of that blob"))
		return
	case err != nil:
		s.log.Error("could not serve a piece",
			"blob", blob.String(), "piece", index, "peer_id", peer.PeerID, "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}

	// No ETag. A piece is not addressable and never will be (ADR-0041), so a
	// validator naming one would be a cache key for a thing with a session's
	// lifetime — and a peer that cached it would serve a piece of a geometry
	// that no longer applies.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	// gosec reads "bytes from a store, written to a response" as a possible XSS
	// sink. It is the same bytes, to the same peers, under the same headers as
	// /blobs/{hash}/content already serves — that route escapes the check only
	// because it goes through http.ServeContent. Opaque blob content, declared
	// application/octet-stream with nosniff, to an mTLS-pinned member: there is
	// no HTML context for it to be script in.
	if _, err := w.Write(body); err != nil { //nolint:gosec // opaque blob bytes as octet-stream+nosniff, as the content route serves
		// The response is already committed, so this is a report rather than a
		// recovery. The destination will notice: it verifies every piece
		// against that piece's hash before counting it.
		s.log.Debug("a piece write was cut short",
			"blob", blob.String(), "piece", index, "peer_id", peer.PeerID, "error", err)
		return
	}

	// What left here, and to whom — the source-side accounting M5 added for
	// the content route, extended to pieces for the same reason: everything
	// else in the fabric is written from the reader's side.
	s.log.Info("served a piece to a peer",
		"blob", blob.String(), "piece", index, "bytes", len(body), "peer_id", peer.PeerID)
}
