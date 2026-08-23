package peerapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/chunking"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// Chunk manifests on the peer fabric (§20, §21, ADR-0030, ADR-0034, M5-05).
//
// # A description, not a negotiation
//
// blobs.go says of the content route that there is "no manifest, no 'what am I
// missing' negotiation" on this surface. This file adds the manifest and does
// not add the negotiation, and the difference between those two sentences is
// the whole design.
//
// The destination fetches a DESCRIPTION of what the source stored and decides
// for itself what to do about it. This route is never told what the
// destination holds, is never asked what it should send, and computes no
// difference. There is no request body, no query parameter carrying an
// inventory, and no response field that varies with the caller — two different
// peers asking about one blob get byte-identical answers. Anything else would
// make the source a participant in a decision it cannot verify, and would put
// the destination's inventory on the wire for no reason.
//
// # Asking never generates
//
// 🔴 The rule this file is arranged around. A GET that chunked a blob to
// answer would be a remote denial of service with a polite name: one request
// naming a 20 GB blob, and the source reads 20 GB off its disks. So the route
// reads [ManifestSource], which is a read, and 404 is a final answer rather
// than a condition to resolve. §16's third state — undecided — is the ANSWER.
// A destination that gets a 404 pulls the blob whole, which is exactly M4's
// behaviour and is always correct (ADR-0034: a manifest "may be discarded at
// any time with no loss of correctness").
//
// Nothing here enqueues a chunk_blob job either. Deciding to chunk is a
// separate call by a caller that wanted to, never a side effect of somebody
// asking.
//
// # The same trust root as the bytes, and no new credential
//
// Mounted beside the content route, inside the same requirePeerIdentity chain,
// on the same mTLS listener (ADR-0012). No per-blob capability and no second
// credential: a pinned member may already read the bytes this manifest
// describes, so refusing it the description would be theatre — the M4-02
// argument, unchanged.
//
// # Three answers, kept apart
//
// The content route keeps 404 and 503 distinct because a puller acts on the
// difference. This route has three:
//
//   - 404 [problem.TypeNoChunkManifest] — this node HOLDS the blob and has no
//     manifest for it. The destination pulls whole, from this same source.
//   - 404 [problem.TypeNotFound] — this node does not hold the blob at all.
//     The destination tries another source; pulling whole from here would
//     404 as well.
//   - 503 — this node is not serving content on the peer fabric at all, so
//     there is nothing here to try again for.
//
// They are distinguished by the `type` URI rather than by prose, because the
// prose is not the contract (see package problem) and a destination that
// branched on a message would break the day the message improved.

// ErrNoSuchBlob is what a [ManifestSource] returns when this node holds no
// blob with that digest at all.
//
// Distinct from "no manifest" and it has to be, because the destination takes
// a different action on each: no manifest means pull these bytes whole from
// this source, and no such blob means this source cannot help at all.
var ErrNoSuchBlob = errors.New("peerapi: this node holds no blob with that digest")

// ManifestSource reads a blob's stored chunk manifest. It is an interface here
// so this package does not import persistence, for the reason [InventorySink]
// is one.
//
// # It is a READ, and the signature is what says so
//
// There is no `generate` argument, no options struct that could grow one, and
// no error that means "not yet, ask again". An implementation returns what is
// stored and the §16 state, and an implementation that chunked a blob to
// satisfy a call would be a bug in the implementation rather than a
// misconfiguration of this route (ADR-0034).
//
// The returned State is the honest three-way answer, so the handler can log
// WHY it has nothing — a source being asked repeatedly for manifests of blobs
// nobody ever decided to chunk is a real diagnostic signal, and one that
// "found=false" throws away.
type ManifestSource interface {
	// ChunkManifest returns the stored manifest for a blob and the state of
	// §16's question. The manifest is meaningful only when the state is
	// [manifests.StatePresent]. [ErrNoSuchBlob] means this node holds no such
	// blob.
	ChunkManifest(ctx context.Context, blob hashing.Hash) (manifests.Manifest, manifests.State, error)
}

// ManifestChunk is one chunk on the wire.
//
// Offset is carried as well as Length even though it is derivable, because the
// destination validates the sequence it received rather than reconstructing
// it: a manifest with a gap must be refusable, and a receiver that recomputes
// offsets from lengths cannot see a gap because it has just filled it in.
type ManifestChunk struct {
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	Digest string `json:"digest"`
}

// ManifestParams are the chunker settings the manifest was produced under.
//
// On the wire rather than assumed, because a manifest computed under other
// parameters describes the same bytes and shares none of the same boundaries.
// A destination whose defaults have moved has to be able to tell that what it
// was handed is not comparable with what it would compute.
type ManifestParams struct {
	Min int `json:"min"`
	Avg int `json:"avg"`
	Max int `json:"max"`
}

