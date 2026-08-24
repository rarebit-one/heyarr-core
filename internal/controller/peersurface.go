package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	peercatalog "github.com/rarebit-one/heyarr-core/internal/peer/catalog"
	"github.com/rarebit-one/heyarr-core/internal/peer/endpoint"
	"github.com/rarebit-one/heyarr-core/internal/peer/health"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/peer/reachability"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/manifests"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/pieces"
)

// snapshotSource adapts the catalog to the peer surface's contract (§52,
// M4-13).
//
// The controller id is bound HERE, once, rather than travelling in from the
// request. A snapshot names the controller it came from so that one restored
// from another deployment's backup (§51, §82) is recognisable as somebody
// else's — which it would not be if the value could be supplied by whoever
// asked for the snapshot.
type snapshotSource struct {
	cat  *catalog.Catalog
	self string
}

func (s snapshotSource) BuildSnapshot(
	ctx context.Context, peerID string, holding int64, full bool,
) (*peercatalog.Snapshot, error) {
	return s.cat.BuildSnapshot(ctx, catalog.SnapshotRequest{
		PeerID:       peerID,
		ControllerID: s.self,
		Holding:      holding,
		Full:         full,
	})
}

// peerManifests adapts the catalog to the peer surface's manifest route
// (§20, ADR-0034, M5-05).
//
// # Two reads, and neither of them writes
//
// It asks the catalogue for §16's three-state answer and, only when that
// answer is "present", for the manifest itself. Both are SELECTs on the reader
// pool. There is no branch here that produces a manifest, records a decision
// or enqueues a chunk_blob job, and there must never be: this adapter sits
// behind a route any pinned member can call, so a generate-on-demand path here
// would let one request make this node read a 20 GB blob off its disks
// (ADR-0034).
//
// The reason the state is read FIRST rather than inferring it from a missing
// manifest is the whole of §16: "no manifest" and "these bytes were decided
// never to need one" are different facts, and the log line the handler writes
// says which — which is the difference between an operator seeing a chunker
// that never ran and one seeing a policy working as intended.
type peerManifests struct{ cat *catalog.Catalog }

var _ peerapi.ManifestSource = peerManifests{}

// ChunkManifest satisfies [peerapi.ManifestSource].
func (p peerManifests) ChunkManifest(
	ctx context.Context, blob hashing.Hash,
) (manifests.Manifest, manifests.State, error) {
	state, err := p.cat.ChunkManifestState(ctx, blob)
	switch {
	case errors.Is(err, catalog.ErrManifestBlobUnknown):
		// The catalogue has never heard of these bytes. Translated into the
		// peer surface's own sentinel, because the route answers it
		// differently from "no manifest" and the destination acts on the
		// difference.
		return manifests.Manifest{}, "", fmt.Errorf("%w: %w", peerapi.ErrNoSuchBlob, err)
	case err != nil:
		return manifests.Manifest{}, "", err
	}
	if state != manifests.StatePresent {
		// The answer, not a condition to resolve.
		return manifests.Manifest{}, state, nil
	}
	m, found, err := p.cat.ChunkManifest(ctx, blob)
	if err != nil {
		return manifests.Manifest{}, "", err
	}
	if !found {
		// A manifest discarded between the two reads. ADR-0034 makes that a
		// supported operation at any moment, so it is reported as the state it
		// now genuinely is rather than as an error — and certainly not by
		// regenerating what somebody just deleted.
		return manifests.Manifest{}, manifests.StateUndecided, nil
	}
	return m, manifests.StatePresent, nil
}

// peerLookup adapts the membership store to the transport's trust root.
//
// It is here rather than in either package on purpose. internal/peer/mtls must
// stay free of the control plane's storage — `heyarr peers ping` dials a peer
// with one key read from the API and has no database at all — and
// internal/peer/membership must stay free of the transport, so that the
// question "is this key a member" cannot acquire a TLS-shaped answer. The
// controller is the one place that already holds both.
type peerLookup struct{ store *membership.Store }

