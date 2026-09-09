# 0086. An asset records the item it belongs to

**Status:** Accepted (2026-09-09)
**Date:** 2026-09-09
**Milestone:** M12 — Followed Sources / The Archive (Phase 5, subtitle provision)

## Context

ADR-0056 placed the Item between Edition and Asset — `Work ── Edition ── {Item}
── Asset ── Blob` — as the byte-less thing a source emitted, so a want could
point at one episode before its bytes existed. But the physical schema never drew
the Item→Asset edge: an asset row references an `edition_id`, and nothing else.
An Item references its work and (nullably) its edition; an asset references its
edition. The two meet only at the edition they share.

For most of what heyarr does that is enough, because the edition is the grain the
consumer reads at. It stops being enough the moment a question is asked about one
*item* whose edition holds many:

- A television season is ONE edition holding every episode's video — and, after
  ADR-0084, every episode's extracted subtitle. "Does *this episode* hold an
  English subtitle?" cannot be answered from the schema; only "does *this
  season*." The video-to-subtitle binding that makes captions work at all rests
  entirely on a filename-stem convention (`renderers.go` `captionForRenderer`
  stem-matches a subtitle to a video at serve time), which is a serving heuristic,
  not a fact the catalog can query.
- ADR-0085's per-episode subtitle wants need exactly that per-item answer. A want
  for the English subtitle of S02E05 is satisfied by a subtitle *for S02E05*, not
  by one that happens to sit on the same season edition for S02E01.

The same gap makes an item-scoped *primary* want's content satisfaction coarser
than it should be — it is evaluated over the whole work's assets today — but that
is a latent imprecision the existing features tolerate, not the pressing need.
The pressing need is subtitles, and it forces the edge to be drawn.

The alternatives were weighed (ADR-0085 session): resolving an item's asset by
matching its `item_key` inside the filename, or accepting edition-coarse
satisfaction. The first re-derives identity from a naming convention — exactly
the fragility ADR-0006 and the credential work (ADR-0031) exist to refuse — and
the second reports a 17-episode season as fully captioned the moment one episode
is. Neither is honest. The honest thing is to store the fact.

## Decision

**An asset carries a nullable `item_id`, and the acquisition that knows the item
sets it.**

### 1. A nullable link, populated by the knower

`assets` gains `item_id TEXT REFERENCES items(id) ON DELETE SET NULL` (a plain
`ADD COLUMN` — no CHECK changes, so no table rebuild). It is nullable because
NULL is the honest answer wherever nothing named an item:

- a **library-scan** asset was matched to a work by its path, and no source ever
  enumerated an item for it;
- a **film's** asset (and its subtitle) attaches to the film's edition with no
  item in play at all.

`ON DELETE SET NULL`, not `CASCADE`: an asset outliving the byte-less Item row it
was linked to is a real state — the item metadata was pruned, the bytes remain —
and pruning metadata must not delete content. This is the opposite of `edition_id`
and `work_id`'s CASCADE, and deliberately so: losing an item is losing a label,
losing a work or edition is losing the thing itself.

### 2. The link is set post-ingest, by the acquisition, not threaded through ingest

The item is known where the *want* is: an item-scoped `DesiredItem` carries the
item id, and the acquisition worker holds it when it ingests the grabbed bytes.
So the worker links the created asset to the item *after* `ingest.Pipeline.Ingest`
returns its `AssetID` — a single `catalog.SetAssetItem(assetID, itemID)` — rather
than threading an item id through the ingest domain.

This is deliberate. The ingest pipeline is the identity heuristic for a file
whose provenance is only its path (ADR — `WorkOverride` already carves out the
one fact an acquisition knows better, the Work, and says plainly that everything
per-file still comes from the path). The Item is another such known fact, but it
is not something the *file* reveals — it is something the *want* asserts — so it
belongs to the acquisition that asserts it, applied to the asset the ingest
produced, keeping the pure pipeline free of one more caller-known override.
`SetAssetItem` is idempotent and re-linking to the same item is a no-op, so a
retried ingest converges.

### 3. It is additive: existing satisfaction is unchanged

Nothing that reads assets today learns about `item_id` in this change. An
item-scoped primary want still evaluates the way it did, so no existing
deployment's satisfaction shifts and no backfill is required to keep working. The
new column is *used* by ADR-0085's subtitle satisfaction (the next slice), which
is new code judging new wants: a subtitle want scoped to an item is satisfied by
a `role='subtitle'` asset whose `item_id` matches and whose language matches, and
one scoped to an edition (a film) by such an asset on that edition. Making the
existing primary path precise with the same link is a follow-up that begins by
backfilling `item_id` for already-ingested items — worth doing, not now.

## Consequences

- Per-episode subtitle satisfaction (ADR-0085) becomes a clean join rather than a
  filename guess, and the caption-serving stem match stays exactly as it is — the
  two now agree, one storing the fact the other infers.
- `item_id` is populated only for content acquired *after* this lands and through
  an item-scoped want. Pre-existing followed items, and everything scanned, have
  NULL — which reads as "no item asserted", the edition-fallback case. A backfill
  that links existing assets to their items (by the same acquisition knowledge,
  replayed, or a careful key match) is the follow-up that makes the *primary*
  item want precise too, and that also lets ADR-0084's extraction backfill stop
  being edition-coarse.
- `RecordExtractedSubtitle` (ADR-0084) can propagate the source video's `item_id`
  to the extracted subtitle it attaches, so an extracted caption is linked to the
  same episode as its video with no new knowledge — done where the source asset
  is already read.
- The content spine finally has the edge ADR-0056 drew in the model but not in
  the schema. It stayed undrawn for eleven milestones because nothing asked a
  per-item question of a shared edition; subtitles are the first, and will not be
  the last.
