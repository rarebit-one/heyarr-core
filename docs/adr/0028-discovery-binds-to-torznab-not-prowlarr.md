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

## What the client actually met, once it was built

This decision was made before anything spoke the protocol. The client landed
against a corpus captured from **two** servers — Jackett and Prowlarr, both
fronting the same public tracker — and the two disagree in ways that decide
whether it works. Recorded here because the argument above was a prediction,
and these are the measurements.

| | invalid API key | unsupported function | rss version |
|---|---|---|---|
| Jackett | HTTP **200**, `<error code="100">` | HTTP **200**, `<error code="201">` | 2.0 |
| Prowlarr | HTTP **401**, **empty body** | HTTP **400**, `<error code="202">` | 1.0 |

An error document therefore arrives with 200 *and* with 400, and an error also
arrives with no document at all. A client that gates parsing on a 2xx misses
the first; one that trusts the status line misses the second. The consequence
of getting it wrong is not a crash — it is a wrong API key reported forever as
"no releases found".

They also differ in attribute vocabulary: Prowlarr emits `tag`, `genre` and
`grabs` where Jackett emits `magneturl`, and Prowlarr emits
`genre` with an **empty value** on every item, which must count as absent
rather than as a determined empty string.

**A single corpus would not have shown any of this**, and the client would have
been shaped to whichever server it saw first while appearing to be
protocol-bound. That is the concrete form of this ADR's claim, and it is the
reason the corpus is organised by server under one protocol.

The same lesson arrived in the tooling rather than the client, which is worth
recording because it was nearly expensive: the capture script's redactor
matched an API key as `[0-9a-fA-F]{32}`, the *arr stack's shape. Jackett issues
a 32-character *alphanumeric* key, and a live one survived redaction intact in
a measured test — as it did the corpus scanner, which had the same assumption.
Both were fixed before any capture was committed. A guard shaped to one
product is not a guard.

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