// Lookup answers the transport's question with the trust root's answer.
//
// It queries every time, because the store does. There is nothing memoised
// here and there must never be: ADR-0012 makes revocation the deletion of a
// record and leaves no CRL, so a cache in this adapter would be a revocation
// window nobody chose — invisible in every test where nobody is removed.
func (l peerLookup) Lookup(ctx context.Context, publicKey []byte) (mtls.Peer, error) {
	m, err := l.store.Lookup(ctx, publicKey)
	switch {
	case errors.Is(err, membership.ErrNotAMember):
		// Translated, not wrapped-and-forgotten: the transport branches on its
		// own error, and a refusal that arrived as "some database error" would
		// be reported as an unavailable trust root, which fails closed but
		// tells the operator the wrong thing.
		return mtls.Peer{}, fmt.Errorf("%w: %w", mtls.ErrNotAMember, err)
	case err != nil:
		return mtls.Peer{}, err
	}
	return mtls.Peer{PeerID: m.PeerID, Name: m.Name, PublicKey: m.PublicKey}, nil
}

// returnPathProber answers the peer surface's reachback route (#186,
// ADR-0037): can this node reach the peer that is asking?
//
// The address it dials is THIS node's own membership record for that peer, and
// never anything the caller supplied — see peerapi.ReturnPathProber for why.
//
// # Why it is a transport dial and not a request
//
// The obvious implementation asks the peer's own surface for its identity,
// exactly as the outbound leg does. It cannot be that, and the reason is
// ORDERING. Enrolment is two operators running two commands, and between the
// first and the second the far node has not enrolled this one yet: a
// credentialled probe would be refused at the handshake, be read as an
// unreachable return path, and refuse the very enrolment that would have made
// it work. The check would then be impossible to satisfy in the order the
// documentation prescribes.
//
// So the question asked here is the one the pairing actually turns on: do
// packets get through in this direction. A completed TCP connection to the
// address recorded for that peer answers it, needs no credential on either
// end, and is unaffected by which half of the enrolment has happened. It is
// not evidence that the far end is well, and it is not meant to be — identity
// is already settled by the connection this probe was requested over.
//
// /healthz is deliberately not used, though internal/peer/health probes it for
// read routing. A peer's recorded endpoint is its mTLS peer surface, which
// answers no plaintext HTTP at all; probing it that way reports every real
// peer as unreachable, which is exactly what this route must not do.
type returnPathProber struct {
	store   *membership.Store
	timeout time.Duration
}

// returnPathTimeout bounds one dial. The caller is waiting on this inside its
// own enrolment, so it is short: an address that has not completed a TCP
// handshake in five seconds is not one the return flows will be using.
const returnPathTimeout = 5 * time.Second

func (p returnPathProber) ProbeReturnPath(
	ctx context.Context, peerID string,
) (reachability.Result, string, error) {
	member, err := p.store.Get(ctx, peerID)
	switch {
	case errors.Is(err, membership.ErrUnknownPeer):
		// Authenticated by certificate and pinned by the trust root, yet not
		// a row here. That is an operator problem rather than a network one,
		// and reporting it as unreachable would refuse an enrolment for the
		// wrong reason.
		return reachability.ResultUnknown, "", nil
	case err != nil:
		return reachability.ResultUnknown, "", err
	}
	if member.Endpoint == "" {
		return reachability.ResultUnknown, "", nil
	}
	address, ok := dialAddress(member.Endpoint)
	if !ok {
		// A unix:// endpoint, or something no normalisation rescues. A peer on
		// this host has no return path to prove, and a malformed row is a
		// configuration fault rather than a network one. Neither is evidence.
		return reachability.ResultUnknown, member.Endpoint, nil
	}

	timeout := p.timeout
	if timeout <= 0 {
		timeout = returnPathTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return reachability.ResultUnreachable, member.Endpoint, nil
	}
	_ = conn.Close()
	return reachability.ResultReachable, member.Endpoint, nil
}

// dialAddress reduces a recorded endpoint to the host:port a probe dials, or
// reports that there is nothing dialable in it.
func dialAddress(recorded string) (string, bool) {
	normalised, err := endpoint.Normalise(recorded)
	if err != nil {
		return "", false
	}
	u, err := url.Parse(normalised)
	if err != nil || u.Scheme != endpoint.Scheme || u.Host == "" {
		return "", false
	}
	if u.Port() == "" {
		return net.JoinHostPort(u.Hostname(), "443"), true
	}
	return u.Host, true
}

