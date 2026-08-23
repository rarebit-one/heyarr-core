// Package transfer is the byte-carrying half of replication: the destination
// opening a pinned connection to a source, reading the blob endpoint, and
// verifying what arrives against what it asked for (§20, §21, §32, ADR-0030,
// M4-09).
//
// # The destination pulls, and the destination verifies
//
// ADR-0030 settles the direction §21's arrow left ambiguous: the destination
// computes what it is missing, opens the connection, and hashes the bytes as
// they arrive against the digest it expected. Nothing in this package is told
// what it is receiving by the thing sending it.
//
// That distinction is the whole of invariant 1 and it is easy to lose. The
// failure mode is not absent verification — nobody writes that — it is
// verification against the hash the source SAID it was sending, or against the
// hash in the URL on the assumption that the response matches it. Both are
// trusting the source with extra steps. So this package never parses a digest
// out of a response: the expectation arrives as an argument, from the
// reconciliation that scheduled the work, and is handed straight to
// cas.PutExpecting, which is the only thing here that decides whether bytes
// become a blob.
//
// # The controller is not on this path
//
// §32: "The controller stays out of the content data path." The connection is
// opened to the source's own endpoint, read from the source's own peer surface,
// and it does not follow redirects — see [ErrRedirected]. A 302 is the cheapest
// way for the controller to end up carrying bytes it was never supposed to see,
// and it is the shape a well-meaning reverse proxy in front of a NATed peer
// produces without anyone deciding to.
//
// # The manifest read is a description, not a negotiation
//
// manifest.go adds the destination half of GET
// /peer/v1/blobs/{hash}/manifest (M5-05). It changes none of the above: it
// goes through the same pinned, redirect-refusing [Puller.clientFor], it sends
// a blob digest read from this node's OWN catalog and nothing else, and it
// verifies what arrives against itself before the caller sees it. The source
// is never told what this node holds and never computes a difference. A source
// with no manifest is a 404 and an answer — the caller pulls whole, which is
// what this file already does.
//
// # There IS resumption here now, and what it is allowed to trust
//
// Until Milestone 5 this paragraph said the opposite, and it said it as a
// warning: "Nothing in this package may learn to send a Range header for a
// resume." That prohibition is lifted, deliberately, by ADR-0035 — and the
// condition it is lifted under is stricter than the prohibition was, so it is
// rewritten here rather than deleted. A caveat that vanishes silently reads,
// to the next person, as one nobody thought about.
//
// [Puller.Pull] is unchanged: one unranged read, verified as it streams,
// published by cas.PutExpecting, and retried WHOLE on failure. It is still
// what every blob with no chunk manifest gets, which is §16's lazy chunking
// doing its job (see resume.go's [ModeWhole]).
//
// [Puller.PullChunked] is the resumable path, and it may send a Range header
// under exactly one condition: **the range is a chunk boundary out of a
// manifest this node fetched and verified itself, and what arrives is hashed
// against that manifest's digest for that chunk before it counts as
// received.** The forbidden shape — "I have 4 GB, send me from 4 GB" — is
// still forbidden and is not computable here: the loop's only input is the
// manifest, and a resumed transfer re-hashes the prefix it kept rather than
// trusting its length. ADR-0035 sets out why the offset resume cannot be made
// safe: the only ways to make it work are persisting BLAKE3 hasher state
// (an unverified serialised intermediate, which is exactly what invariant 1
// exists to have none of), re-reading the prefix anyway, or skipping the
// whole-object verification.
//
// So the destination still verifies the assembled whole-object digest itself,
// on this path as on every other, before anything is published (ADR-0034).
// Partial bytes live in the store's private staging area, are addressable by
// nothing, are a replica of nothing, and are reaped by age.
//
// # Idempotence under invariant 9, restated
//
// The handler used to be idempotent because a receive that did not finish left
// nothing to be right about. It is now idempotent in a stronger sense: a re-run
// may find work already done and RE-VERIFIES it rather than trusting it, so
// the outcome of one run and of ten interrupted runs is byte-identical.
package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// maxRefusalBody bounds how much of a source's refusal is read back into an
// error message. A problem document is a few hundred bytes; a source that
// answers a non-200 with a gigabyte is not something to put in a log line.
const maxRefusalBody = 8 << 10

// Refusals this package makes.
var (
	// ErrRedirected is a source answering with a redirect.
	//
	// It is refused rather than followed, and the refusal is the point. A
	// redirect is how the controller ends up in the data path without anybody
	// deciding it should be: a reverse proxy in front of an awkwardly NATed
	// peer, a "temporary" indirection during a migration, a load balancer
	// doing what load balancers do. Any of them makes controller availability
	// into replication's availability, which is the coupling §53 exists to
	// avoid (§32, ADR-0030).
	//
	// A destination pulls from the peer its own membership record names, at
	// the endpoint its own catalog records, and from nowhere else.
	ErrRedirected = errors.New("transfer: the source answered with a redirect, and a replication pull follows none — the controller must never be in the data path (§32, ADR-0030)")
	// ErrSourceLacksBlob is a source that does not hold the blob after all:
	// its inventory report is stale, or it lost the bytes since. Separate from
	// a transport failure because the response is different — try another
	// source, do not retry this one.
	ErrSourceLacksBlob = errors.New("transfer: this source does not hold these bytes")
	// ErrSourceRefused is any other non-200 from the source.
	ErrSourceRefused = errors.New("transfer: the source refused this read")
)

