# 0090. A Prowlarr provider is a live indexer aggregator, not one torznab entry per indexer

**Status:** Accepted (2026-09-10)
**Date:** 2026-09-10
**Milestone:** M12 — Followed Sources / The Archive (Phase 6)

## Context

heyarr reaches an indexer through the `CapabilityIndexer` seam (§59, ADR-0025):
a provider `Entry{type: torznab, endpoint, api_key}` speaks Torznab and returns
releases. Prowlarr is consumed this way — but Prowlarr exposes **one Torznab
endpoint per indexer** (`/{indexerId}/api`), so today every indexer Prowlarr
manages is a **separate heyarr `Entry`** (the live config has `linuxtracker` at
`/1/api` and `internet-archive` at `/2/api`, each with the Prowlarr API key).

This is the friction behind "why not build Prowlarr into heyarr": the indexer set
lives in **two places**. Add an indexer in Prowlarr and heyarr does not know about
it until someone hand-writes a matching `torznab` Entry with the right `/{id}/api`
path; remove one in Prowlarr and heyarr keeps a dead Entry. Prowlarr already *is*
the aggregator — a single service that normalises hundreds of trackers behind one
API and one key — and heyarr flattens that back into N hand-maintained entries,
re-creating the bookkeeping Prowlarr exists to remove.

The alternative that keeps coming up — **absorb Prowlarr's featureset into
heyarr** (a Cardigann definition engine over the community definition repo) — was
weighed and rejected: it imports a GPLv3 licensing conflict with heyarr's closed
release model, a perpetual sync of a community-maintained definition catalog, and
the anti-bot (CloudFlare/FlareSolverr) surface — four costs to delete one running
service, and a direct violation of §59's "external services are capability
providers behind a stable protocol" (the same reason Transmission moves bytes and
TMDB resolves metadata). The right change is at the **seam**, not in absorbing the
engine.

## Decision

**A new `prowlarr` provider kind is a single `CapabilityIndexer` Entry that speaks
Prowlarr's native API and covers *every* indexer Prowlarr currently manages —
queried live, so heyarr's searchable set tracks Prowlarr's as it changes, with no
per-indexer heyarr config.** One Entry, one key, the whole of Prowlarr; Prowlarr
keeps owning the definitions, the anti-bot and the GPL boundary.

### 1. One Entry, the whole aggregator

```yaml
- name: prowlarr
  type: prowlarr
  endpoint: http://127.0.0.1:9696   # Prowlarr base URL, not a /{id}/api path
  api_key: <prowlarr api key>       # X-Api-Key; AuthToken scheme (ADR-0031)
```

The provider implements the existing `Indexer` interface. On search it calls
Prowlarr's **aggregate search** (`GET /api/v1/search?query=…&categories=…&type=search`),
which fans the query across every enabled indexer Prowlarr holds and returns
releases already normalised (title, size, seeders, categories, indexer name,
download/magnet URL). heyarr maps those to its release model exactly as the
`torznab` provider maps a Torznab feed — same `Indexer` contract, same grading
downstream. No dynamic provider registration: the registry stays config-driven
(one Entry), and "which indexers" is answered **live at search time** by Prowlarr,
not baked into heyarr's config. That is what makes it self-syncing — add or remove
an indexer in Prowlarr and the next search reflects it, because heyarr never held
the list.

### 2. Health reports the aggregate

`Check` pings Prowlarr (`/api/v1/health` or `/api/v1/indexer`) and reports
reachability **plus how many indexers are enabled** (e.g. `reachable — Prowlarr, 7
indexers`), so `get_provider_status` shows the aggregator's real coverage rather
than a bare "reachable". A Prowlarr that is up but has zero enabled indexers is
surfaced as healthy-but-empty, which is the honest state (it is why a search finds
nothing).

### 3. The per-indexer `torznab` Entry stays

