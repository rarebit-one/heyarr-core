# 0088. A confident enrich match corrects a Work's display identity, never its dedup key

**Status:** Accepted (2026-09-10)
**Date:** 2026-09-10
**Milestone:** M12 — Followed Sources / The Archive (Phase 6, catalogue enrichment)

## Context

ADR-0087 gave held music/book Works an enrichment path: a `CapabilityEnrich`
provider keyed on the Work returns its canonical ids and a cover, an
`enrich_work` job writes `external_ids` and attaches the cover, and every
consumer serves it with no change. ADR-0087 §4 drew a deliberate line: enrichment
**ADDS** — ids, a cover, a description — and does **not** overwrite the Work's
`title`, `year`, `author`, because those are the identity the `work_key` and every
reference depend on, and a fuzzy provider match must not silently rename a
correctly-identified Work. It named the counter-case as a future revisit:
*"Enrichment overwriting identity when a provider match is confident enough to
correct a bad local identification, rather than only adding to it."* This ADR is
that revisit, forced by what the real library actually holds.

### The library's book identity is not merely sparse — it is wrong

The ~219 held books were ingested from a shelf layout — `Books/<filename>.epub`,
`Reads/<filename>.epub`, `Review/<filename>.epub` — where the top directory is a
*shelf*, not an author. `identification`'s `matchBookAuthorDir` (book.go:77) reads
the directory as the author and the whole filename stem as the title, and the
filenames cram title + author + provenance junk with no delimiter for `splitPair`
to find. The result, verbatim from the catalogue:

- `author = "Books"`, `title = "AI Engineering Building Applications With Foundation Models Chip Huyen"`
- `author = "Reads"`, `title = "The Almanack Of Naval Ravikant Eric Jorgenson Z Library"`
- `author = "Review"`, `title = "13 Chicken Coop Plans And Designs ... Davidson John Guptill Jeffrey"`

The author is a shelf name; the real author (`Chip Huyen`, `Eric Jorgenson`) is
trapped inside the title, alongside provenance noise (`Z Library`, `Kepub`). This
is not a Work missing a cover — it is a Work whose *identity is a filename
accident*. ADR-0087's "identity is the ingest heuristic's, and enrichment only
decorates it" is exactly wrong here: the ingest heuristic had nothing to work
with, and decorating a wrong title with the right cover leaves the library still
unbrowsable by title or author.

### Why the local heuristic cannot fix it, and OpenLibrary can

No filename rule can split `"The Almanack Of Naval Ravikant Eric Jorgenson"` into
title and author without knowing that *Eric Jorgenson* is a person and *The
Almanack of Naval Ravikant* is a work — that knowledge is precisely what
OpenLibrary is. The enrich provider ADR-0087 already calls returns a *canonical*
record: the work's real title and its author(s). The correction is therefore not
new network machinery — it is a decision about **what to do with an answer we are
already fetching** when that answer is trustworthy.

## Decision

**A `CapabilityEnrich` match that is confident enough corrects a Work's
*display* identity — its `title`, `sort_title`, and `author` attribute — from the
provider's canonical record; it never rewrites the Work's `work_key`.** Identity
splits into two things that ADR-0087 treated as one: the *dedup key* (stable,
ingest-derived, what uniqueness and references stand on) and the *displayed
identity* (what a person reads and searches). Enrichment may fix the second when
sure, and must not touch the first.

### 1. `work_key` is frozen; display fields are corrected

The Work row already carries `work_key`, `title`, and `sort_title` as separate
columns (they are distinct in every `browse_library` result). ADR-0087 read them
as one identity; they are not. The `work_key`
(`workKey(Book, authorKey, titleKey, yearKey)`, book.go:161) is the **dedup key** —
the unique index, the reconcile match, every foreign reference. Re-keying it on a
fuzzy match is the collision-and-merge hazard ADR-0087 rightly refused, and this
ADR does **not** do it: a corrected Work keeps its ingest `work_key` exactly. What
changes is the **display identity** — `title`, `sort_title` (its normalised form),
and `attributes.author` — the fields a person reads, sorts and searches by, none
of which any other row keys on. The key stays the filename's; the face becomes the
truth.

This is why the correction is safe where a rename would not be: two messy Works
that both map to one canonical book (`"6 Frank Herbert"` and
`"6 Frank Herbert Z Library"`) keep their two distinct `work_key`s and stay two
rows — they are not merged, not collided, just each shown correctly. **Merging
duplicate Works is explicitly out of scope** (see revisit).

### 2. The provider answer gains canonical identity + a confidence

`EnrichResult` (ADR-0087) extends with the canonical identity and a match
confidence the adapter computes, so the *provider* owns "how sure am I" and the
*worker* owns "is that sure enough":