// Store is the half of a content store a transfer needs.
//
// Narrow on purpose, and the two writes it may make are the two that verify
// before they publish: PutExpecting for a whole pull, and OpenPartial's
// [cas.Partial.Publish] for a chunked one. There is no third, which is what
// keeps "verify then publish" from being two steps a caller could reorder.
//
// Open and Verify are reads, and they are here for chunk reuse (M5-07): Open
// supplies a chunk this node already holds out of the blob that holds it, and
// Verify is what a mismatch on such a chunk points at the donor blob, so that
// a damaged blob reaches quarantine on the path that already exists for it and
// a merely stale index entry does not (ADR-0018).
type Store interface {
	Has(ctx context.Context, h hashing.Hash) (bool, error)
	PutExpecting(ctx context.Context, r io.Reader, expected hashing.Hash) (cas.Descriptor, error)
	OpenPartial(ctx context.Context, expected hashing.Hash) (cas.Partial, error)
	Open(ctx context.Context, h hashing.Hash) (cas.ReadSeekCloser, cas.Descriptor, error)
	Verify(ctx context.Context, h hashing.Hash) error
}

// Options configure a Puller.
type Options struct {
	// Material is this node's certificate, and therefore its identity to the
	// source. Required: the peer fabric authenticates by certificate only.
	Material *mtls.Material
	// Store is where verified bytes land. Required.
	Store Store
	// Index is this node's local chunk index, consulted to decide which chunks
	// of a transfer it can supply from blobs it already holds (M5-07).
	//
	// Optional, and a Puller without one is M5-06 without M5-07: it resumes
	// and it never reuses. That is a legitimate configuration rather than a
	// degraded one — a node with no index answers "I hold none of these
	// chunks", which is the same answer an empty index gives.
	Index Index
	// Logger records what was pulled from where. Optional.
	Logger *slog.Logger
}

// Puller pulls blobs from source peers into this node's content store.
type Puller struct {
	material *mtls.Material
	store    Store
	index    Index
	log      *slog.Logger
}

