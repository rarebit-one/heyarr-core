# 0094. A guest on a trusted network watches without an identity

**Status:** Proposed (2026-09-15)
**Date:** 2026-09-15
**Milestone:** M7 — Access leases (§54, builds on #285); clients (heyarr-kmp)

## Context

heyarr's identity is voidbind device enrolment (ADR-0022): a client authenticates
as an enrolled Device, or as a scoped bearer token (ADR-0011), and personal state
— playlists, resume positions, ratings, history, follows — is per-principal and
encrypted controller-side (§72). MCP holds no key for it; there is no anonymous
view of a person's library because there is no anonymous person.

That gate is correct for anything **personal** or **mutating**. It is too heavy
for the single most common thing a home server is for: a family member on the
house network wanting to *watch*. Today dad must enrol a device before he can
press play on the living-room TV — an identity ceremony to consume content that
is already sitting on a box on the same LAN.

And the trusted-network perimeter that would justify skipping that ceremony
already exists and is already enforced. heyarr binds to each site's LAN
reservation, never `0.0.0.0`, and the deployment's `IPAddressAllow` admits only
the site LANs and the WireGuard estate client nets. ADR-0079 already lets a
renderer's origin lean on network position (a plain-HTTP capability URL a TV
fetches without TLS). The network is a trust boundary; it is simply not yet a
principal.

## Decision

Introduce a **guest** access tier. A request arriving from an allow-listed
trusted-network address and carrying no device credential is served as an
anonymous `guest` principal whose capabilities are **read and play only** — browse
the library, resolve and stream content, fetch subtitles — and nothing else. A
guest cannot create a want or acquire (that spends the operator's bandwidth and
disk), cannot follow a source, cannot rate, and has no personal state, because it
has no identity to key it to.

Guest is a **capability, not a bypass.** It is minted the way every other access
is: as a lease (§54, M7-05 / #285) — `principal=guest`, `resource=<library>`,
`capabilities=[browse, play, subtitle]`, short expiry, scoped by source address.
The M7 lease store is the natural and intended home; guest is that store's first
non-device principal, not a second, parallel auth path bolted beside it. This is a
reason to land #285, not new machinery.

Enrolment (ADR-0022, cruciform) is unchanged and becomes **optional**. A guest who
wants their place kept, their playlists and ratings, or the ability to want and
follow, signs in and upgrades from the guest lease to a device principal.
*"Sign in to save"* replaces *"sign in to watch."*

### Discovery — a client finds a server without being told one

A guest is useless if it cannot find the node, so two mechanisms, in order:

1. On the estate, split-horizon DNS already resolves `heyarr.thesim.family` to the
   nearest healthy box (AdGuard). A client defaults to that name and is zero-config
   today, and — via nearest-server — lands on the closest of an active-active pair.
2. For any network, the node advertises `_heyarr._tcp` over mDNS / DNS-SD. heyarr
   already speaks SSDP to *find* renderers (ADR-0079 area); this is the client-
   facing sibling — the node *announcing itself* to clients. A client discovers,
   else falls back to the DNS name, else to manual entry.

### The trust boundary is the network, stated plainly

"Trusted network" means the site LANs and the WireGuard estate client nets — the
same ranges `IPAddressAllow` already admits, and no wider. A request from the raw
internet (no VPN) is **not** a guest; it must enrol. Guest never widens the
perimeter that exists; it only stops re-authenticating *inside* it. An operator
who wants identity everywhere disables the tier (a trusted-net allow-list of zero).

## Consequences

- The common case — watch, on the home network — needs no enrolment. Family
  devices are plug-and-play; a fresh phone on the estate can browse and play the
  moment it is on the VPN. This is the point.
- Personal features stay gated exactly as before: a guest cannot reach them, so
  nothing personal is exposed to an anonymous principal.
- Acquisition, following and rating stay behind identity; a guest cannot spend the
  operator's resources or mutate the catalogue.
- Clients (heyarr-kmp desktop + android, any future iOS) gain a "Browse as guest"
  default and auto-discovery. The desktop client, today bearer-only, gets guest as
  its default and device-auth as the optional upgrade.

## What would make us revisit

- If personal state ever needs to reach a guest (a shared household profile, say),
  guest is no longer purely anonymous and this decision reopens.
- If the LAN stops being a sufficient trust boundary (an untrusted device on the
  network), guest needs a weaker capability set or an explicit challenge.
- If the tier proves a footgun for an exposed deployment, the default flips to off.
