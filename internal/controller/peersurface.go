package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
)

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
func (c *Controller) newPeerSurface(self identity.Identity, members *membership.Store) (*peerapi.Server, error) {
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
	srv, err := peerapi.New(peerapi.Options{
		Addr:       c.cfg.Peer.Listen,
		Material:   material,
		Members:    peerLookup{store: members},
		SelfPeerID: self.PeerID,
		Logger:     c.log,
	})
	if err != nil {
		return nil, fmt.Errorf("controller: %w", err)
	}
	return srv, nil
}
