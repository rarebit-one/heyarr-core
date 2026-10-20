# 0119. A site publishes the release it chose, and its siblings acquire it themselves

**Status:** Proposed
**Date:** 2026-10-20
**Follows:** ADR-0038 (desire is per site), ADR-0042 (a peer does not join a
swarm to replicate), ADR-0060 (a direct release), ADR-0073 (the catalog
converges by an op-log), ADR-0076 (heyarr resolves the `.torrent`), ADR-0090
(each site has its own indexers)

## Context

Two sites that both want the public-media library whole have so far got there
by copying bytes between them out of band: a filesystem-level send of the
whole media dataset, then a rescan at the other site (#704). That transport is
wrong for this content, for three reasons that do not go away with a faster
link:

- The site-to-site link is the narrowest link either site has. Each site's own
  internet connection is an order of magnitude wider, and its VPN egress to
  the swarm never touches the link between the sites.
- The download client's scratch directory has to live on the same dataset as
  the content store, because ingest hardlinks finished files into it
  (ADR-0014). A dataset-level send therefore carries every in-flight transfer's
  piece churn as well as the finished bytes. One day's batch of large
  acquisitions became an increment the link needed most of a day to carry.
- The content is reproducible. What the library holds is, by construction,
  what a release carrying a known infohash produced. The origin already
  persists that identity (`release_candidates.candidate_id` is
  `infohash:<hex>` when the indexer supplies it; `acquisitions.external_id`
  is the client's infohash). The sibling lacks only the *intent*: wants are
  per site (ADR-0038) and nothing tells a sibling which release was chosen.

The personal profile (ADR-0107) already solves its version of this with
placement (ADR-0114): a site asks its sibling what to hold and pulls the bytes
over the peer surface. That is the right shape for private, irreproducible
bytes. For public media the bytes need not cross the link at all.

## Decision

**A site publishes the release it chose. A sibling that has opted in acquires
that same release through its own download client, from the swarm, and
converges on the same work.**

- **The statement is a catalog op.** A new kind, `acquire`, rides the
  ADR-0073 op-log, keyed like every catalog op on the work's cross-site-stable
  `(content_type, work_key)`. It carries what a sibling needs to acquire the
  same bytes and nothing it must not: the scope within the work (the work, an
  edition, or an item), the aspect (primary, or a subtitle language), the
  quality profile's name, the infohash, the release title and size, the
  work's own title and year (so a sibling that has never seen the work can
  create it under the same key), when it was selected, and the origin's site
  name. It carries announce URLs only when
  they are credential-free; a release from a private indexer is published with
  `private: true` and no trackers.
- **Credentials never ride an op.** The indexer URL the origin fetched the
  `.torrent` from, the `.torrent` bytes, and anything a private tracker put in
  them stay local (`release_candidates.source` is a secret today and remains
  one, ADR-0076). An op whose tracker list carries userinfo or a query string
  is malformed.
- **The origin publishes at selection.** When a want reaches SELECTED, whether
  the scorer chose the release or a person named it (ADR-0060), the origin
  signs an `acquire` op. The op-log's existing sync carries it.
- **A sibling acquires only with its own means.** A sibling with fan-out
  enabled evaluates the op by ensuring the work exists under the same
  `work_key`, ensuring a want for that scope and aspect under the named profile
  (falling back to the content type's default profile, and saying so), and
  resolving a source it can use: a magnet from the infohash for a public
  release, or a hit from its own indexers whose candidate id is the same
  infohash for a private one. It records that as a direct release (ADR-0060)
  and runs the ordinary grab. If it can resolve no source, the want stays idle
  with the reason `no_site_source`. **It never grabs a different release for a
  fan-out want:** the point is byte-identical copies.
- **Convergence follows from ingest.** An acquired asset is keyed on its
  want's work (`applyWorkOverride`), so both sites land the same bytes under the
  same `work_key`, and the edition or item the scope names. The path-key
  divergence a rescan would produce (#704) does not arise.
- **The newest selection wins.** For one `(work_key, scope, aspect)`, the op
  with the latest `selected_at` is the effective one; ties fall to the hash.
  An upgrade is a newer op. A tombstone on the work (ADR-0073) suppresses its
  acquisitions.
- **ADR-0042 stands.** heyarr still does not speak BitTorrent. It tells the
  site's download client what to fetch, and that client keeps seeding under
  its own policy, so with a reachable port at each site the first site to
  finish seeds the other directly, over each site's own connection.

## Consequences

- One new op kind in `catalogop`; no new table and no new peer route. The
  op-log sync already carries the whole set.
- A sibling's library converges on new acquisitions without any bytes crossing
  the site-to-site link. The dataset-level send of the media store can stop;
  the state database keeps whatever backup discipline it had.
- Two clients may be in the same swarm under one tracker account, from two
  addresses. For public indexers this is nothing. For a private indexer whose
  rules forbid it, the sibling must have its own account, which is why a
  private release is published without trackers and resolved through the
  sibling's own indexers.
- A release the swarm no longer carries is not acquired at the sibling. The op
  records the gap (`no_site_source`); pulling finished bytes from the origin
  over the peer surface is a separate decision (see below).
- Fan-out is a per-site switch. A site that does not enable it sees the ops
  and ignores them, exactly as a site that does not list a sibling ignores
  placement (ADR-0114).

## Alternatives rejected

- **Replicate wants.** A want is per site by design (ADR-0038) and names a
  profile that is per site too. Replicating wants would make both sites search
  and let them choose different releases, which defeats convergence. The op
  publishes the *choice*, not the desire.
- **Extend placement (ADR-0114) to the media profile.** Placement moves bytes
  over the peer surface, which is the link this decision avoids, and it needs
  a local blob row the sibling does not have until it has the bytes. It remains
  the right fallback for a dead swarm.
- **Carry the `.torrent` in the op.** It would carry a private tracker's
  credential across sites and into a signed, content-addressed log.
- **Let the sibling run its own search for the work.** It may pick a different
  release, and the library would hold two files for one work.

## What would make us revisit

- A sibling that regularly cannot resolve a source (a thin swarm, a private
  indexer with one account). Then the origin should publish the blob digest
  after ingest and the sibling should pull it over the peer surface, the
  placement way (#704, #779).
- More than two sites, or sites that should hold different subsets. The op is
  broadcast; a subset policy would need a placement class.
- An indexer ecosystem where infohash identity is routinely unavailable
  (title-only indexers, ADR-0091). Then the op has nothing to pin.
