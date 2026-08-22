package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/api/blobs"
	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/events"
	peercatalog "github.com/rarebit-one/heyarr-core/internal/peer/catalog"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
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
) (*peerapi.Server, error) {
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
		Blobs:      blobHandler,
		Logger:     c.log,
	})
	if err != nil {
		return nil, fmt.Errorf("controller: %w", err)
	}
	return srv, nil
}