// ManifestResponse is what GET /peer/v1/blobs/{hash}/manifest answers.
//
// # Digest names the MANIFEST
//
// ADR-0034 is explicit: a manifest "may itself be content-addressed, for its
// own integrity ... That digest names the manifest. It is not an alias for the
// blob, it does not appear in `blobs`, and nothing may resolve it to bytes."
// It is here so the destination can check it received the manifest the source
// has; it is never a handle for the bytes, and [BlobManifestPath] takes BlobHash.
//
// # Chunks is a SEQUENCE
//
// The order is the data. A set of individually valid chunks assembled in the
// wrong order is a set of valid chunks and the wrong file, and it is the only
// fault the per-chunk digests cannot see. The digest above covers the chunk
// digests in index order for exactly that reason, so a source that shuffled
// this array produces a response the destination rejects rather than one it
// reassembles wrongly.
//
// # Nothing here varies with the caller
//
// ServedBy names the answering node, as [IdentityResponse.ServedBy] does, so a
// destination can tell which end of the fabric replied. It is the only field
// that is about anything other than the manifest, and it is about the SOURCE.
// There is no field describing the caller, because the source formed no
// opinion about the caller.
type ManifestResponse struct {
	BlobHash    string          `json:"blob_hash"`
	Digest      string          `json:"digest"`
	Algorithm   string          `json:"algorithm"`
	Params      ManifestParams  `json:"params"`
	CoveredSize int64           `json:"covered_size"`
	ChunkCount  int             `json:"chunk_count"`
	GeneratedAt time.Time       `json:"generated_at"`
	Chunks      []ManifestChunk `json:"chunks"`
	ServedBy    string          `json:"served_by"`
}

// NewManifestResponse renders a stored manifest for the wire, in index order.
func NewManifestResponse(m manifests.Manifest, servedBy string) ManifestResponse {
	chunks := make([]ManifestChunk, 0, len(m.Chunks))
	for _, c := range m.Chunks {
		chunks = append(chunks, ManifestChunk{
			Offset: c.Offset, Length: c.Length, Digest: c.Digest.String(),
		})
	}
	return ManifestResponse{
		BlobHash:    m.BlobHash.String(),
		Digest:      m.Digest.String(),
		Algorithm:   m.Algorithm,
		Params:      ManifestParams{Min: m.Params.Min, Avg: m.Params.Avg, Max: m.Params.Max},
		CoveredSize: m.CoveredSize,
		ChunkCount:  m.ChunkCount(),
		GeneratedAt: m.GeneratedAt.UTC(),
		Chunks:      chunks,
		ServedBy:    servedBy,
	}
}

// Manifest rebuilds the manifest a response describes, WITHOUT verifying it.
//
// The verification is deliberately the caller's separate step
// ([manifests.Manifest.Validate]), and this method deliberately does not do it
// for them: a constructor that validated would leave callers with no way to
// express "I have not checked this yet", and the destination's rejection path
// is the thing M5-05 has to be able to demonstrate firing.
//
// ChunkCount is checked against the array here, though, because that is a
// decoding question rather than an integrity one — a truncated array with an
// intact count is a broken response, not a tampered manifest, and the digest
// check would report it as the wrong thing.
func (r ManifestResponse) Manifest() (manifests.Manifest, error) {
	blob, err := hashing.Parse(r.BlobHash)
	if err != nil {
		return manifests.Manifest{}, err
	}
	digest, err := hashing.Parse(r.Digest)
	if err != nil {
		return manifests.Manifest{}, err
	}
	if r.ChunkCount != len(r.Chunks) {
		return manifests.Manifest{}, fmt.Errorf(
			"peerapi: the manifest declares %d chunks and carries %d — the response is truncated",
			r.ChunkCount, len(r.Chunks))
	}
	chunks := make([]chunking.Chunk, 0, len(r.Chunks))
	for _, c := range r.Chunks {
		d, err := hashing.Parse(c.Digest)
		if err != nil {
			return manifests.Manifest{}, err
		}
		chunks = append(chunks, chunking.Chunk{Offset: c.Offset, Length: c.Length, Digest: d})
	}
	return manifests.Manifest{
		BlobHash:    blob,
		Algorithm:   r.Algorithm,
		Params:      chunking.Config{Min: r.Params.Min, Avg: r.Params.Avg, Max: r.Params.Max},
		CoveredSize: r.CoveredSize,
		Digest:      digest,
		GeneratedAt: r.GeneratedAt,
		Chunks:      chunks,
	}, nil
}

