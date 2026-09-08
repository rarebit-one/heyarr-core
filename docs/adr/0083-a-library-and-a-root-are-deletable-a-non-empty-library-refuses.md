# 0083. A library and a root are deletable; a non-empty library refuses

**Status:** Accepted (2026-09-08)
**Date:** 2026-09-08

## Context

#228 recorded a gap found by driving Heyarr for real: every library ever
created is permanent. `heyarr library` is `add` and `list`; the routing table
has `POST /libraries`, `POST /libraries/{id}/roots` and a `scan`, and no delete
for either a library or a root — a quality profile can be deleted, a work can be
deleted (#428, ADR-0071), a library cannot. A library made by a typo, and the
throwaway probe libraries a verification session leaves behind, are in every
`library list` forever, and a mis-typed `content_type` (#227) cannot be
corrected because the library that carries it cannot be removed.

The concrete consumer is thesim-family/homelab-ops#485: a reference host
carrying three throwaway libraries (`verify-92`, `hardlink-probe`, a duplicate
`shows-downloads`). Since #452 their *works* can be removed one by one
(`DELETE /works/{id}` + `gc`), which empties them — but the empty library and
root rows remain, and nothing can remove them.

#228 flagged one decision as needing to be made first: **what does deleting a
library mean for its assets?** The assets schema hints one answer —
`library_id TEXT REFERENCES libraries (id) ON DELETE SET NULL` — which would
orphan a library's assets from their library while keeping them managed. "Remove
the library, keep the content" and "remove the library and everything it brought
in" are both defensible and lead to very different sweeps.

## Decision

`DELETE /api/v1/libraries/{id}` and `DELETE /api/v1/libraries/{id}/roots/{rootID}`.

**`write`, not `admin`.** The same class of library management as the asset,
edition and work deletes (ADR-0071): requiring an admin token to tidy a library
would put an admin credential on every screen that offers the button.

**Logical, in ADR-0018's sense — no byte is unlinked (invariant 8).** The
catalog rows go; the blobs stay for the `gc_blobs` sweeper to reclaim behind its
grace window. Each delete emits its event with `bytes_removed: false`.

**A non-empty library refuses (409); it does not cascade its content.** This is
how the "decision that has to be made first" is resolved: not by choosing
between the two meanings and hard-wiring one into a single verb, but by refusing
the ambiguous case. `assets.library_id` is `ON DELETE SET NULL`, so a raw
`DELETE FROM libraries` does *not* refuse — it silently strips a whole library's
assets of the library they belonged to. So the handler counts the library's
assets first and refuses with a `409` naming the fix
(`DELETE /api/v1/works/{id}`) when any remain. What the route then performs is
the one unambiguous operation: an **empty** library — one whose content was
already removed per-work — goes together with its roots, which are
`ON DELETE CASCADE`, in one `DELETE FROM libraries`. This is exactly what #485
needs, because its libraries are already empty.

The cascade meaning ("remove the library and everything it brought in") is
deliberately **not** built here. A caller that wants it composes it out of parts
that already exist and each emit their own removals — delete the works, then the
empty library — rather than getting a whole library's content removed by one
verb whose blast radius is not visible at the call site. **Revisit if** a real
need for a single atomic "library and contents" delete appears; the honest form
then is an explicit, opt-in mode (`?cascade=…`), not a default.

**Removing a root is unconditional and always safe.** An asset references its
**library**, never a root — there is no `root_id` on assets; only
`scanned_files.root_id` references a root, `ON DELETE CASCADE`. So removing a
root orphans no ingested content: it stops Heyarr scanning that directory and
drops the scanned-file bookkeeping a remaining root's rescan would rebuild
anyway. A root is addressed under its own library
(`/libraries/{id}/roots/{rootID}`), so a root id belonging to another library is
a `404`, never a cross-library delete.

**Every removal is on the log (invariant 7).** `content.library.deleted`
carrying the library's name, its root count and `bytes_removed: false`; and
`content.library_root.removed` — the counterpart to the existing
`content.library_root.added` — carrying the library id, root id and path.

**The CLI catches up (#228 items 1–2).** `heyarr library rm <library>`,
`heyarr library root add <library> <path>` (the second root of a library, and
every one after, without a second library over the same tree) and
`heyarr library root rm <library> <root>`, resolving a library by id or name and
a root by id or exact path.

## Consequences

- The homelab-ops#485 throwaway libraries can be removed once emptied, and a
  mis-typed `content_type` library (#227) can be deleted and recreated correctly
  rather than living "wrong forever".
- `content.library.deleted` and `content.library_root.removed` are new event
  types. A subscriber that reacted to `content.library.created` /
  `content.library_root.added` now has their removal counterparts.
- A library delete cannot, by itself, remove content — an operator with a full
  library to discard runs the per-work deletes first. That is a deliberate two
  steps rather than one, chosen so that no single call silently removes more
  than the row it names.
- `quality-profile create` from the CLI (#228 item 3) is a separate, smaller
  gap and is left to its own change.

---

*Provenance: #228, driven by thesim-family/homelab-ops#485. Mirrors the work and
edition deletes (#428/ADR-0071, #439/ADR-0071) and ADR-0018's logical-delete
invariant.*
