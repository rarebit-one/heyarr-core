# 0073. The catalog converges by an editorial op-log over a re-derivable base

**Status:** Proposed
**Date:** 2026-09-04
**Builds on:** ADR-0038 (each peer is authoritative for its own site), ADR-0068
(membership ops are a state-based CRDT), ADR-0003 (the control plane is
single-writer SQLite), ADR-0005/0030 (content-addressed bytes converge by a
destination pull), ADR-0017 (time, identifiers, determinism), ADR-0018
(deletion is logical), ADR-0071 (a followed work refuses an edition delete)
**Tracks:** #449 (design first — no schema change and no Go change lands with
this record)

## Context

The homelab is going two-site active-active: peer-a at Site A and
peer-b at Site B are two peers of one library, and either site should accept
writes and converge. Three planes already converge under a partition and need
nothing here:

- **CAS blobs** — content-addressed and immutable (ADR-0005); a destination
  pulls what it lacks and verifies it (ADR-0030). Identical bytes are the same
  object everywhere, so blobs converge with no coordination (ADR-0038).
- **Voidbind membership ops** — a state-based CRDT (ADR-0068, `membership_ops`
  in 00022): a G-set keyed by op hash, merge is set union, remove wins
  causally, seniority breaks concurrency, and `enrolment.Evaluate` is a pure
  function every relying party runs to get the same device set. It is
  server-opaque — the node evaluates the log, it never authors into it — and it
  rides the wire as small tokens carried beside a credential and fetched over
  `GET/POST /membership`.
- **Encrypted personal state (M9)** — `encrypted_changes` in 00038: a
  content-addressed causal DAG of opaque changes the peer routes by parents and
  never reads; convergence is client-side (§42, §44), and the peer is the
  single writer of its own opaque store.

The one plane the move to active-active opens and that heyarr does **not**
converge is the **library catalog** — the mutable control-plane rows in
`00002_core.sql` (`works`, `editions`, `external_ids`, `assets`) and M12's
`follow_sources` / `items` (00040). These are ordinary single-writer SQLite
rows (ADR-0003, Invariant 5). A create or edit at Site A and a concurrent one at
Site B have no defined merge. ADR-0038 answered this for **desired state** — two
sites wanting 1080p and 4K "are two policies, both authoritative where they
live… there is nothing to merge" — but the catalog is not desired state. It is
the two sites' shared description of *one* collection, and there the sites are
meant to agree, not to diverge.

### What already converges indirectly, and what genuinely does not

The catalog is not one thing. Most of it is **re-derivable from bytes that
already converge**:

- `works` are get-or-created on a normalised `work_key`
  (`UNIQUE (content_type, work_key)`), so a rescan "converges on the same Work
  instead of multiplying it".
- An edition is "scanner-recreatable — the identifier a scan re-derives from
  the files on disk re-creates the edition it belongs to" (ADR-0071).
- `assets` are a projection of blobs on disk, and blobs converge via CAS.

So if both sites hold the same blobs (they do — CAS) and run the same
identification, they independently get-or-create the **same** spine rows by the
same natural keys. That base converges today without a merge model, the same
way the §52 catalog snapshot (`internal/peer/catalog`, a one-way read view from
the controller era) was only ever a rebuild, never a writable convergent state.

What does **not** re-derive from bytes is the *editorial* layer laid on top:

