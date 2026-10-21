package leases

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rarebit-one/void-which-binds-go/grant"
)

// Device write leases (ADR-0048 amendment, #285): this peer lifts a device it
// has authorised for write (a management grant, ADR-0065) to write at each of
// its siblings, by issuing "device D may write at site B" for every sibling B.
// Only B honours a lease whose resource is B, and only if B opted into this
// peer as an issuer; that check, and the opt-in, belong to the honouring side.
//
// # Every sibling, not only the ones that opted in
//
// The issuer cannot know which siblings opted into it: the opt-in is B's
// per-site authority (ADR-0038) and B never reports it. It does not need to.
// A lease B has not opted into is refused at B as `unknown_issuer`, and a lease
// is never a bearer credential (B looks it up by the key a `Device` credential
// just proved), so a lease issued for a sibling that ignores it opens nothing
// anywhere. Issuing for every sibling keeps the opt-in in exactly one place.
const (
	// DevicePrincipalPrefix marks a lease whose principal is an enrolled
	// device's key: the same at every site, unlike a node-local PrincipalID.
	DevicePrincipalPrefix = "device:"
	// SiteResourcePrefix marks a lease whose resource is a site, named by its
	// peer key (identity.FormatPublicKey): the site that honours the lease.
	SiteResourcePrefix = "site:"

	// DeviceWriteTTL is the life of a device write lease: the whole of
	// grant.MaxTTL (ADR-0048). It is the partition bound — how long a sibling
	// that cannot reach this peer keeps honouring a lease this peer has since
	// revoked — and the most authority a cached lease can carry through an
	// outage. Steady-state revocation is far faster: the beat stops re-issuing
	// and revokes on its next pass, and a reachable sibling drops the lease on
	// its next fetch.
	DeviceWriteTTL = grant.MaxTTL

	// DeviceWriteRefreshThreshold is the least life a served device write
	// lease has. A live lease is reused while its remaining life is at least
	// this, and a fresh one is issued once it falls below. Because the fresh
	// lease is issued before the old one is worth less than the threshold, a
	// sibling that fetches at ANY moment caches at least this much authority —
	// which is what lets a partition that starts just after a fetch still be
	// survived for half a day. Reusing a lease to its end would let a sibling
	// fetch seconds of write (Codex review on #805).
	//
	// Half the TTL balances the two costs: a higher threshold re-issues more
	// often (one row and one event per device per sibling per TTL−threshold),
	// a lower one shortens the outage a cached lease is guaranteed to cover.
	DeviceWriteRefreshThreshold = DeviceWriteTTL / 2

	// DeviceWriteReissueSlack is how far above DeviceWriteRefreshThreshold a
	// lease is re-issued. The issuer runs on a beat, so a lease served between
	// two passes is up to one beat older than the last pass saw (Codex review
	// on #808). Re-issuing at threshold + slack keeps every served lease at or
	// above the threshold as long as the issuer runs at least once per slack;
	// the controller's beat is pinned well inside it (leasebeat_test.go).
	DeviceWriteReissueSlack = 15 * time.Minute
)

// DevicePrincipal is the lease principal for a device key, in the canonical
// form the management grant stores and the auth path compares.
func DevicePrincipal(deviceKey string) string { return DevicePrincipalPrefix + deviceKey }

// SiteResource is the lease resource for the site whose peer key this is.
func SiteResource(peerKey string) string { return SiteResourcePrefix + peerKey }

// IsDeviceWriteLease reports whether a lease is a device write lease: a device
// principal, a site resource and the write capability. These are the only
// leases a sibling is served (ActiveLeaseTokens); every other kind — a guest
// lease above all (ADR-0094) — is node-local.
func IsDeviceWriteLease(l Lease) bool {
	return strings.HasPrefix(l.Principal, DevicePrincipalPrefix) && len(l.Principal) > len(DevicePrincipalPrefix) &&
		strings.HasPrefix(l.Resource, SiteResourcePrefix) && len(l.Resource) > len(SiteResourcePrefix) &&
		slices.Contains(l.Capabilities, grant.CapabilityWrite)
}

// WriteDevices lists the device keys this node has authorised for write
// (ADR-0065's management grants). The controller adapts the catalog to it.
type WriteDevices interface {
	WriteDeviceKeys(ctx context.Context) ([]string, error)
}

