package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/peer/endpoint"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
)

// Reading a source's chunk manifest (§20, ADR-0030, ADR-0034, M5-05).
//
// # The destination asks for a description and decides for itself
//
// This is the destination half of the manifest route. What it sends is a blob
// digest it read out of its OWN catalog, and nothing else: it does not tell
// the source what it already holds, does not ask the source what to send, and
// receives no difference. The source is not a participant in the decision —
// it answers with what it stored, identically for every caller, and everything
// downstream of this function is the destination's own reasoning about its own
// inventory.
//
// # A 404 is an answer, and the answer is "pull whole"
//
// [ErrSourceHasNoManifest] is not a failure. §16 makes chunking lazy and
// ADR-0034 makes a manifest discardable at any moment, so a source that holds
// the bytes and has no manifest for them is in an ordinary, permanent state.
// The caller pulls the blob whole with [Puller.Pull], which is exactly M4's
// behaviour and is always correct. Nothing here retries, escalates, or asks
// the source to produce one — a client that could ask would be a client that
// could make a source read a 20 GB blob off its disks.
//
// [ErrSourceLacksBlob] is the OTHER 404 and it is a different action: this
// source cannot help at all, so try another one. The two are told apart by the
// problem document's `type` URI, which is the contract (see package problem),
// never by its prose.
//
// # The manifest is verified before it is anything
//
// The digest in the response names the MANIFEST, never the blob (ADR-0034).
// It is recomputed here over what actually arrived and compared, so a manifest
// that was truncated, reordered or edited in flight is refused rather than
// stored — and the whole-object digest is still verified independently at the
// end of any transfer that follows, because a proof about the pieces is not
// invariant 1.

// maxManifestBody bounds a manifest response.
//
// A manifest is roughly a hundred bytes per chunk, and the default chunker
// averages 1 MiB, so 32 MiB is on the order of three hundred thousand chunks —
// past any blob a homelab holds, and still a bound. An authenticated peer
// streaming an unbounded body into a JSON decoder is the cheapest denial of
// service in any fabric, and "it is pinned" is not a reason to skip the limit.
const maxManifestBody = 32 << 20

// Refusals the manifest read makes.
var (
	// ErrSourceHasNoManifest is a source that holds the blob and has no chunk
	// manifest for it.
	//
	// An ANSWER rather than an error condition: §16's third state is a
	// legitimate final state, and the caller's correct response is to pull the
	// blob whole. It is separate from [ErrSourceLacksBlob] because the actions
	// differ — this source is still the right source.
	ErrSourceHasNoManifest = errors.New("transfer: this source holds the blob and has no chunk manifest for it — pull it whole (§16, ADR-0034)")
	// ErrManifestCorrupt is a manifest whose recorded digest does not match
	// the chunks that arrived, or which does not describe a byte sequence.
	//
	// It is refused, not stored and not repaired. A manifest is only ever an
	// optimisation, so discarding a bad one costs nothing but a whole pull;
	// keeping one would put a wrong description of a blob's boundaries into
	// the one table a later reassembly trusts.
	ErrManifestCorrupt = errors.New("transfer: the manifest this source served does not check out")
)