1. **Human/agent metadata edits** — a corrected title, an `attributes` change,
   an identifier written into `external_ids` (sparse: most works carry none;
   #431 only inlined the forward read). A scan will not reproduce these
   identically at the other site.
2. **`follow_sources`** — a standing subscription is pure configuration
   (`work_id`, `feed_ref`, `monitor`, `quality_profile_id`, `backfill`),
   `UNIQUE (work_id, feed_ref)`. Following a series at Site B creates a fact no
   scan at Site A will ever derive.
3. **Logical deletes** — `DELETE /works/{id}`, `DELETE /editions/{id}`
   (ADR-0071), `DELETE /assets/{id}`. A delete at one site that does not reach
   the other is *worse than silent*: the other site's next scan or next follow
   poll re-materialises exactly what was removed — the "delete-and-rebuild"
   churn ADR-0071 already refuses within one node, generalised across sites.

The gap #449 names is precisely this editorial layer. The design question is
how it converges without introducing a second writer to a control database
(ADR-0003) and without minting a third convergence mechanism whose semantics
nobody else in the codebase shares.

## Options considered

### A. A per-entity op-log, evaluated the way membership already is

Every editorial mutation (create/edit/delete of a work/edition/follow-source,
add/remove of an external id) becomes a signed **op** in a G-set keyed by the
op's content hash, carrying a **stable entity id** and a causal `prev`. A pure
evaluator materialises the ops into the existing catalog rows in the same
single-writer transaction — exactly the shape ADR-0068 already ships, where
`membership_ops` is the truth and `device_identities` is the reconciled view.

- **Pros.** It is the pattern the codebase has already proven twice
  (`membership_ops` and `encrypted_changes` are both content-addressed causal
  logs materialised into a view). It respects Invariant 5 / ADR-0003 untouched:
  one writer applies the log, convergence is a *pure evaluation*, not a second
  writer on the control plane. It rides the **existing** transport with no new
  machinery — ops are small content-addressed rows carried beside requests and
  fetched over an endpoint, just like membership ops, or synced as a causal DAG
  like `encrypted_changes`. It is true multi-master: either site writes.
  Tombstones and remove-wins come for free from the same merge rule membership
  uses, which is the exact primitive the cross-site delete problem needs. It is
  idempotent by op hash and deterministic (ADR-0017). And it *composes* with
  membership rather than sitting beside it — one merge discipline, not two.
- **Cons.** It is the largest build if applied naïvely to *all* catalog rows.
  If the scanner emitted an op per get-or-create, op volume would explode and
  two sites scanning the same bytes would emit two op-streams for one logical
  create. It needs a cross-site-stable id for entities that have none (see open
  questions). It needs an evaluator plus a same-transaction view reconciliation
  (the `RecordOps` idiom). Field-vs-row merge granularity is still a decision.

### B. Designated-writer-per-entity

Each entity has an owner peer; only the owner mutates it; other sites hold its
rows read-only and forward edits (or refuse them with a hint, the way ADR-0071
answers 409 and names the fix). This is ADR-0038's "authoritative for its own
site", taken to a per-entity grain.

- **Pros.** No merge logic at all — concurrent writes to one entity cannot
  happen by construction. Small: an `owner_peer_id` column and a
  forward-or-refuse rule. It matches today's reality, where Site A owns everything
  because clients hardcode the site-A origin. Deletes are owner-authored facts
  that replicate cleanly.
- **Cons.** It forfeits the goal. #449 asks that *either* site accept writes and
  converge; B makes a non-owner write a round-trip to the owner that **fails
  under partition** — the one thing ADR-0038 insists must be "Tuesday", not a
  fault. Worse, it does not converge concurrent *creates*, it forbids them: two
  sites scanning the same new blobs both create the work, and deciding an owner
  after the fact needs a deterministic election (owner = lowest peer id, or by
  `work_key` hash). That election is a global-agreement rule — the consensus
  ADR-0038 spent a whole record explaining a homelab fabric does not need.
  `follow_sources` created independently at both sites still collide.

### C. Per-field LWW registers with tombstones and a logical clock

Each mutable field carries a last-writer-wins register — `(value, HLC, writer
peer id)` — deletes are clocked tombstones, and merge takes the field with the
highest `(clock, peer id)`. HLC sits under ADR-0017's time/determinism remit.

- **Pros.** Bounded state: O(rows), not the unbounded op-log an append-only G-set
  grows into. Simple and well understood, deterministic tie-break by peer id.
  Tombstones handle deletes and, clocked correctly, can beat a later
  scanner-recreate. Replicates as a row-diff above a high-water mark.
- **Cons.** LWW *discards* the losing concurrent edit silently and without
  trace — tolerable for a title, dangerous for a `follow_source` setting where a
  dropped `monitor`/`quality_profile_id` change is invisible. It needs a
  trustworthy wall clock (two homelab sites are fine, but it is a new
  dependency and a new failure mode). It requires clock + writer columns on
  **every** catalog table — the migration churn across `works` / `editions`
  (which today has no `updated_at` at all) / `items` / `follow_sources` /
  `external_ids` that ADR-0038's authors keep declining. Per-key LWW over the
  `attributes` JSON blob is awkward: one register per blob clobbers concurrent
  edits to different keys, per-key registers are a large decomposition. It keeps
  no causal history — it cannot say *why* a value is what it is, which
  membership's `prev` gives for nothing. And it is a **third** convergence
  mechanism with semantics neither membership nor M9 share.

## Decision

**Adopt Option A, scoped tightly to the editorial layer: the catalog converges
by an append-only, content-addressed op-log — merged and materialised exactly as
membership ops are (ADR-0068) — laid as an *overlay* over the byte-derivable
spine, which keeps converging indirectly as it does today.**

The two halves are the whole decision:

**The base re-derives; it is not put into the op-log.** `works`, `editions` and
`assets` that a scan get-or-creates from converged CAS blobs by their natural
keys (`work_key`, the ADR-0071 recreatable edition identity, the blob hash)
continue to converge with no ops at all. The scanner emits nothing to the log.
This is what neutralises Option A's only real objection — the op-log carries
low-volume *intent*, not the high-volume derivable spine, so it never explodes
and two independent scans never fight over one logical create.

**The overlay is an op-log the sites merge like membership.** Intentional,
non-derivable editorial acts — a logical delete, a `follow_source`, a metadata
override / `external_id` — become signed ops in a G-set keyed by op hash, each
naming a **stable entity key** and a causal `prev`. A pure evaluator merges
(set union; remove wins causally; seniority breaks ties) and reconciles the
result into the existing rows **in the same single-writer transaction** — the
`RecordOps` idiom, so ADR-0003 / Invariant 5 is untouched: there is still
exactly one writer per control database, and convergence is a pure function of
the ops, never a second writer reaching across a network. A delete becomes a
remove-wins tombstone that a later scan's get-or-create must lose to (an
entity the overlay has tombstoned is suppressed), which is how the cross-site
"delete then re-materialise" churn is closed — the same guarantee ADR-0071
gives inside one node, now across the pair.

