package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	domaintransfer "github.com/rarebit-one/heyarr-core/internal/domain/transfer"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/pieces"
)

// Candidate is one source a piece pull may use, and which contract it speaks.
//
// The kind is not decoration and it is not derived. A peer and a web seed can
// be the SAME MACHINE at the same address: the difference is which routes
// answer. A peer exchanges pieces; a web seed only serves byte ranges of a
// whole blob it already holds, which is every Heyarr node whether or not it
// runs piece exchange at all (§27).
//
// Stating it rather than probing for it is deliberate. Discovering the kind by
// trying the piece route and falling back on a 404 would make a peer whose
// piece route is BROKEN indistinguishable from one that never had it, and the
// transfer would quietly downgrade instead of reporting a fault.
type Candidate struct {
	// Source is where and who, and how to authenticate.
	Source replication.Source
	// Kind is which contract this source speaks. KindPeer and KindWebSeed are
	// the two this transport can drive; KindExternal is acquisition's business
	// (§25) and is refused here rather than silently skipped.
	Kind domaintransfer.Kind
}

// Peer names a source that speaks piece exchange.
func Peer(src replication.Source) Candidate {
	return Candidate{Source: src, Kind: domaintransfer.KindPeer}
}

// WebSeed names a source that serves byte ranges and nothing else.
func WebSeed(src replication.Source) Candidate {
	return Candidate{Source: src, Kind: domaintransfer.KindWebSeed}
}

// ErrUndrivableSource is a source of a kind this transport cannot drive.
var ErrUndrivableSource = errors.New("transfer: this transport cannot drive that kind of source")

// # Which credential a web seed uses, and why it is not a new one
//
// §27 calls this "the ordinary Heyarr HTTP Blob endpoint", and there are two of
// those. The choice matters, so it is stated here rather than implied by an
// import.
//
// It is the PEER surface's content route, over mTLS — the same credential the
// node already holds, no new one (ADR-0012).
//
// The client API's blob route was the alternative, with a capability minted per
// fetch (ADR-0040). That is right for a renderer: a browser cannot present a
// peer certificate, and a capability is how a thing outside the fabric is
// handed bytes without being made a member. A transfer is not outside the
// fabric. Using the capability path would mean minting one per transfer, giving
// every piece pull a credential lifecycle it does not need, and putting an
// issuing step between a node and bytes it is already entitled to.
//
// The issue asks the question this way round: *a web seed that needs an admin
// token is not a web seed*. Nothing here needs one. The peer certificate is
// what a node has by being a member, and the web seed is exactly a member that
// happens not to run piece exchange.
//
// This also settles what a web seed IS: not a foreign HTTP origin, but a
// participant reachable over HTTP that is not dialling for pieces. Serving
// arbitrary origins would mean deciding what to trust about bytes from
// somewhere with no membership at all, and invariant 1 answers that — the
// digest — but the reachability and accounting stories do not, and inventing
// them was not asked for.

// fetchPieceFromWebSeed reads one piece as a byte range of the whole blob.
//
// A fixed piece IS a byte range (§28, ADR-0013), so the serving side needs no
// piece awareness and this deliberately adds none: it asks the ordinary content
// route for the range the geometry implies.
func (p *Puller) fetchPieceFromWebSeed(
	ctx context.Context, src replication.Source, blob hashing.Hash,
	g pieces.Geometry, index int,
) ([]byte, error) {
	off, length, err := g.Range(index)
	if err != nil {
		return nil, err
	}
	client, err := p.clientFor(src)
	if err != nil {
		return nil, err
	}
	origin, err := p.originFor(src)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		origin+peerapi.BlobContentPath(blob.String()), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+length-1))

	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, ErrRedirected) {
			return nil, fmt.Errorf("%w: web seed %s, piece %d of %s",
				ErrRedirected, src.PeerID, index, blob)
		}
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: web seed %s does not hold %s", ErrSourceLacksBlob, src.PeerID, blob)
	case http.StatusRequestedRangeNotSatisfiable:
		// The web seed holds a blob of a different length under this digest,
		// which cannot be — or this node was told the wrong size. Either way it
		// is not a source for this blob, and it is NOT a transport failure to
		// be retried.
		return nil, fmt.Errorf(
			"%w: web seed %s refused bytes %d-%d of %s as outside the blob, so it holds a "+
				"different length under that digest or this node was told the wrong size",
			ErrRangeRefused, src.PeerID, off, off+length-1, blob)
	default:
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, maxRefusalBody))
		return nil, fmt.Errorf("%w: %d for bytes %d-%d of %s from web seed %s: %s",
			ErrRangeRefused, resp.StatusCode, off, off+length-1, blob, src.PeerID,
			strings.TrimSpace(string(detail)))
	}

	// A source that answers 206 and then streams something else is refused
	// rather than assembled — the same discipline the chunk path applies, and
	// for the same reason: a source ignoring Range answers the WHOLE blob, and
	// its first bytes would look like a plausible piece 0.
	buf := make([]byte, length)
	if _, err := io.ReadFull(io.LimitReader(resp.Body, length), buf); err != nil {
		return nil, fmt.Errorf("web seed %s served a short piece %d of %s: %w",
			src.PeerID, index, blob, err)
	}
	// Bounded to one byte: an over-long 206 is a source answering a question it
	// was not asked, and the extra bytes are exactly what the next piece's
	// offset would land on. Reading to the end also lets the connection be
	// reused, which matters at one request per piece.
	if extra, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, 1)); extra > 0 {
		return nil, fmt.Errorf("%w: web seed %s sent more than the %d bytes of piece %d it was "+
			"asked for", ErrRangeRefused, src.PeerID, length, index)
	}
	return buf, nil
}