`type: torznab` is unchanged and still supported: it remains the way to point
heyarr at **one** Torznab/Newznab feed directly — a single tracker, or a Prowlarr
indexer you deliberately want isolated (its own health line, its own grading
scope). `prowlarr` is the *aggregate* door; `torznab` is the *single-feed* door.
An operator uses one, the other, or both (a `prowlarr` Entry beside a standalone
`torznab` Entry is fine — they are distinct sources). The two existing hyperion-1
entries can collapse into one `prowlarr` Entry, or stay as-is; nothing forces the
migration.

### 4. Attribution and categories

Prowlarr's search result carries the originating indexer name; heyarr records it
on the release so `explain_release` can still say *which* source a candidate came
from, even though they arrived through one provider. heyarr's content-type →
Torznab category mapping (the same table the `torznab` provider uses) is passed to
Prowlarr's `categories` parameter, so a movie search does not drag back every
category from every indexer.

### 5. It is pull, not Prowlarr's app-push

Prowlarr's "Applications" sync works by Prowlarr calling an *arr app's API to push
indexers into it. This ADR deliberately does **not** implement that contract:
heyarr **pulls** from Prowlarr instead. Pull keeps heyarr in control of its own
provider registry (nothing external writes into it), needs no Prowlarr-side
configuration of heyarr as an "application", and avoids heyarr having to expose a
Prowlarr-shaped indexer-management API it does not otherwise have. One key in one
direction, read-only.

## Consequences

- **The indexer set lives in one place — Prowlarr — and heyarr tracks it live.**
  The "two places to configure" friction is gone: manage indexers in Prowlarr,
  and heyarr's search set follows with no config edit. This is the "feels built
  in" experience without building the engine in.
- **No new maintenance surface, no license entanglement.** Prowlarr keeps owning
  the definition catalog, its updates, the anti-bot layer and the GPL boundary.
  heyarr gains one small read-only client. §59 stays intact.
- **`get_provider_status` becomes informative** — one `prowlarr` line with a live
  indexer count, instead of N torznab lines that may or may not match what
  Prowlarr actually holds.
- **A wrong or empty Prowlarr is legible** — unreachable, or reachable-with-zero-
  indexers, are distinct honest health states, not a silent "search finds
  nothing".
- **The `torznab` single-feed door is untouched**, so nothing regresses and the
  isolated-single-tracker use case still has its provider.

## Alternatives considered

- **Absorb Prowlarr (a native Cardigann engine over the community defs).**
  Rejected (context): GPLv3 conflict + a perpetual definition-repo sync + anti-bot
  infra, to save one service — the treadmill §59 exists to keep out of the core.
- **Dynamically materialise N heyarr providers from Prowlarr's indexer list.**
  Rejected: it makes the provider registry a mirror of a remote system's state,
  fighting the config-driven registry, and buys per-indexer health at the cost of
  a moving registry and add/remove churn. The single aggregator with a live count
  (§2) gives the same operational visibility without a self-mutating registry.
- **Implement Prowlarr's app-push sync (heyarr as a Prowlarr "Application").**
  Rejected (§5): it inverts control (Prowlarr writes heyarr's config), needs
  Prowlarr-side setup, and forces heyarr to expose an indexer-management API. Pull
  is simpler and read-only.
- **Leave it — keep one `torznab` Entry per indexer.** Rejected: that *is* the
  two-places-to-configure friction the operator hit; the whole point is to remove
  it.

## What would make us revisit

- **Per-indexer grading or health from the aggregate** — if operators want to
  score or disable a single Prowlarr indexer from heyarr, the aggregate would need
  to surface per-indexer handles (Prowlarr's search API carries the indexer id;
  the door is open) without going all the way to a mirrored registry.
- **Newznab/Usenet through the same door** — Prowlarr aggregates Usenet indexers
  too; this provider can cover them once heyarr has a Usenet **download** client
  (today only torrent transports exist), at which point the aggregator is already
  the right shape.
- **A managed/bundled Prowlarr** — heyarr owning Prowlarr's lifecycle (deploy,
  update, the auth wall) is a further ergonomic step that keeps this same seam; a
  separate concern from consuming it.
