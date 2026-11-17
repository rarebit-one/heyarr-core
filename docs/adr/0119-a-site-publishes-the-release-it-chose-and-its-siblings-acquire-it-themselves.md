# 0119. A site publishes the release it chose, and its siblings acquire it themselves

**Status:** Accepted (2026-11-17, the owner's decision on #833)
**Date:** 2026-10-20
**Amended:** 2026-11-17. An asset a site holds without an infohash (a scan, an
HTTP, usenet or direct fetch) reaches the sibling by **peer-pull**, for
libraries both sites opt in: the origin publishes a `hold` op naming the blob,
and the sibling pulls the bytes over the peer surface and verifies them. Default
off. See *Amendment: a non-swarm asset is pulled from the origin, per library
(2026-11-17, #833)*. Passages changed in place are marked *(amended
2026-11-17)*.
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
  name. For a public release it carries the announce URLs the download client
  reports, kept only in the shape a public tracker uses (udp, http(s) or ws(s),
  a host, no userinfo, query or fragment, a path of nothing, `/` or
  `/announce`), so a sibling finds the swarm through its trackers rather than
  DHT alone, which is slow on a thin swarm. A release from a private indexer is
  published with `private: true` and no trackers.
- **Credentials never ride an op.** The indexer URL the origin fetched the
  `.torrent` from, the `.torrent` bytes, and anything a private tracker put in
  them stay local (`release_candidates.source` is a secret today and remains
  one, ADR-0076). An op whose tracker list carries userinfo or a query string
  is malformed.
- **The origin publishes when it grabs.** Every route to SELECTED (the
  scorer, a person's direct choice, a feed's enclosure: ADR-0060) ends in the
  one grab that hands the release to the download client, and that is where
  the origin signs the `acquire` op, once the client has confirmed the
  transfer's infohash. A transfer with no infohash (an HTTP enclosure, a
  usenet article) publishes nothing: there is no swarm for a sibling to join.
  The op is written before the want advances, so a failed write retries with
  the grab rather than being lost. The op-log's existing sync carries it.
- **The siblings are Partial Peers of each other.** A Full Peer holds everything
  (§19): enrolling the two sites as Full Peers would turn on blob convergence,
  control-plane backups and state replication between them, the byte traffic this
  decision exists to remove. So each site enrols the other in `partial` mode. The
  catalog-ops sync runs between Full and Partial Peers; every byte-moving loop
  stays Full-only, so the op-log is the only thing that crosses.