// DeviceWriteIssuer converges this peer's device write leases on its
// management grants and its siblings. It is idempotent: a pass over a
// converged state writes nothing, so a pass that is re-run, interrupted or
// run after a restart is safe.
type DeviceWriteIssuer struct {
	store    *Store
	devices  WriteDevices
	siblings SiblingKeys
}

// NewDeviceWriteIssuer builds an issuer over a signing store. siblings is the
// set of sites to issue for; nil means none, which is a single-site node.
func NewDeviceWriteIssuer(store *Store, devices WriteDevices, siblings SiblingKeys) (*DeviceWriteIssuer, error) {
	if store == nil {
		return nil, errors.New("leases: a lease store is required to issue device write leases")
	}
	if len(store.signer) == 0 {
		return nil, ErrNoSigner
	}
	if devices == nil {
		return nil, errors.New("leases: a source of write-authorised devices is required")
	}
	return &DeviceWriteIssuer{store: store, devices: devices, siblings: siblings}, nil
}

// DeviceWriteResult counts what one pass did.
type DeviceWriteResult struct {
	// Issued is the leases minted this pass: a new grant, a new sibling, or a
	// live lease whose remaining life fell below the refresh threshold.
	Issued int
	// Reused is the (device, site) pairs a live lease already covered.
	Reused int
	// Revoked is the leases revoked because their grant was deleted or their
	// site left membership.
	Revoked int
}

type devicePair struct{ principal, resource string }

// Reconcile runs one pass, on the store's clock: for every write-authorised
// device and every sibling it keeps one live write lease with at least
// DeviceWriteRefreshThreshold left, and it revokes every device write lease
// this peer issued that no longer has a grant or a sibling behind it.
//
// A failure to read the grants or the siblings stops the pass before it
// touches anything. Reading "no grants" from an error would revoke every
// device's write at every sibling.
func (i *DeviceWriteIssuer) Reconcile(ctx context.Context) (DeviceWriteResult, error) {
	var res DeviceWriteResult
	devices, err := i.devices.WriteDeviceKeys(ctx)
	if err != nil {
		return res, fmt.Errorf("leases: listing write-authorised devices: %w", err)
	}
	var sites []string
	if i.siblings != nil {
		keys, err := i.siblings.PeerKeys(ctx)
		if err != nil {
			return res, fmt.Errorf("leases: listing sibling sites: %w", err)
		}
		for key := range keys {
			// A lease for this peer's own site would lift nothing: this node
			// authorises its own writes from the grant itself.
			if key != i.store.issuerID {
				sites = append(sites, key)
			}
		}
	}
	slices.Sort(sites)

	desired := map[devicePair]bool{}
	for _, d := range devices {
		if d == "" {
			continue
		}
		for _, s := range sites {
			desired[devicePair{DevicePrincipal(d), SiteResource(s)}] = true
		}
	}

	now := i.store.clock.Now().UTC()
	active, err := i.store.ActiveLeases(ctx, now)
	if err != nil {
		return res, err
	}
	// freshest is the latest-expiring live lease per pair: the one a refresh
	// decision is made on. An older one for the same pair stays live to its
	// expiry, because a sibling may have cached it.
	freshest := map[devicePair]time.Time{}
	for _, l := range active {
		if l.Issuer != i.store.issuerID || !IsDeviceWriteLease(l) {
			continue
		}
		p := devicePair{l.Principal, l.Resource}
		if !desired[p] {
			if _, err := i.store.Revoke(ctx, l.ID); err != nil {
				return res, err
			}
			res.Revoked++
			continue
		}
		if l.ExpiresAt.After(freshest[p]) {
			freshest[p] = l.ExpiresAt
		}
	}

	pairs := make([]devicePair, 0, len(desired))
	for p := range desired {
		pairs = append(pairs, p)
	}
	slices.SortFunc(pairs, func(a, b devicePair) int {
		if c := strings.Compare(a.principal, b.principal); c != 0 {
			return c
		}
		return strings.Compare(a.resource, b.resource)
	})
	for _, p := range pairs {
		if exp, ok := freshest[p]; ok && exp.Sub(now) >= DeviceWriteRefreshThreshold+DeviceWriteReissueSlack {
			res.Reused++
			continue
		}
		if _, err := i.store.Issue(ctx, p.principal, p.resource, []grant.Capability{grant.CapabilityWrite}, DeviceWriteTTL); err != nil {
			return res, err
		}
		res.Issued++
	}
	return res, nil
}
