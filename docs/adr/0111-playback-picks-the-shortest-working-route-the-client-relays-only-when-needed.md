# 0111. Playback picks the shortest working route; the client relays only when needed

**Status:** Proposed
**Date:** 2026-10-14
**Builds on:** [ADR-0024](0024-one-consumption-session-model.md) (one consumption session), [ADR-0030](0030-replication-is-a-destination-pull.md) (replication is a destination pull), [ADR-0040](0040-a-renderer-fetches-bytes-with-a-capability-not-a-credential.md) (capability URL trust root), [ADR-0069](0069-a-client-that-cannot-decode-the-source-gets-an-on-the-fly-repackage.md) (repackaged stream), [ADR-0079](0079-renderers-get-a-plain-http-origin-of-their-own.md) (plain-HTTP render origin), [ADR-0094](0094-a-guest-on-a-trusted-network-watches-without-an-identity.md) (trusted networks)

## Context

SSDP discovery is multicast on the controller's local segment. It does not
cross an inter-site link. A user at Site B talking to Site A's controller sees
only Site A's renderers — not the projector sitting on Site B's LAN. Casting
from the wrong site either silently fails or crosses the inter-site link twice:
once for control, once for every byte the renderer fetches. Neither is
acceptable for 4K content.

Away from home the situation is worse. A hotel network isolates clients from
the controller entirely; the controller cannot reach the television at all, and
the client is the only device with a route to both.

§68's playback planner already chooses between DIRECT, REMUX and TRANSCODE for
a given asset and device, but its inputs are local: it has no notion of where
the client is, whether the controller can reach the renderer's control URL, or
which route avoids an unnecessary hop. §35 names failure domains (Site A,
Site B), but the planner does not use them when choosing where bytes come from.

Three gaps need closing: renderer discovery, client location, and route
selection.

## Decision

**The planner grows a route dimension — R1, R2, R3 — that places the client in
the path only when no controller can reach the renderer. Discovery is extended
so clients report renderers they see, and the planner picks the shortest route
for which every element is reachable.**

### 1. Two discoverers — controllers and clients

Controllers continue discovering renderers on their local segment via SSDP.
Clients (desktop and mobile apps) also discover renderers on whatever network
they are on — SSDP now, Chromecast/mDNS later — and **report** them to the
controller with the renderer's control and description URL. A reported renderer
is tagged with its origin (which discoverer saw it), is ephemeral (expires after
a short TTL), and is keyed by UDN like the existing controller-side cache
(`internal/api/resources/renderers.go`).

