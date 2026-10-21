# 0114. A site asks its sibling what to hold, and pulls it

**Status:** Proposed
**Date:** 2026-10-18
**Milestone:** M9 — Encrypted personal state (two-site vault durability, #662)

## Context

A device pins a vault blob at the site it talks to (ADR-0096). A pin naming the
**other** site was planned as a `replicate_blob` in this site's queue. That job
can never run: replication is a destination pull (ADR-0030). It was refused, and
then replanned every cycle. The other site never learned the blob existed, so
every vault byte lived only where it was uploaded.

Three facts constrain the fix:

- **Peer ids are node-local** (ADR-0012), so no message between sites can name
  a peer by id.
- **Each node is authoritative for its own site**, and may tell another node
  only about itself (ADR-0033, ADR-0038).
- **Reachability belongs to a pair** (ADR-0038). Placement discovery must
  travel in the same direction as the fetch, or one-way-reachable sites
  deadlock.

## Decision

**The destination asks.** Each personal-profile site (ADR-0107) periodically
asks each sibling it lists which blobs the sibling wants kept at the asking site.
It records the answer as its own desire and pulls in its own queue, verifying
the digest as ever. A site never plans a transfer whose destination is another
peer.

- **A set, replaced per hash range.** The answer is the whole offered set inside
  one range of the hash space, and the asker replaces what it took from that
  source in the range. A withdrawal is simply absence, and a lost exchange
  changes nothing. A digest of what the asker already holds lets an unchanged
  range cost one round trip.
- **What a site offers.**
  - Blobs a device pinned to the asker.
  - When the asker is a configured sibling, every blob pinned at this site (an
    upload's self-pin). This is **node policy**: a departure from ADR-0096's
    device-only placement, bounded to configured siblings. The server still
    learns no blob→space mapping, which is why the policy is per site rather
    than per space.
  - **Never a placement it took from another site.** Otherwise two sites
    would keep each other's pins alive after the origin let go.
- **Wanted is not held.** Each offered blob says whether the source holds its
  bytes now. A source that lost its copy keeps offering the blob. Dropping it
  would read as a withdrawal at exactly the moment the asker's copy may be the
  only good one.
- **Claims are about the claimant, and complete.** The question carries the
  asker's claim about its **whole** disk in the range. The source records what
  the claim lists as present, and marks any of the asker's present replicas the
  claim omits as missing. Both sides' copies, and `sites_durable` for a client
  waiting until a blob is safe at two sites, stay true through withdrawals and
  losses.
- **Both sites opt in, for every cross-site placement.** A site asks only the
  siblings it lists, and offers its node-policy set only to them. A device pin
  naming a peer that does not list this site is therefore never acted on, and
  that is the stated prerequisite. Unlisting a sibling releases what was taken
  from it, so the bytes then go by GC's grace window (ADR-0018).

## Consequences

- One new peer route (`POST /peer/v1/placements`) and one table of placements
  taken from siblings. GC counts a taken placement as a reference. The transfer
  plane is unchanged.
- The source's queue no longer accumulates jobs it cannot run.
- A withdrawn copy at the destination is reclaimed only while the source's
  claim about its own copy is fresh. Otherwise ADR-0018's last-copy rule spares
  it, which is safe but slow to free space (#779).
- **Release intent, personal profile only (amended 2026-10-21, #779).** When a
  vault blob's last pin is removed, or its source withdraws the placement
  (`blob_releases`), the personal profile's GC may reclaim the last copy once
  the grace window has passed, recording the basis `released`. On a two-site
  fabric the last-copy rule would otherwise spare it at one site forever. The
  media profile keeps ADR-0018's rule; `gc --honour-releases` opts one run in,
  and library media is never released.

## Alternatives rejected

- **The source pushes its placements.** This runs against the fetch direction
  and deadlocks one-way-reachable pairs (ADR-0038).
- **Clients pin every upload to the sibling.** Every writer would have to repeat
  it, using node-local ids, and backfilling would take one request per blob.
  Durability across sites belongs to the sites.
- **Per-pin deltas.** A lost delete would leave a pin forever, whereas a range
  replace converges.

## What would make us revisit

- A space must live at one site only. The server cannot see spaces, so uploads
  would have to carry an opaque placement class.
- More than two sites, or sites that should not all hold everything.
- Range exchanges stop being cheap at vault sizes far beyond a household's.
