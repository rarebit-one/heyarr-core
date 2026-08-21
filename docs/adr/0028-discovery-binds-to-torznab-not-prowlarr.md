# 0028. Discovery binds to Torznab, the protocol, not Prowlarr, the product

**Status:** Accepted
**Date:** 2026-08-21

## Context

Spec §1 names Prowlarr as the delegated specialist for discovery, alongside
Transmission for transfer and FFmpeg for audiovisual work. §59 centralises
provider configuration; §60 keeps centralised provider management among the
lessons retained from *arr.

Milestone 3 makes that concrete, and the question that surfaced while scoping
it was whether Heyarr should talk to trackers **directly** and drop the
dependency entirely.

It should not, and the reason is where the cost actually sits. Prowlarr's value
is not its code — it is several hundred per-tracker definitions that break
continually as trackers change their markup, authentication and rate limits,
maintained by a community that does nothing else. Adopting that is adopting a
maintenance treadmill that would eat the project.

It would also make the testing problem **worse** rather than better: talking to
trackers directly is no more reproducible than talking to Prowlarr, and it
would bring tracker credentials inside a public AGPL repository's blast radius.

So the delegation stands. The remaining question is what Heyarr binds to.

## Decision

**Heyarr speaks Torznab. Prowlarr is one implementation of it.**

The indexer provider talks the documented Torznab interface — `t=caps` for
capabilities, `t=search` / `t=tvsearch` / `t=movie` for queries, an `apikey`
parameter, and a fixed XML result schema — rather than Prowlarr's own
application API.

Nothing in the provider is named for a product. A Torznab endpoint is a Torznab
endpoint: Prowlarr's, Jackett's, or a tracker exposing one natively.

## Consequences

**Prowlarr stops being a dependency and becomes an instance.** An operator
running Jackett, or several indexers directly, gets the same integration. The
registry holds endpoints, not product names.

**The contract is far more stable, which matters more here than anywhere
else.** A documented protocol schema moves slowly; a product's JSON API moves
at its release cadence. ADR-0026 makes recorded fixtures the *primary* and only
test strategy for an indexer — a corpus that is the only thing the client will
ever see. Binding that corpus to a protocol rather than to a product's current
version is the difference between captures that stay valid and captures that
need recapturing after every upgrade, from an instance nobody may still have.

**`t=caps` is a real capability handshake**, landing on the pattern ADR-0025
already establishes: presence checked at startup, compatibility checked at
connect. It is the same shape as reading Transmission's `rpc-version` — the
indexer states what it can search for, and one that cannot serve the content
type being wanted is reported as such rather than queried and found wanting.

**Heyarr does the fan-out, and that is a gain rather than a cost.** Torznab is
per-indexer, so the cross-indexer search Prowlarr would otherwise perform
becomes the provider registry's routing job. That is where it belongs: results
reach §63's evaluator with their **provenance intact** — which indexer offered
which release — instead of pre-merged by something else. "Why did it choose
that release" is only answerable if the answer can include where it came from.

**One capability is lost and it is not missed.** Prowlarr's own API can drive
configuration — adding indexers, pushing settings — and Torznab cannot. Heyarr
has no business configuring somebody's indexer manager; §61 lists duplicated
integrations among the things to avoid, and a Heyarr that configured Prowlarr
would be a second application managing the same state.

**XML, in a codebase that is otherwise JSON throughout.** Accepted knowingly.
The alternative is a product-shaped JSON API, and a stable schema in an
unfashionable format is worth more than a moving one in a familiar one.

## What would make us revisit

A Torznab successor with real adoption. The protocol is old and its Newznab
inheritance shows; if the ecosystem moves, this decision moves with it — the
point is to bind to whatever the interoperable layer is, not to Torznab
specifically.

An indexer manager whose useful capability is genuinely not expressible in
Torznab — a search dimension the protocol cannot carry — where the choice would
be between a product-specific integration and doing without.

Evidence that per-indexer fan-out is materially worse than delegating the
merge: rate limiting across many indexers, say, turning out to be something
Heyarr handles badly and Prowlarr handles well.