// New builds a Puller, or explains what is missing.
func New(opts Options) (*Puller, error) {
	switch {
	case opts.Material == nil:
		return nil, errors.New("transfer: this node's certificate material is required — " +
			"the peer fabric authenticates by certificate only (ADR-0012)")
	case opts.Store == nil:
		return nil, errors.New("transfer: a content store is required — a transfer with nowhere to " +
			"land is a read")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Puller{
		material: opts.Material, store: opts.Store, index: opts.Index,
		log: log.With("component", "transfer"),
	}, nil
}

// Outcome is what one completed pull did.
//
// The chunk and byte counters are the number M5-07 exists to produce: an
// operator who cannot see how much a transfer reused cannot tell whether reuse
// is working, because a transfer that reused everything and one that reused
// nothing both end in a published blob and a log line saying so.
type Outcome struct {
	// SourcePeerID is the peer the bytes actually came from.
	SourcePeerID string
	// Mode is which path moved the bytes — whole or chunked. An enum, because
	// the thing worth asserting is which path RAN, and "it succeeded" is
	// satisfied by either.
	Mode Mode
	// Bytes is how many the published blob has, verified whole.
	Bytes int64
	// Deduplicated reports that the store already held these bytes by the time
	// they landed — a concurrent transfer or an ingest won the race. It is
	// success, not a conflict: the blob is present and it is the right blob.
	Deduplicated bool

	// ChunksKept and BytesKept are what a previous attempt left on disk and
	// this one re-hashed against the manifest and believed. Zero on a first
	// attempt and on the whole path.
	ChunksKept int
	BytesKept  int64
	// ChunksReused and BytesReused are what this node supplied out of blobs it
	// already held, each re-verified against the manifest before use.
	ChunksReused int
	BytesReused  int64
	// ChunksFetched and BytesFetched are what came off the wire. This is the
	// number the saving is measured against — though the honest measurement is
	// the SOURCE's served bytes, because this one is the destination's own
	// claim about itself.
	ChunksFetched int
	BytesFetched  int64
	// ChunksIndexStale counts chunks the local index claimed and the bytes did
	// not support, which were fetched instead. It is not an error count: an
	// index entry going stale is ordinary. It is a zero that should stay zero,
	// and a number that says the re-verification is doing something when it
	// does not.
	ChunksIndexStale int
}

// Pull fetches one blob from one source and verifies it against expected.
//
// The order below is the acceptance condition, expressed as control flow:
//
//  1. the source is checked for a pinned key and an endpoint, and refused
//     without a connection if it has neither (before any bytes move);
//  2. a client pinned to THAT key is built — not a general-purpose one — so a
//     DNS change or a hijacked address cannot substitute another machine;
//  3. the body is streamed into the store against the expectation this
//     function was given, never against anything in the response.
//
// A non-200 is never a partial success. There is one call site that turns a
// stream into a blob and it is [Store.PutExpecting], which publishes nothing it
// has not verified.
func (p *Puller) Pull(
	ctx context.Context, src replication.Source, expected hashing.Hash,
) (Outcome, error) {
	// Refused before a socket exists if the candidate has no pinned key or no
	// dialable endpoint — dialling it and accepting whatever answered would be
	// trust on first use. A unix:// endpoint is a legitimate peer endpoint and
	// a legitimate probe target (§31), and it is not something a mutually
	// authenticated TLS connection can be presented over. See originFor, which
	// the manifest read shares so there is one place that decides this.
	origin, err := p.originFor(src)
	if err != nil {
		return Outcome{}, err
	}

	client, err := p.clientFor(src)
	if err != nil {
		return Outcome{}, err
	}

	url := origin + peerapi.BlobContentPath(expected.String())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Outcome{}, fmt.Errorf("transfer: building the read of %s from peer %s: %w",
			expected, src.PeerID, err)
	}
	// No Range header on THIS path, deliberately, and it is not an oversight
	// left over from M4: a whole pull is what a blob with no chunk manifest
	// gets, and it has nothing to resume against. Ranged reads live in
	// resume.go, where every one of them is a chunk boundary from a verified
	// manifest (ADR-0035).
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, ErrRedirected) {
			return Outcome{}, fmt.Errorf("%w: peer %s, reading %s", ErrRedirected, src.PeerID, expected)
		}
		return Outcome{}, fmt.Errorf("transfer: reading %s from peer %s at %s: %w.\n"+
			"A refusal in this fabric is a failed handshake rather than an error status, so this is "+
			"either that peer refusing this node's key, this node refusing that peer's key, or "+
			"nothing listening at the endpoint", expected, src.PeerID, origin, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, maxRefusalBody))
		if resp.StatusCode == http.StatusNotFound {
			// The peer's inventory said it held this and it does not. Ordinary
			// — an inventory is a snapshot — and it is a different action from
			// a refusal: another source may have it.
			return Outcome{}, fmt.Errorf("%w: peer %s answered 404 for %s",
				ErrSourceLacksBlob, src.PeerID, expected)
		}
		return Outcome{}, fmt.Errorf("%w: peer %s answered %d for %s: %s",
			ErrSourceRefused, src.PeerID, resp.StatusCode, expected, strings.TrimSpace(string(detail)))
	}

	// The one line that decides whether these bytes become a blob, and the
	// expectation it is given is this function's ARGUMENT — read from the
	// destination's own catalog by the caller. Nothing from resp is consulted:
	// not the ETag, not the URL, not a length. A source that sent the wrong
	// bytes gets them quarantined rather than published (ADR-0018), and a
	// stream that dies mid-flight leaves nothing addressable at all.
	desc, err := p.store.PutExpecting(ctx, resp.Body, expected)
	if err != nil {
		return Outcome{}, err
	}

	p.log.Info("pulled a blob from a peer",
		"blob_hash", expected.String(), "source_peer_id", src.PeerID,
		"source_peer_name", src.Name, "bytes", desc.Size, "deduplicated", desc.Deduplicated)
	return Outcome{
		SourcePeerID: src.PeerID, Mode: ModeWhole,
		Bytes: desc.Size, Deduplicated: desc.Deduplicated,
	}, nil
}

// clientFor builds a client pinned to exactly one peer's key.
//
// One client per source rather than one shared client, because the pin is per
// peer: mtls.PinnedKey is a membership of exactly one key, and it is the
// strongest available answer here for the reason `heyarr peers ping` gives —
// the caller has already been told, by its own membership record, which key it
// is about to talk to.
//
// Nothing in this package builds a bare http.Client, and nothing may. A caller
// that did would be one line from an unpinned transport, and the traffic would
// look identical until the day it mattered (ADR-0012).
func (p *Puller) clientFor(src replication.Source) (*http.Client, error) {
	client, err := mtls.Client(mtls.Options{
		Material: p.material,
		Members: mtls.PinnedKey(mtls.Peer{
			PeerID: src.PeerID, Name: src.Name, PublicKey: src.PublicKey,
		}),
		Logger: p.log,
	})
	if err != nil {
		return nil, fmt.Errorf("transfer: building a pinned client for peer %s: %w", src.PeerID, err)
	}
	// The redirect refusal, set here because this is the only place a client
	// for a pull is constructed. It is a property of the pull rather than of
	// the transport, and stating it at the one construction site means there is
	// no second one where it could be omitted.
	client.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		return fmt.Errorf("%w: it pointed at %s", ErrRedirected, req.URL.Redacted())
	}
	return client, nil
}