Ops ride the transport the codebase already has: small tokens carried beside a
request and fetched over an endpoint (membership's shape), or synced as a
content-addressed causal DAG (M9's shape). No new replication machinery is
introduced by this record.

### Interim posture (unchanged by this record)

Catalog writes funnel to Site A today because clients hardcode the site-A origin;
the interim write path is ADR-0061's follow-management grant and ADR-0065's
device write scope. The base spine already converges to Site B via CAS + identical
scanning; the *editorial* layer does not, so until Phase 1 a delete or a follow
initiated at Site B is either forwarded to Site A or simply not offered. This ADR does
not remove the funnel — it defines the target that lets the funnel be removed
one increment at a time.

## A phased sketch (design only)

- **Phase 0 — today.** Keep the funnel. Base converges via CAS + scan; editorial
  writes originate at Site A. Document that Site B is read-mostly for the catalog.
- **Phase 1 — tombstones first.** A delete op-log so a logical work/edition/asset
  delete at either site propagates and *suppresses* the other site's
  scanner/poll recreate. Deletes are the sharpest correctness bug (silent
  resurrection), and the smallest overlay: remove-wins keyed by the entity's
  stable key, reusing membership's merge rule and a DAG-style sync.
- **Phase 2 — `follow_sources` as ops.** The purest non-derivable config. Follow
  at either site and converge the subscription set as a G-set keyed by the
  work's stable key + `feed_ref`; `monitor` / `quality_profile_id` / `backfill`
  resolve by last-op-wins within the entity.
- **Phase 3 — the editorial metadata overlay.** `external_id` adds/removes and
  title/`attributes` overrides as ops layered over the scanner-derived base.

Each phase is an overlay evaluated into the existing rows in one transaction; no
phase adds a second writer, and Phase 1 alone removes a real active-active
hazard.

## Open questions

1. **The stable id.** `works.id` / `editions.id` are per-site UUIDv7 (ADR-0017),
   so two sites mint different ids for the same film — they cannot key a
   cross-site op. `works` has `work_key` (`UNIQUE (content_type, work_key)`) and
   that is the natural candidate; **editions have no cross-site-unique natural
   key** beyond `(work, label, edition_type, language)`, and `external_ids` are
   too sparse to rely on. Do we key on `work_key`, synthesize a deterministic
   edition key, or add a `stable_key` column (a schema change, out of scope
   here but flagged)?
2. **Who signs a catalog op.** Membership is deliberately server-opaque — the
   node evaluates but never authors (ADR-0068). A catalog fact is a *node/library*
   fact, so the peer probably *does* sign with its ADR-0012 identity — a
   different trust model from membership. Or does the writing device sign under
   its ADR-0065 write scope? This must be decided before Phase 1.
3. **Scanner determinism.** The overlay assumes the base re-derives *identically*
   at both sites (same identifier, same `work_key` normalisation). If
   identification is nondeterministic across sites, the base itself diverges and
   the overlay's "carry only intent" premise breaks. This needs verifying, not
   assuming.
4. **Tombstone lifetime and the resurrection window.** A delete tombstone must
   outlive any in-flight scan that could re-create the row. How long are
   tombstones kept, how do they interact with `gc_blobs`' grace window
   (ADR-0018), and how does a legitimately re-added work (re-ripped months
   later) defeat an old tombstone — a new causal op citing it?
5. **`attributes` JSON granularity.** Whole-blob op (concurrent edits to
   different keys clobber) versus per-key ops (both survive, larger change).
6. **The §52 catalog snapshot.** `internal/peer/catalog` is a controller-era
   one-way read view (pre-ADR-0038). Does the overlay retire it, repurpose it,
   or leave it independent?
7. **`items` and the follow projection.** `items` (byte-less rows) are a
   projection of a `follow_source` (ADR-0057) and cascade from it. Once Phase 2
   converges follow-sources, do items simply re-project at each site, or do they
   need their own convergence?

## Revisit if

- The editorial op-log grows unbounded in practice (heavy metadata churn). The
  answer is then log compaction / a materialised checkpoint, the way M9 pairs
  `encrypted_changes` with `encrypted_snapshots` (00039), not a switch to LWW.
- Scanner identification proves nondeterministic across sites, which would pull
  parts of the *base* spine into the overlay and enlarge the scope well beyond
  the editorial layer this record draws.
- A per-entity owner turns out to be wanted for a genuinely site-local catalog
  fact (Option B in miniature), at which point that fact is desired state, not
  catalog, and ADR-0038 already covers it.

---

*Provenance: #449, the two-site active-active convergence gap. Design first — no
migration and no Go change lands with this record; it fixes the model so the
build can proceed in the phases above.*
