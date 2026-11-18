# 0124. A library is renamed in place; its id is its identity

**Status:** Accepted (2026-11-18)
**Date:** 2026-11-18

## Context

A library's name is fixed at creation. ADR-0083 made a library deletable, but
only an empty one. So a library whose name is wrong, and which already holds
content, cannot be corrected. The case that prompted this is a reference
host's live film library, which still carries the name of the verification
probe that created it (`verify-92`) while acquisitions import into it. Removing
its works to delete and recreate it would throw away the catalog rows, the
consumption history and every peer's view of those assets, just to change a
label.

Inside the catalog, nothing depends on the name. Assets, roots, scanned files
and holds all reference `libraries.id`. The name matters in three places
outside the catalog: the `libraries:` configuration block, reconciled by name
at every start (`getOrCreateLibrary`); `acquisition.peer_pull.libraries`,
which opts libraries in by name; and the `hold` op, which carries the origin
library's name for a sibling to match.

## Decision

`PATCH /api/v1/libraries/{id}` with `{"name": "..."}`, and
`heyarr library rename <library> <new-name>`.

**The id is the identity and the name is a label.** A rename changes one column
of one row. The id, roots, assets and everything that references them stay as
they are.

**The name is validated as at creation.** It is trimmed and must be non-empty.
The `UNIQUE` constraint stays the authority, so a name another library holds is
a `409`, the same refusal `library add` gives. `content_type` is not patchable.
Changing it would re-identify every work in the library, which is a migration,
not an edit, and the reconciler already refuses it for the same reason. The
strict decoder refuses it as an unknown field.

**`write`, not `admin`**, like the rest of library management (ADR-0083).

**On the log (invariant 7).** `content.library.renamed` carries the library id,
the new name and `previous_name`. A rename to the current name changes nothing
and emits nothing.

**Configuration is not rewritten.** The API does not edit the operator's
configuration file. A library declared there must be renamed there too, or the
next start creates a new, empty library under the old name. The same applies
to `acquisition.peer_pull.libraries` at both sites. The CLI help and the API
description say so.

## Consequences

- A misnamed library is corrected without touching its content.
- `content.library.renamed` is a new event type.
- A config-declared or peer-pull library has to be renamed in more than one
  place. That is the cost of those surfaces keying on a name an operator can
  read and type.
- **Revisit if** a config-declared library renamed only through the API turns
  out to be a common mistake. The fix then is for reconciliation to warn when a
  library it would create shares its roots with an existing library. Keying the
  configuration on ids would be the wrong fix, because nobody writes a UUID
  into a YAML file.

---

*Provenance: an operator's request to rename a reference host's movie library.
Follows ADR-0083's library-management conventions.*
