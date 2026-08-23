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
		Logger:   c.log,
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
