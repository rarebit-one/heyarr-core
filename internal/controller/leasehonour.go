package controller

// leasehonour.go wires the honouring half of ADR-0048's device write leases
// (#285): the request path asks a lease cache whether a device with no
// management grant here is lifted to write by a lease a sibling issued for it.
// It honours only the siblings this node opted into (peer.lease_issuers) that
// are still members, and nothing at all by default.
//
// The fetch beat (leasefetchbeat.go) fills the cache; this reads it. Both are
// handles on the same cached_leases table, the pattern the other controller
// stores follow.

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/leases"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// The lease cache is what the request path consults.
var _ httpapi.DeviceWriteLeases = (*leases.CacheStore)(nil)

// deviceWriteLeases builds the lease cache the request path asks. It returns nil
// (no lease check at all) when this node has no key to be a site by or no
// membership to resolve siblings in. With an empty peer.lease_issuers it is
// still built, and httpapi does not consult it; the honoured set is empty too,
// so the two gates agree.
func (c *Controller) deviceWriteLeases(ctx context.Context, db *sqlite.DB, eventLog *events.Log,
	members *membership.Store, selfKey ed25519.PublicKey,
) (httpapi.DeviceWriteLeases, error) {
	if len(selfKey) != ed25519.PublicKeySize || members == nil {
		return nil, nil
	}
	cache, err := leases.NewCache(leases.CacheOptions{
		Writer: db.Writer(), Reader: db.Reader(), Events: eventLog,
		Siblings: siblingKeys{store: members},
		Honoured: honouredIssuers{store: members, optIn: c.cfg.Peer.LeaseIssuers},
		SelfID:   identity.FormatPublicKey(selfKey), SelfKey: selfKey,
	})
	if err != nil {
		return nil, fmt.Errorf("controller: opening the lease cache for the request path: %w", err)
	}
	warnUnresolvedLeaseIssuers(ctx, members, c.cfg.Peer.LeaseIssuers, c.log)
	return cache, nil
}

// honouredIssuers adapts membership to leases.SiblingKeys for the request path:
// the pinned keys of the peers named in peer.lease_issuers, intersected with
// membership as it is NOW. It runs a query on every call, which is membership's
// own rule (a lookup, never a list loaded at start), so a sibling removed from
// membership stops lifting anyone on the very next request.
//
// Excluded, each for its own reason:
//   - a peer not named in the opt-in: pinned is not opted in (ADR-0038: the
//     honouring site chooses whom to delegate to);
//   - this node: it needs nobody's lease to be authoritative here;
//   - a malformed key: skipped rather than trusted, as siblingKeys does.
type honouredIssuers struct {
	store *membership.Store
	optIn []string
}

func (h honouredIssuers) PeerKeys(ctx context.Context) (map[string]ed25519.PublicKey, error) {
	keys := map[string]ed25519.PublicKey{}
	if len(h.optIn) == 0 {
		return keys, nil
	}
	want := make(map[string]bool, len(h.optIn))
	for _, id := range h.optIn {
		want[id] = true
	}
	members, err := h.store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("controller: listing peers for the lease opt-in: %w", err)
	}
	for _, m := range members {
		if !want[m.PeerID] || m.IsSelf || len(m.PublicKey) != ed25519.PublicKeySize {
			continue
		}
		keys[identity.FormatPublicKey(m.PublicKey)] = m.PublicKey
	}
	return keys, nil
}

// warnUnresolvedLeaseIssuers says, once at startup, which peer.lease_issuers
// entries honour nothing yet: not a member here, or this node itself. It only
// informs. The per-request check reads membership afresh and does not warn,
// because a warning per request would bury the log; the device lease check
// metric's refused_unknown_issuer is where that shows up at runtime.
func warnUnresolvedLeaseIssuers(ctx context.Context, members *membership.Store, optIn []string, log *slog.Logger) {
	if len(optIn) == 0 {
		return
	}
	ms, err := members.List(ctx)
	if err != nil {
		log.Warn("could not check peer.lease_issuers against membership", "error", err)
		return
	}
	byID := make(map[string]membership.Member, len(ms))
	for _, m := range ms {
		byID[m.PeerID] = m
	}
	for _, id := range optIn {
		m, ok := byID[id]
		switch {
		case !ok:
			log.Warn("peer.lease_issuers names a peer that is not enrolled here; its leases lift nobody",
				"peer_id", id, "note", "the id is this node's id for the peer, as `peers list` shows it")
		case m.IsSelf:
			log.Warn("peer.lease_issuers names this node; a node never honours its own leases", "peer_id", id)
		}
	}
}