A client-reported renderer grants nothing beyond what the reporting client
could already play (ADR-0040's capability URL is still the trust root). The
controller only probes and drives renderers on networks it can route to (see
reachability below).

### 2. Client location

The controller derives the client's **site** from the request's source address
alone, matched against an optional `sites:` map of `name → []CIDR` whose names
are the `peer.site` labels (§35). A single-site deployment configures nothing:
every estate address is this controller's site. An address matched by no entry
is `"away"`. There is **no client-supplied location hint**: where a client is,
is what the network says, so a client cannot talk itself into another site's
renderers.

`http.guest.trusted_nets` (ADR-0094) is deliberately not reused: it is a guest
trust allow-list with no site names, and conflating "may browse without an
identity" with "is at Site B" would couple two decisions that change for
different reasons.

Renderer lists returned to a client are sorted: the client's own site first,
then other sites, then renderers with no site label.

### 3. Route planner — R1, R2, R3

The planner is a **pure, testable function** that extends §68's
DIRECT/REMUX/TRANSCODE output with a route and controlling party. Inputs:

- the renderer (UDN, site, who reported it, its control and description URL)
- client location (site, `"away"`)
- which controllers hold a verified replica of the blob (§30, ADR-0030)
- reachability: whether a controller can reach the renderer's control URL
  (a cheap HTTP GET of its description document, cached briefly, not a
  per-request probe)
- link capacity facts (operator-declared or passively measured inter-site
  capacity, in Mbit/s, less any share the operator reserves for background
  transfer such as replication) and the asset's declared bitrate

Three ordered candidates:

**R1 — local-direct.** A controller on the same site as the renderer holds the
blob and can reach the renderer's control URL. The controller drives it and
serves bytes from its `rendererBaseURL` (ADR-0079). One hop, no inter-site
traffic. Preferred unconditionally when available.

**R2 — cross-site direct.** No controller on the renderer's site holds the blob
(or the renderer is on a site the planner has no local controller for), but a
controller elsewhere can reach the renderer's control URL. The controller drives
it; bytes come from the nearest blob holder whose render address falls inside a
known site network and is therefore reachable by the renderer. Gated on:
`asset_bitrate_kbps ≤ declared_capacity_kbps` for the inter-site link; if not
met, the planner falls to the existing REMUX/TRANSCODE branch (ADR-0069) or
returns an explanation with a stable reason code rather than a silent failure.

**R3 — client relay.** No controller can reach the renderer's control URL, but
the client that reported it can, and the client can still reach a controller. The client drives the renderer and re-serves the bytes from a
client-local HTTP origin, pulling from the controller over the client's existing
authenticated path. The plan carries `client_must_stay_connected: true`.

For R1 and R2 the plan carries `client_must_stay_connected: false`: the client
brokered the session and may sleep or disconnect. Progress continues server-side
(§67 ConsumptionSession, polled from the controller's own renderer driver).

If no candidate is viable, the plan returns an error with a reason code rather
than choosing a wrong route silently.

### 4. Plan output additions

The plan output (extending §68) gains:

| Field | Values |
|---|---|
| `route` | `r1`, `r2`, `r3` |
| `controlling_party` | a controller node ID, or `"client"` for R3 |
| `byte_origin` | the render base URL the renderer will fetch from |
| `client_must_stay_connected` | bool |
| `reason_code` | a stable code; set on refusals, informational on success |
| `reason_human` | a human-readable explanation |

Clients surface `client_must_stay_connected` to the user ("you can close the
app" vs. "keep this device on and connected").

### 5. Client obligations for R3

These are requirements on client implementations; implementation is out of
scope for this ADR:

- **Keep-alive**: a mobile client opens a foreground service with a wake lock;
  a desktop client acquires a sleep inhibition lease for the session duration.
- **Local relay**: the client serves the same capability semantics as a
  controller render origin — expiring, single-asset, fetched on demand from
  the controller.
- **Visible state**: the client shows a "relaying" indicator while R3 is active.
- **Clear failure**: if the local network isolates clients from each other
  (common on a hotel Wi-Fi), the client surfaces a specific error rather than
  hanging.

### 6. Security

- Capability URLs (ADR-0040) remain the trust root. A renderer fetching from
  the client relay receives the same expiring, single-blob capability it would
  receive from a controller.
- The bearer token used to authenticate the client's pull from the controller
  is never forwarded to the renderer.
- **No server-side request forgery.** A client-reported description or control
  URL makes the controller issue HTTP requests, so the controller fetches one
  only when its host is a literal address inside a configured site network,
  over plain HTTP to the renderer's own port, and only the description and
  AV-transport paths that the description itself names — never a redirect, never
  a hostname that resolves elsewhere. A reported renderer outside every site
  network is listed for its reporter and eligible for R3 only; the controller
  never fetches it.

### 7. Phasing

**Phase 1:** Client-assisted discovery + client location inference + R1 and R2.
No new replication is required: R2 streams across sites without requiring a
local replica at the renderer's site.

**Phase 2:** R3 client relay.

Peered renderer federation — peers sharing their discovered renderers over the
peer replication surface — is the natural next step once multi-peer enrolment
is stable. It is noted here but not required.

## Consequences

- §68's planner output is extended. Clients that do not consume the new fields
  continue to work; `client_must_stay_connected: false` is the safe default for
  R1/R2, which covers existing deployments.
- The reachability probe (controller → renderer description URL) is cached but
  adds a round-trip on cache miss. The cache TTL must be tuned against the cost
  of a stale entry (attempting to drive an unreachable renderer).
- R2 introduces inter-site streaming. Bitrate gating mitigates overload, but an
  operator with a constrained inter-site link should set a declared capacity to
  prevent R2 from being chosen for high-bitrate assets.
- The `sites:` map is needed only by a multi-site operator; a single-site
  deployment's behaviour is unchanged.
- Client-assisted discovery makes the renderer list at the controller
  authoritative across all sites a client can see, but only while at least one
  client is connected. A headless cast (no client present) sees only the
  controller's locally discovered renderers.

## What would make us revisit this

- A renderer protocol that requires **push from the client** (AirPlay-like
  direct push rather than renderer-pull) would make R3's relay architecture
  inapplicable; that protocol would need its own route entry.
- If inter-site capacity reliably makes R2 unviable in practice, R2 becomes
  dead code. Consider removing it and treating cross-site as always R3 (or
  requiring a local replica via replication policy before cast is attempted
  cross-site).
- If the reachability probe proves too expensive or too unreliable, replace it
  with operator-declared topology (explicit "controller A can reach site B
  renderers") rather than live probing.

## Alternatives rejected

- **Always relay through the client.** Keeps the app awake and adds two extra
  hops (client → controller → renderer) for a session the controller could
  drive directly. Rejected in favour of keeping the client out of the path when
  it is not needed.
- **SSDP relay or multicast forwarding between sites.** Brittle: multicast does
  not traverse most inter-site links without explicit configuration. Leaks
  discovery traffic across trust boundaries. Does nothing for a hotel network
  or any network the controller cannot reach. Rejected in favour of
  client-reported renderers as a universal mechanism.
- **Controller-only discovery with a manual "add renderer by URL".** Handles a
  single renderer on a known address but requires the user to know and enter
  the URL. Does not generalise to a new network where the user has no prior
  knowledge, and forces manual maintenance as renderer addresses change.
  Rejected in favour of the client reporting what it discovers automatically.