// FetchManifest reads one blob's chunk manifest from one source.
//
// The order below is the acceptance condition expressed as control flow, and
// it is the same order [Puller.Pull] uses:
//
//  1. the source is checked for a pinned key and an endpoint, and refused
//     without a connection if it has neither;
//  2. a client pinned to THAT key is built through [Puller.clientFor] — the
//     one construction site in this package, redirect-refusing, and nothing
//     here builds a bare http.Client;
//  3. what comes back is verified against itself and against the digest this
//     function was GIVEN, never against anything the response asserts.
//
// The manifest is returned for the caller to reason about. It is not stored,
// not acted on, and not turned into a request for chunks: this function's only
// output is a description and an error.
func (p *Puller) FetchManifest(
	ctx context.Context, src replication.Source, blob hashing.Hash,
) (manifests.Manifest, error) {
	origin, err := p.originFor(src)
	if err != nil {
		return manifests.Manifest{}, err
	}
	client, err := p.clientFor(src)
	if err != nil {
		return manifests.Manifest{}, err
	}

	url := origin + peerapi.BlobManifestPath(blob.String())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return manifests.Manifest{}, fmt.Errorf(
			"transfer: building the manifest read of %s from peer %s: %w", blob, src.PeerID, err)
	}
	// No body and no query. There is nothing to negotiate: the source is told
	// which blob, and that is the whole of the request (ADR-0030).
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, ErrRedirected) {
			return manifests.Manifest{}, fmt.Errorf("%w: peer %s, reading the manifest of %s",
				ErrRedirected, src.PeerID, blob)
		}
		return manifests.Manifest{}, fmt.Errorf(
			"transfer: reading the manifest of %s from peer %s at %s: %w.\n"+
				"A refusal in this fabric is a failed handshake rather than an error status, so this "+
				"is either that peer refusing this node's key, this node refusing that peer's key, or "+
				"nothing listening at the endpoint", blob, src.PeerID, origin, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return manifests.Manifest{}, p.refusal(resp, src, blob)
	}

	var body peerapi.ManifestResponse
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxManifestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		return manifests.Manifest{}, fmt.Errorf("%w: peer %s answered 200 with something that is not "+
			"a manifest: %w", ErrManifestCorrupt, src.PeerID, err)
	}

	m, err := body.Manifest()
	if err != nil {
		return manifests.Manifest{}, fmt.Errorf("%w: peer %s, blob %s: %w",
			ErrManifestCorrupt, src.PeerID, blob, err)
	}
	// The manifest describes the blob this node asked about, and the check is
	// against the ARGUMENT rather than against the URL or the response: a
	// source that answered with another blob's manifest would otherwise hand
	// this node a valid, self-consistent description of the wrong file, and
	// every per-chunk digest in it would check out.
	if !m.BlobHash.Equal(blob) {
		return manifests.Manifest{}, fmt.Errorf(
			"%w: this node asked peer %s for the manifest of %s and was handed one for %s",
			ErrManifestCorrupt, src.PeerID, blob, m.BlobHash)
	}
	// Validate recomputes the manifest's own digest over the chunk sequence
	// that actually arrived, in the order it arrived in. A reordered array
	// therefore fails HERE rather than at reassembly — which is the only place
	// it could otherwise be caught, and by then bytes have moved.
	if err := m.Validate(); err != nil {
		return manifests.Manifest{}, fmt.Errorf("%w: peer %s, blob %s: %w",
			ErrManifestCorrupt, src.PeerID, blob, err)
	}

	p.log.Info("read a chunk manifest from a peer",
		"blob_hash", blob.String(), "manifest_digest", m.Digest.String(),
		"chunk_count", m.ChunkCount(), "covered_size", m.CoveredSize,
		"source_peer_id", src.PeerID, "source_peer_name", src.Name)
	return m, nil
}

// refusal turns a non-200 into the error that names the action to take.
//
// The discriminator is the problem document's `type` URI, because that is what
// package problem makes the contract: the titles and details are prose and are
// expected to be reworded. A destination that branched on a message would take
// the wrong action the first time somebody improved one.
func (p *Puller) refusal(resp *http.Response, src replication.Source, blob hashing.Hash) error {
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, maxRefusalBody))
	if resp.StatusCode == http.StatusNotFound {
		var doc problem.Problem
		if err := json.Unmarshal(detail, &doc); err == nil && doc.Type == problem.TypeNoChunkManifest {
			// The source holds the bytes and has no description of them. Pull
			// whole — this source is still the right source.
			return fmt.Errorf("%w: peer %s, blob %s", ErrSourceHasNoManifest, src.PeerID, blob)
		}
		// The source does not hold the bytes at all. Try another one.
		return fmt.Errorf("%w: peer %s answered 404 for the manifest of %s",
			ErrSourceLacksBlob, src.PeerID, blob)
	}
	return fmt.Errorf("%w: peer %s answered %d for the manifest of %s: %s",
		ErrSourceRefused, src.PeerID, resp.StatusCode, blob, strings.TrimSpace(string(detail)))
}

// originFor is the pre-flight both reads share: a usable source, at an https
// origin this node can dial.
//
// Extracted so there is one place that decides a candidate is dialable. Two
// copies would be two chances for one of them to accept a unix:// endpoint or
// an unpinned peer, and the traffic would look identical until the day it
// mattered (ADR-0012).
func (p *Puller) originFor(src replication.Source) (string, error) {
	if err := src.Usable(); err != nil {
		// Refused before a socket exists. Membership is the only trust root in
		// the inter-peer path and a candidate with no pinned key is one
		// membership cannot vouch for.
		return "", err
	}
	origin, err := endpoint.Normalise(src.Endpoint)
	if err != nil {
		return "", fmt.Errorf("transfer: peer %s is recorded at an endpoint this node cannot "+
			"dial: %w", src.PeerID, err)
	}
	if !strings.HasPrefix(origin, endpoint.Scheme+"://") {
		return "", fmt.Errorf("%w: peer %s is at %q, which is not an %s origin, and the peer "+
			"surface is mutually authenticated TLS (ADR-0012)",
			endpoint.ErrMalformed, src.PeerID, src.Endpoint, endpoint.Scheme)
	}
	return origin, nil
}
