# 0071. An edition is deletable, logically, and a followed work refuses it

**Status:** Accepted (2026-09-03)
**Date:** 2026-09-03

## Context

#439 (ADR precedent: the work delete, `DELETE /works/{id}`) gave a client the
two ends of catalog removal — a single asset (`DELETE /assets/{id}`) and a whole
work — and offered the middle, `DELETE /editions/{id}`, as an explicit optional
follow-up. An edition is the grouping between the two: a season of a series, a
particular cut or pressing of a film. Until now a client that wanted to remove
one had to delete every asset under it by hand and leave the empty edition row
behind, or delete the entire work.

An edition is **scanner-recreatable** — the identifier a scan re-derives from
the files on disk re-creates the edition it belongs to — so removing one is
ordinary library management, the same class as removing an asset or a work.

## Decision

`DELETE /api/v1/editions/{id}` removes an edition, its assets, its byte-less
items and the wants scoped beneath it. It mirrors the work delete faithfully:

**`write`, not `admin`.** The same class of library management as the asset and
work deletes it sits between; requiring an admin token to tidy a season would
put an admin credential on every screen that offers the button.

**Logical, in ADR-0018's sense — no byte is unlinked (invariant 8).** The
catalog rows go; the blobs stay. The removal is one `DELETE FROM editions`, and
every child — the edition's assets and their consumption sessions, its items,
and the wants scoped to the edition or to one of its items — is already
`ON DELETE CASCADE`, so the database performs the removal it already knows how
to perform rather than the handler re-deriving the graph and eventually missing
a table somebody adds later. The existing `gc_blobs` sweeper reclaims a blob no
asset references any more, behind its grace window. Unlinking bytes inside a
request handler is the version of this feature where a bug is unrecoverable,
which is exactly what ADR-0018 exists to prevent.

**The parent work is untouched.** An edition is a subordinate grouping; deleting
it leaves the work and a rescan re-derives the edition from the files that
remain. This is the one substantive difference from the work delete, and it is
why the feature is safe to offer at `write`.

**A want is cascade-cancelled; a followed work refuses the delete.** This is the
work delete's distinction, applied at the edition's granularity. A want scoped
to the edition — or to one of its items — is a one-off intent that removing its
target supersedes, so it goes with the edition and its removal is emitted. A
follow source (00040) is standing configuration, and it anchors to the **work**,
not the edition: its projected items ARE the seasons this route removes.
Deleting an edition out from under a live subscription is therefore the same
surprise the work delete refuses — the next poll would silently re-materialise
what was just removed — so an edition whose work is still followed answers `409`
naming the fix (`DELETE /followed-sources/{id}`, which also decides what happens
to the archive). The check is on the edition's `work_id`, because that is where
a subscription lives; a work-scoped want over the parent, by contrast, still has
its target and is left alone.

**Every removal is on the log (invariant 7).** A `content.asset.deleted` per
removed asset — the same type and payload the per-asset route emits, so a
subscriber never special-cases "unless the whole edition went" — a
`desired.removed` per cancelled want, and one new `content.edition.deleted`
carrying the counts and `bytes_removed: false`.

## Consequences

- The middle of the catalog-removal API is filled: asset, edition, work. A
  client can tidy a season without deleting a series and without hand-removing
  every file under it.
- `content.edition.deleted` is a new event type. A subscriber that reacted to
  `content.work.deleted` and `content.asset.deleted` now has a third, and the
  per-asset events it already handles fire beneath it unchanged.
- The followed-work refusal is broad on purpose: an edition of a followed series
  cannot be tidied while the follow stands, even though the subscription would
  survive the delete. The alternative — letting the delete through and having
  the next poll re-create the edition — is churn an operator did not ask for and
  cannot see. **Revisit if** a caller has a real need to prune a season of a
  followed series without unfollowing it; the honest fix then is a per-item or
  per-edition exclusion on the follow source, not a silent delete-and-rebuild.

---

*Provenance: the optional follow-up offered in #439's work delete, built on
request. Implemented in the same change as this record.*
