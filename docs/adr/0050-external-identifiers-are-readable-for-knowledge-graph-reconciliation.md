# 0050. External identifiers are readable, for knowledge-graph reconciliation

**Status:** Proposed — **external draft, awaiting heyarr-maintainer sign-off** (raised by the `jumpdrive-index` / Starchart track; ADR number 0050 coordinated against main @ ADR-0049)
**Date:** 2026-08-27

## Context

heyarr already stores external identifiers. `00002_core.sql` defines:

```sql
CREATE TABLE external_ids (
    id          TEXT PRIMARY KEY,
    entity_type TEXT NOT NULL CHECK (entity_type IN ('work', 'edition')),
    entity_id   TEXT NOT NULL,
    source      TEXT NOT NULL,   -- e.g. 'tmdb', 'imdb'
    value       TEXT NOT NULL,
    UNIQUE (source, value, entity_type)
) STRICT;
```

These rows are how a work is *known to the outside world* — `tmdb:603` is The
Matrix, `imdb:tt0133093` is the same film. They are populated on the write path
but **surfaced on no `/api/v1` route and no MCP tool**. Concretely, the one read
that comes closest — `search_content` (ScopeRead, `internal/api/mcp/reads.go`) —
runs `SELECT id, content_type, title, year FROM works …` and returns
`{"works":[{"work_id","content_type","title","year"?}], "count", "truncated"}`:
titles and types, and **not one external identifier**. A consumer that holds a
`tmdb` id and wants to know *which heyarr work it is* — or holds a heyarr
`work_id` and wants its `tmdb`/`imdb` ids — has no read path. The data is there;
the door is not.

The consumer that needs this is **`jumpdrive-index` / Starchart**, a schema.org
knowledge-graph index that links entities across systems by external id and
**references heyarr titles by id, never by copying heyarr's catalogue** (a
Starchart node that *is* a heyarr title carries only `{heyarr-work|edition|asset,
id}` and reads title/year through `search_content` at query time). To place a
YouTube analysis video's "about Alien" edge onto the right heyarr work, Starchart
resolves an outside `tmdb` id to a heyarr `work_id`. Without a readable
`external_ids`, that reconciliation is impossible and the whole cross-system join
degrades to fuzzy title matching — exactly the ambiguity external ids exist to
remove.

This record does not ship behaviour. It decides that heyarr *exposes what it
already stores*, read-only and additively, and fixes the shape before Starchart's
M3 depends on it — the same "decide the contract once, in writing, because it is
cheap now" posture as ADR-0044/0048.

Framing facts:

- **ADR-0005** — a BLAKE3 whole-object digest is the canonical *byte* identity.
  External ids are a *different altitude*: catalogue identity (this film), not
  byte identity (this file). Both are join keys; this ADR surfaces the former and
  leaves the latter (already reachable) alone. Starchart joins at whichever
  altitude the question needs — `work_id` (semantic), `edition_id` (version),
  `blake3:` (byte).
- **ADR-0019** — MCP is the agent-facing surface; a new read belongs there as a
  ScopeRead tool, mirroring `search_content`, not as a bespoke shape.
- **ADR-0020 / ADR-0025** — external network *services* are optional and
  capability-routed, and a missing capability **degrades to "no match," never an
  error**. This read must inherit that contract: an unknown id returns an empty
  result, not a failure, so a caller can probe cheaply.
- **ADR-0032** — additive, read-only, no new content type and no new authority:
  nothing here enrols, grants, or writes. heyarr stores **no edge back** to the
  consumer; the relationship lives entirely in the consumer's graph.

## Decision

Expose the existing `external_ids` rows through three read-only, additive
surfaces. No schema change; no new content type; no write path.

1. **`GET /api/v1/works/{id}/external-ids`** (and the `editions` analogue) —
   forward lookup: the external ids of a known work/edition. Returns
   `[{source, value}, …]`, empty array when none.
2. **`GET /api/v1/external-ids?source=tmdb&value=603`** — reverse lookup: the
   `{entity_type, entity_id}` (at most one, by the `UNIQUE(source,value,entity_type)`
   constraint per type) carrying that id. Empty result when unmatched — **"no
   match," per ADR-0025, never a 4xx for an absent id.**
3. **MCP tool `get_external_ids`** (`Scope: auth.ScopeRead`, registered in
   `internal/api/mcp/tools.go` beside `search_content`) — the agent-facing twin of
   the two routes, taking either `{work_id}`/`{edition_id}` (forward) or
   `{source, value}` (reverse).

All three are read-only projections of rows heyarr already owns. OpenAPI is
hand-written and contract-tested (ADR-0015), so the two REST routes land with
their spec and a contract test; the MCP tool lands with a boundary test
(`internal/api/mcp/boundary_test.go` idiom).

## Consequences

- Starchart's M3 reconciles `tmdb`/`imdb` → heyarr `work_id` (and back) over a
  stable, documented contract, and vendors this shape into its heyarr↔Starchart
  contract-drift gate (so a future heyarr change to the surface is caught, not
  silently broken).
- The read leaks only what a catalogue id already is — public identifiers for a
  work the caller can already `search_content`. It exposes no bytes, no vault
  content, no acquisition state; ScopeRead is the correct ceiling.
- heyarr keeps no relationship to the consumer: no edge, no back-reference, no new
  durable state. If Starchart disappears, nothing in heyarr changes. This is the
  property that makes the surface cheap to grant and cheap to keep.
- **Revisit if** heyarr ever wants to *write* external ids from a consumer, or to
  model a first-class cross-system link on its own side — both are new authority
  and out of scope here; this ADR is deliberately the read-only floor.

---

*Provenance: proposed by the jumpdrive-index/Starchart integration track (M3).
Needs a heyarr-maintainer decision on the route shapes and the ADR number before
any implementation PR. Do not merge without that sign-off.*