```
type EnrichResult struct {
    ExternalIDs  map[string]string
    CoverURL     secret.Value
    Overview     string
    Title, Author string   // canonical display identity (empty ⇒ no correction offered)
    Confidence   float64   // 0..1, the adapter's own match strength
}
```

The adapter derives `Confidence` from the normalised similarity between what it
searched for and what it matched (token-set ratio over the noise-stripped query
vs. the returned title+author), so a top-hit that barely resembles the query
scores low and a near-exact one scores high. Values in, values out — the adapter
still draws the provider line (ADR-0026 fixtures test the scoring, never live).

### 3. Correction is gated, additive, and auditable

The `enrich_work` worker applies a display correction only when
`Confidence >= enrichCorrectThreshold` (a named constant, conservative by default
— high enough that a wrong match is rejected and left bare, per ADR-0087's "finds
nothing ⇒ back off, never an error"). When it corrects:

1. it writes the canonical `title` + recomputed `sort_title` and sets
   `attributes.author`;
2. it preserves the original ingest strings under
   `attributes.identified_title` / `attributes.identified_author` — the correction
   is auditable and reversible, and a later confident-mismatch review can see what
   was replaced;
3. it still writes `external_ids` and the cover exactly as ADR-0087 specifies —
   the id is *why* the title is now trustworthy.

Below the threshold, the Work keeps its ingest identity untouched and only gains
whatever ids/cover the weak match justified (or nothing). Correction never blanks
a field: an empty canonical `Title` or `Author` is "no correction offered", not
"erase what we had".

### 4. Scope is books first, the mechanism is content-type-agnostic

Books are where the damage is (a shelf-as-author) and where OpenLibrary is the
authority, so the OpenLibrary adapter is the first to return a confident canonical
identity. The worker path is not book-specific — a MusicBrainz match that is
confident enough corrects an album's `title`/`artist` by the same rule — but music
is absent from the library today, so books are the only live corrector. Movies and
series are untouched: enrichment does not run on them (ADR-0087 §3), and their
identity comes from a different, generally cleaner path.

## Consequences

- **The library becomes browsable by real title and author.** The visible payoff
  of enrichment is not just a cover but a shelf where `The Almanack of Naval
  Ravikant` is filed under `Eric Jorgenson`, not under `Reads`.
- **No re-keying, so no merges, collisions, or dangling references.** Every Work
  keeps its `work_key`; assets, editions, wants and external_ids stay attached.
  The correction is a column update on one row, not a catalogue reshaping.
- **The correction is reversible and auditable** — the ingest strings survive in
  `attributes`, so a bad correction can be found and undone, and the threshold can
  be re-tuned against real outcomes.
- **A weak match changes nothing** — the conservative threshold means enrichment
  either improves a Work or leaves it exactly as ingested; it never degrades one.
- **Duplicate Works stay duplicated** (the `Z Library` twins, the FLAC/Kepub
  format-variants that resolved to distinct keys). They now display correctly but
  remain separate rows.

## Alternatives considered

- **Re-key the Work from the canonical identity (true re-identification).**
  Rejected: changing `work_key` collides confident matches onto one key (the
  duplicates above), forcing a merge of two rows with their own editions, assets
  and wants — a large, dangerous catalogue operation. Freezing the key and
  correcting only the face delivers the browsable-library win with none of it.
- **Fix the identification heuristic instead (parse author out of the filename).**
  Rejected (§context): no local rule can separate a person from a title without an
  authority; that authority is the provider we already call.
- **Keep ADR-0087's no-overwrite rule and only add a cover.** Rejected: a right
  cover on a wrong title (`author = "Books"`) leaves the library unbrowsable; the
  want was a usable shelf, not decoration on a broken one.
- **Overwrite unconditionally from the top hit.** Rejected: an unconfident match
  would rename a correctly-identified Work — exactly the harm ADR-0087 guarded.
  The confidence gate is the whole safety of this ADR.

## What would make us revisit

- **Merging duplicate Works** — collapsing the `Z Library` twins and format
  variants that now display as one book but persist as several rows — is the next
  step and a separate ADR (it needs an edition-and-asset re-parenting story).
- **A confident-mismatch review UI** — surfacing corrections (via the preserved
  `identified_*` attributes) for a human to confirm or revert, rather than trusting
  the threshold alone.
- **Correcting movie/series identity** from TMDB by the same rule, if their ingest
  identity ever proves as noisy as the book shelves did.
- **Promoting author to an entity** keyed on the OpenLibrary author id this
  correction now records, rather than a corrected string attribute (ADR-0087's own
  named revisit).