// newPeerSurface builds this node's mTLS peer listener (§26, ADR-0012, M4-05).
//
// It is always constructed and binds nothing unless peer.listen is set. That
// asymmetry is deliberate: constructing it proves at every startup that the
// identity on disk can produce a certificate, while binding it is an explicit
// decision by an operator who has a second site. A single-node deployment —
// `heyarr all` on a laptop, and the split-process acceptance path — therefore
// needs no certificate configuration of any kind, which is a requirement
// rather than a convenience: loopback must never have to authenticate itself
// to itself.
func (c *Controller) newPeerSurface(
	db *sqlite.DB, self identity.Identity, members *membership.Store, blobStore cas.Store,
	material *mtls.Material, peerHealth *health.Tracker,
) (*peerapi.Server, error) {
	// The catalog behind the peer surface's inventory route (M4-07) and its
	// catalog-snapshot route (§52, M4-13).
	//
	// A peer runs no control plane and cannot write control-plane rows
	// directly (ADR-0029): it reports what its disk holds, and the
	// controller's single writer records that. This is that writer — and it is
	// also the reader the snapshot is built from, which is why ONE catalog
	// serves both directions rather than two: the inventory a peer reports and
	// the snapshot it is issued are two views of the same state, and two
	// catalogs would be two event logs recording halves of one conversation.
	//
	// It gets its own event log for the same reason every other construction
	// in this file does — one log per process would be tidier and is a
	// refactor rather than this issue.
	peerEvents, err := events.New(events.Options{
		Writer: db.Writer(), Reader: db.Reader(), Logger: c.log,
	})
	if err != nil {
		return nil, fmt.Errorf("controller: opening the event log for the peer surface: %w", err)
	}
	peerCatalog, err := catalog.New(catalog.Options{
		DB: db, Events: peerEvents,
		PeerName: c.cfg.Peer.Name, PeerSite: c.cfg.Peer.Site, Logger: c.log,
	})
	if err != nil {
		return nil, fmt.Errorf("controller: opening the catalog for the peer surface: %w", err)
	}

	// Byte serving on the peer fabric (M4-09). The SAME handler the client API
	// mounts, over the same store, differing only in the credential that
	// reaches it — ADR-0013's "a contract, not an endpoint" is the reason there
	// is one handler here rather than a replication-shaped second one.
	//
	// This is the only place in the process where a replica's bytes leave the
	// machine, and the controller's own API is not on that path: a destination
	// dials this listener directly (ADR-0030, §32).
	blobHandler, err := blobs.New(blobs.Options{Store: blobStore, Logger: c.log})
	if err != nil {
		return nil, fmt.Errorf("controller: building the peer surface's blob handler: %w", err)
	}

	srv, err := peerapi.New(peerapi.Options{
		Addr:       c.cfg.Peer.Listen,
		Material:   material,
		Members:    peerLookup{store: members},
		SelfPeerID: self.PeerID,
		Inventory:  peerCatalog,
		Snapshots:  snapshotSource{cat: peerCatalog, self: self.PeerID},
		ReturnPath: returnPathProber{store: members},
		Blobs:      blobHandler,
		// The peer surface's own liveness observation (#184). Without it a
		// remote peer — which holds no bearer token and so never reaches the
		// client API's guard — could talk to this node all day without its
		// stored health ever leaving `unknown`.
		Liveness: peerLiveness(peerHealth),
		// The description of those same bytes (M5-05). The SAME catalogue the
		// inventory and snapshot routes use, read-only — one store of record,
		// and a manifest route reading a second one would be a second opinion
		// about what this node holds.
		Manifests: peerManifests{cat: peerCatalog},
		// What this node holds of a blob, whole or in part (M6, ADR-0042).
		// The same store the content route serves from, so availability and
		// bytes cannot disagree about what is here.
		Pieces: peerPieces{store: blobStore},
		Logger: c.log,
	})
	if err != nil {
		return nil, fmt.Errorf("controller: %w", err)
	}
	return srv, nil
}