// handleBlobManifest answers GET /peer/v1/blobs/{hash}/manifest.
//
// Read the package-level comment above before changing anything here. The two
// rules that are not negotiable: this handler generates nothing, and it makes
// no decision on the destination's behalf.
func (s *Server) handleBlobManifest(w http.ResponseWriter, r *http.Request) {
	peer, ok := PeerFrom(r.Context())
	if !ok {
		// The identity middleware is the only path here, so this is a wiring
		// failure rather than a request failure. It is also the assertion that
		// notices a route mounted outside the chain, which is exactly the
		// mistake a new route on an existing listener invites.
		httpapi.Fail(w, r, problem.Internal())
		return
	}
	if s.manifests == nil {
		// The same 503 the content route gives, and for the same reason: this
		// node is not serving content on the peer fabric at all, so there is
		// nothing here to try again for. Answering 404 would have a
		// destination conclude "no manifest, pull whole" and then fail the
		// whole pull against a node that serves no bytes.
		httpapi.Fail(w, r, problem.New(http.StatusServiceUnavailable, problem.TypeInternal,
			"Service Unavailable", "this node is not serving blob content on the peer fabric: it has "+
				"no content store behind its peer surface, so it has no manifests to describe it with"))
		return
	}

	raw := chi.URLParam(r, "hash")
	blob, err := hashing.Parse(raw)
	if err != nil {
		httpapi.Fail(w, r, problem.BadRequest(
			"the path must name a blob as blake3:<64 lowercase hex characters>: "+err.Error()))
		return
	}

	m, state, err := s.manifests.ChunkManifest(r.Context(), blob)
	switch {
	case errors.Is(err, ErrNoSuchBlob):
		// This node does not hold the bytes. A different answer from "no
		// manifest" and a different action for the destination: there is
		// nothing to pull from here at all, whole or in chunks.
		s.log.Info("a peer asked for the manifest of a blob this node does not hold",
			"blob_hash", blob.String(), "peer_id", peer.PeerID, "peer_name", peer.Name)
		httpapi.Fail(w, r, problem.NotFound(
			"this node holds no blob with digest "+blob.String()+". This is not the same as holding "+
				"it with no manifest: there is nothing to read from this source at all, so a "+
				"destination should try another one"))
		return
	case err != nil:
		s.log.Error("reading a chunk manifest for a peer failed",
			"request_id", httpapi.RequestIDFrom(r.Context()),
			"blob_hash", blob.String(), "peer_id", peer.PeerID, "error", err)
		httpapi.Fail(w, r, problem.Internal())
		return
	}

	if state != manifests.StatePresent {
		// 🔴 The line the whole issue is about. There is no manifest, and this
		// handler does not make one. The state is reported so an operator can
		// see WHICH of §16's two manifest-less answers it was — nobody has
		// decided, or somebody decided these bytes never need one — but both
		// are final answers to the destination, which pulls whole.
		s.log.Info("a peer asked for a chunk manifest this node does not have",
			"blob_hash", blob.String(), "manifest_state", state.String(),
			"peer_id", peer.PeerID, "peer_name", peer.Name)
		httpapi.Fail(w, r, problem.New(http.StatusNotFound, problem.TypeNoChunkManifest,
			"No Chunk Manifest",
			"this node holds "+blob.String()+" and has no chunk manifest for it (state: "+
				state.String()+"). Asking does not produce one — a read that chunked a 20 GB blob "+
				"to answer would be a denial of service (ADR-0034) — so this is a final answer: "+
				"pull these bytes whole, which is always correct"))
		return
	}

	body := NewManifestResponse(m, s.self)

	// What this node served, and to whom, the way the content route records
	// "served blob content to a peer". A source is otherwise the one machine
	// in the fabric with no record of what left it, and a source being asked
	// for manifests it does not have — the branch above — is a real diagnostic
	// signal rather than noise.
	s.log.Info("served a chunk manifest to a peer",
		"blob_hash", blob.String(), "manifest_digest", body.Digest,
		"chunk_count", body.ChunkCount, "covered_size", body.CoveredSize,
		"peer_id", peer.PeerID, "peer_name", peer.Name)
	s.writeJSON(w, r, body)
}

// BlobManifestPath is where a peer reads another peer's manifest for a blob.
//
// A constant rather than a string built at each end, for the reason
// [BlobContentPath] is one. Note what it takes: the BLOB's digest. A manifest
// is looked up by the identity of the bytes it describes and is never itself
// an address (ADR-0034), so there is deliberately no path anywhere that names
// a manifest by its own digest.
func BlobManifestPath(hash string) string {
	return Prefix + "/blobs/" + strings.TrimSpace(hash) + "/manifest"
}
