# 0127. A library type Heyarr does not understand is retyped by its operator

**Status:** Accepted (2026-11-20)
**Date:** 2026-11-20

## Context

A library's `content_type` was checked only for being non-empty until #227
(the API) and #896 (the configuration). Rows created before then keep what they
were given. The one spelling on record is `show`. Such a library is identified
as if it declared no type, with every rule and the movie rules first, and
acquisition never chooses it as a target for a wanted series. #917 made these
rows visible: a warning at start, and `(unknown)` in `heyarr library list`. It
did not give them a way out. The reconciler refuses a content-type change, and
ADR-0083 deletes only an empty library, so a non-empty legacy library could be
corrected only by removing every work and recreating it.

#896 set out three options: a one-shot `show` → `series` migration, an explicit
re-type allowed only from an unknown type, or detection alone. The maintainer
chose the explicit re-type.

## Decision

`POST /api/v1/libraries/{id}/retype` with `{"content_type": "..."}`, and
`heyarr library retype <library> <content-type>`.

**One-way: from an unknown type to a known one.** The target must pass
`identification.IsContentType` (`400` otherwise, worded as `POST /libraries`
words it). A library whose current type is already known, and different, is
refused with `409`. That is a real content-type change. It re-identifies every
work in the library, which is a migration rather than a correction, and the
reconciler refuses it for the same reason.

**The operator names the type.** Heyarr does not guess that `show` meant
`series`. This keeps the stance `createLibrary` records: a refused type is not
normalised, because a guess that is right for `show` is wrong for `tv` or a
typo.

**An action route, not a PATCH field.** ADR-0124 made `PATCH
/libraries/{id}` rename only, with `content_type` refused as an unknown field.
A field that is accepted only when the stored value is unknown would make the
PATCH conditional on state the caller cannot see in the body. A named action
also says in the route that this is a correction, not an edit. `PATCH` is
unchanged.

**Retyping to the current type is a `200` with no event.** Nothing
transitioned, as with a rename to the current name, and the operator's command
is safe to run twice.

**`write`, not `admin`**, like the rest of library management (ADR-0083,
ADR-0124).

**On the log (invariant 7).** `content.library.retyped` carries `library_id`,
`name`, `content_type` and `previous_content_type`.

**Only the column changes.** Roots, assets and works stay as they are. Works
already identified under the old type are not re-identified. That is a rescan or
re-identify question of its own.

**Configuration converges and is not rewritten.** A `libraries:` block that
says the new type while the row says the old one is refused at start, and the
refusal names `library retype`. Once the row is retyped, the same block
reconciles onto it.

## Consequences

- A legacy library is corrected in place without touching its content.
- `content.library.retyped` is a new event type, and the API gains one additive
  operation.
- A node whose configuration already declares the corrected type will not
  start until the row matches. The operator removes the library from the
  configuration, starts, retypes, and restores it. The refusal says so.
- **Revisit if** `show` turns out to be common enough that a one-shot migration
  to `series` is worth an ADR of its own, or if a real need appears to move
  between two known types. That would be a migration with re-identification,
  not an extension of this route.

---

*Provenance: #896, option 2, chosen by the maintainer. Follows ADR-0083's and
ADR-0124's library-management conventions.*