// peerMaterial builds this node's certificate material.
//
// It is separate from newPeerSurface because two things need it and one of
// them is built first: the peer LISTENER presents it, and the health probe
// DIALS with it (#184). Deriving it twice would be two certificates for one
// identity, which is not wrong so much as it is two places to get a lifetime
// wrong.
func (c *Controller) peerMaterial(self identity.Identity) (*mtls.Material, error) {
	// The private half, read here and nowhere else in the controller. It never
	// reaches Identity, a log field or a response body — it exists to sign one
	// certificate that is regenerated in memory and never written down.
	priv, err := identity.Signer(c.cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("controller: loading this peer's private key: %w", err)
	}
	material, err := mtls.NewMaterial(mtls.MaterialOptions{
		PrivateKey: priv,
		PeerID:     self.PeerID,
	})
	if err != nil {
		return nil, fmt.Errorf("controller: %w", err)
	}
	return material, nil
}

// peerLiveness converts a possibly-absent tracker into the interface the peer
// surface takes, for the reason liveness() does it for the client API: a typed
// nil pointer in an interface is not a nil interface, and the guard would read
// it as present and panic on the first peer request.
func peerLiveness(t *health.Tracker) peerapi.Liveness {
	if t == nil {
		return nil
	}
	return t
}

// peerPieces reports what this node holds of a blob, whole or in part — the
// question that makes a swarm possible (§23, ADR-0042, ADR-0043).
//
// # Two sources of truth, and they answer different halves
//
// A blob this node holds COMPLETELY is in the store, and its availability is
// every piece. A blob this node is still FETCHING has a sparsely written
// partial and a bitset beside it recording which pieces landed. There is no
// third case: a blob is held, being fetched, or unknown here.
//
// The store is asked first, because a complete blob's answer does not depend on
// any record having survived. A crash between the last piece landing and the
// bitset being written leaves a complete blob and a stale bitset, and asking
// the bitset first would report that blob as incomplete — which would have a
// peer fetch pieces of something this node could have served whole.
//
// # It computes nothing
//
// The geometry comes from the blob's SIZE, which the store already knows, so
// answering never reads a byte of content. That is what makes this safe behind
// a route any pinned member can call: a GET that had to read a 20 GB blob to
// answer would be a remote denial of service, exactly as ADR-0034 says of
// generating a manifest on demand.
type peerPieces struct{ store cas.Store }

// pieceProgressReader is the half of the store this adapter needs that
// cas.Store does not declare.
//
// A narrow interface asserted at the call site rather than a method added to
// cas.Store, because piece progress is M6's business and every other
// implementation of Store — and every test double — would otherwise have to
// grow a method it will never use. A store that does not offer it simply has no
// in-flight transfers to report, which is the honest answer for one that cannot
// stage them.
type pieceProgressReader interface {
	LoadPieceProgress(blob hashing.Hash) (string, error)
}

var _ peerapi.PieceSource = peerPieces{}

// PieceAvailability satisfies [peerapi.PieceSource].
func (p peerPieces) PieceAvailability(
	ctx context.Context, blob hashing.Hash,
) (string, bool, error) {
	if p.store == nil {
		return "", false, nil
	}

	// Held whole? Then every piece, derived from the size.
	desc, err := p.store.Stat(ctx, blob)
	switch {
	case err == nil:
		g, gerr := pieces.For(desc.Size)
		if gerr != nil {
			// A zero-length blob has no pieces. Reported as "nothing to
			// exchange" rather than as an error: it is a real blob and a
			// session that wanted it should fetch it whole.
			return "", false, nil //nolint:nilerr // an empty blob is not a fault
		}
		have := pieces.NewAvailability(g.Count())
		for i := range g.Count() {
			have.Add(i)
		}
		return have.Encode(), true, nil
	case errors.Is(err, cas.ErrNotFound):
		// Fall through to the in-flight case.
	default:
		return "", false, err
	}

	// Being fetched? Then whatever the bitset says, which is a HINT and is
	// carried verbatim — this adapter does not parse it, because ADR-0041
	// keeps a piece a transport detail and the control plane does not learn
	// what one is.
	reader, ok := p.store.(pieceProgressReader)
	if !ok {
		// No staging, so nothing in flight to report.
		return "", false, nil
	}
	progress, err := reader.LoadPieceProgress(blob)
	if err != nil {
		return "", false, err
	}
	if progress == "" {
		// Neither held nor in flight, or in flight with nothing landed yet.
		// The route answers 404 for the first; this collapses the second into
		// it, and that is a deliberate simplification — a peer with nothing
		// yet has nothing to offer either way, and distinguishing them would
		// mean keeping a record of transfers that have produced nothing.
		return "", false, nil
	}
	return progress, true, nil
}