- **A sibling acquires only with its own means.** A sibling with fan-out
  enabled evaluates the op by ensuring the work exists under the same
  `work_key`, ensuring a want for that scope and aspect under the named profile
  (falling back to the content type's default profile, and saying so), and
  resolving a source it can use: a magnet from the infohash for a public
  release, or a hit from its own indexers whose candidate id is the same
  infohash for a private one. It records that as a direct release (ADR-0060)
  and runs the ordinary grab. If it can resolve no source, it creates nothing
  (an idle want would be searched by the site's own scorer, which could pick
  another release) and records `no_site_source` against the op, re-evaluated
  on every pass so a later means of resolving it acts on ops already received.
  **It never grabs a different release for a fan-out want:** the point is
  byte-identical copies.
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
  over the peer surface is a separate decision (see below). *(amended
  2026-11-17)* The amendment decides it for assets with no swarm at all, in
  opted-in libraries. A dead swarm for a release that has one is still not
  decided (#800).
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
  the right fallback for a dead swarm. *(amended 2026-11-17)* For an asset with
  no swarm, the amendment takes this shape after all, bounded to libraries both
  sites opt in, with an op standing in for the offer.
- **Carry the `.torrent` in the op.** It would carry a private tracker's
  credential across sites and into a signed, content-addressed log.
- **Let the sibling run its own search for the work.** It may pick a different
  release, and the library would hold two files for one work.

## What would make us revisit

- A sibling that regularly cannot resolve a source (a thin swarm, a private
  indexer with one account). Then the origin should publish the blob digest
  after ingest and the sibling should pull it over the peer surface, the
  placement way (#704, #779). *(amended 2026-11-17)* The `hold` op and the
  op-sourced blob row are that mechanism; extending them to swarm-acquired
  assets is #800's decision.
- More than two sites, or sites that should hold different subsets. The op is
  broadcast; a subset policy would need a placement class.
- An indexer ecosystem where infohash identity is routinely unavailable
  (title-only indexers, ADR-0091). Then the op has nothing to pin.

## Amendment: a non-swarm asset is pulled from the origin, per library (2026-11-17, #833)

The `acquire` op covers only what a swarm can reproduce. Two ways of holding an
asset publish nothing: a library scan (a `rom` library seeded by hand, ADR-0121
§3), and a transfer with no infohash (an HTTP enclosure, a usenet article, a
direct URL). For content that exists at one site only, the sibling has no source
of its own, so it never holds it. Moving those bytes over the site-to-site link
is what this record set out to avoid. For a small collection the owner accepts
the cost (decision on #833, 2026-11-17): **peer-pull, as a per-library opt-in.**

### The decisions

1. **The origin publishes a `hold` op.** It is a new op kind on the same
   ADR-0073 log, keyed on the work's `(content_type, work_key)`. It carries the
   scope (the asset's edition key), the blob's BLAKE3 digest and size, the
   file's path relative to its library root and its asset role, the work's title
   and year, the origin library's name and site, and when it was held. It
   carries no absolute path and nothing else about the origin's disk. There is
   one op per `(work, blob)`; the newest by issue time wins, ties fall to the
   hash. A hold is never withdrawn by another op. A tombstone on the work
   (ADR-0073) suppresses it, as it does an `acquire`.
2. **Only for an asset with no swarm.** The origin publishes a hold for an asset
   of an opted-in library unless an `acquire`, its own or a sibling's, names the
   asset's work, edition or item. Those reach the sibling through its own swarm,
   and the link stays unused. An asset this site pulled from a hold is never
   published back. Publishing is a periodic pass, not part of ingest,
   so a library opted in after its scan publishes what it already holds. The
   pass is idempotent: a `(work, blob)` this site already published is skipped.
3. **The opt-in is per library, at both sites.**
   `acquisition.peer_pull.libraries` lists library names in node config, and it
   is empty by default. At the origin the list decides what is published. At the
   sibling it decides what is pulled and where it lands: in the listed library
   of the op's content type, preferring one with the origin library's name. If
   several listed libraries share the content type and none has that name,
   nothing is pulled, and the sibling says why. Validation refuses the setting
   outside the media profile, and refuses duplicate or empty names. A name that
   matches no library is reported at startup and otherwise ignored. With the
   list empty, nothing is published, nothing is applied, and every existing path
   behaves exactly as before. The setting is independent of
   `acquisition.fan_out`.
4. **Who may pull, and what is trusted.** Any pinned member may already read any
   blob on the peer surface (ADR-0030); the amendment adds no route. A sibling
   acts on a hold only when the op verifies under its `by` key, which recording
   refuses otherwise, and when that key is a current member here. It pulls only
   from that signer. The op is the origin's claim about its own disk (ADR-0033),
   the same claim an ADR-0114 offer makes. The bytes are hashed on arrival
   against the op's digest and recorded present only if they match (invariant 1,
   §21). The op's size is a hint, and the verified length replaces it. Nothing
   else is trusted.
5. **An op can source a blob row (#800's prerequisite).** Applying a hold
   records a `blobs` row from the op's digest and size, and a `present` replica
   for the signing peer. This is metadata only, the way an ADR-0114 offer adopts
   one. `replicate_blob` then pulls it like any other gap. Before this, a media
   node with no local row for a blob refused the pull.
6. **The pulled asset lands on the origin's keys.** Once the bytes are present,
   the sibling records the asset through ingest's own recording, against the
   chosen library. The op's work key overrides what the path would derive, as an
   acquisition's want does (`applyWorkOverride`, #224), and its edition key
   overrides the derived one in the same way. The sibling's own rules supply the
   per-file facts: label, region, role. The bytes live in the content store, and
   the library's root on disk is not written, as for an acquired asset.
7. **Bounds on the link.**
   - **Size:** `acquisition.peer_pull.max_blob_bytes` caps the size of one blob,
     with a default of 4 GiB. A larger asset is not published, and a larger hold
     is not pulled.
   - **Concurrency:** pulls run in the existing `replicate_blob` lane, at two
     per node.
   - **Queue depth:** one pass queues a bounded number of new pulls.
   - **Crossing once:** a blob crosses once. A digest held here is recorded
     without a transfer, and a hold that is held is never pulled again.
   - **Retries:** a failed pull is not retried until an hour after it failed.

   There is no rate limit on the link. If one is needed, that is a reason to
   revisit (see below).
8. **Failure and retry.** Each hold op's outcome is recorded per op hash, with
   an event (invariant 7):
   - `own`: this site signed it. Final.
   - `held`: the asset is recorded here. Final, so deleting it at the sibling is
     respected and it is not pulled again.
   - `pulling`: the pull is queued or awaiting retry.
   - `no_library`: no listed library takes it, or the choice is ambiguous.
   - `too_large`: the blob is over the size cap.
   - `no_site_source`: the signer is not a member here.
   - `tombstoned`: the work is tombstoned here.

   Every outcome except `own` and `held` is re-evaluated on each pass. Bytes
   that fail verification are quarantined (ADR-0018) and retried after the
   hour. `get_replica_status` reports a blob's hold outcome beside its
   replicas.

### How it relates to #704 and #800

- **#704.** A pulled asset takes the origin's work and edition keys. The
  path-key divergence of an independently scanned sibling (#704) therefore does
  not arise in an opted-in library. Scanning a copy of the same files at the
  sibling as well would add a second, path-keyed asset of the same blob, so seed
  one way or the other.
- **#800.** The dead-swarm fallback would reuse decisions 1 and 5 for an asset
  that an `acquire` covers. That would move large public-media bytes across the
  link, so it is not decided here, and decision 2 keeps such assets out.

### Consequences

- The op-log gains a kind, and a node gains a table recording hold outcomes.
  There is no new peer route. The catalog-ops sync carries the op.
- Each hold cites the one published before it, so the log keeps one head and
  the next op signed there stays within its bound on cited heads.
- The whole log rides one catalog-sync body, capped at 8 MiB. The publisher
  stops short of that, so a very large library is published only in part,
  and says so. This is the first kind that can approach the cap.
- **An older build refuses a sync batch with an op kind it does not know.** That
  stops catalog sync for the pair, tombstones included. List a library only once
  both sites run a build that reads `hold`.
- Deleting an edition or asset at the origin publishes nothing (ADR-0073 has no
  op for it), so the sibling keeps its copy. Deleting the work tombstones it at
  both sites.

### What would make us revisit

- Collections large enough that the size cap and the retry spacing do not bound
  the link. That would call for a rate limit, or a schedule for when the link
  may carry pulls.
- A wish to pull a dead swarm's release this way (#800).
- More than two sites. A hold is broadcast, and each sibling pulls from the
  signer only.
