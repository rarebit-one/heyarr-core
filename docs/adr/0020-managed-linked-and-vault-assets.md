# 0020. Managed, linked and vault assets

**Status:** Accepted
**Date:** 2026-08-19

## Context

Heyarr is built for mass media: content that exists in the world, has external
identity, is acquired, and is replaceable. Users also have personal media —
photos, home video, scans — and want to browse it alongside the acquired
library rather than in a second application.

The obvious implementation is to point Heyarr at a directory in the user's home
and treat those files as Blobs. That breaks the system: §14 makes Blobs
immutable, §61 rejects filesystem paths as canonical identity, and ADR-0006
makes paths ingest inputs only. A home directory is mutable by definition. A
"Blob" that changes underneath us makes `verify_blob` report corruption for a
legitimate edit, and an integrity alarm that cries wolf is worse than none.

But the instinct behind it is right. For personal media the file *is* the
artifact: people want it in Finder, in their camera roll, on their laptop — not
reachable only through an application's API.

## Decision

An Asset carries a **source class**, and the classes differ in what Heyarr
promises rather than in what it can show you.

| | `managed` | `linked` | `vault` |
|---|---|---|---|
| Bytes live | in the CAS | at a path Heyarr does not own | in the CAS, encrypted |
| Blob | yes | **none** | yes (of the ciphertext) |
| Catalogued, searchable, playable | yes | yes | client-side only |
| Replicated and converged | yes | no | yes |
| Integrity-verified | yes | no | yes |
| Eligible for garbage collection | yes | never | yes |

The load-bearing choice is that **a linked asset has no Blob at all**. Not a
mutable Blob, not a special Blob — none. Blob immutability therefore stays
absolute, and replication, placement, integrity and GC need no special cases:
they operate on Blobs, and there is nothing to operate on.

A linked asset records a path and a scan-time fingerprint. The fingerprint
detects change; it is explicitly not an identity and never addresses anything.

Promotion is one-way and explicit: ingesting a linked asset produces a managed
one, with all the guarantees that implies. There is no automatic promotion,
because "Heyarr quietly took custody of my photos" is not an acceptable
surprise.

`vault` is specified in ADR-0021.

## Consequences

**A linked asset is now the special case in five places, and this is a pattern
rather than five incidents.** It cannot be probed (M2-04), cannot carry
publication metadata (M2-08), cannot be planned against (M2-07), cannot be
verified (M1-16) — and as of Milestone 3 it cannot have its PLACEMENT evaluated
either, because placement is a question about blobs and there is no blob.

The fifth one is recorded here rather than solved because the honest answer is
a value: acquisition state carries `placement = not_applicable` for such an
asset, so it rests at `CONTENT_SATISFIED` permanently and can never reach
`FULLY_SATISFIED` (ADR-0027). Calling it satisfied instead — zero required
blobs are all present, which is vacuously true — would make `FULLY_SATISFIED`
mean "one copy, on one disk, with no integrity guarantee", which is the
opposite of what the name promises.

Milestone 5 owns the underlying question. What matters until then is that each
new subsystem notices the gap and expresses it, rather than quietly assuming a
blob and producing a wrong answer.

**Two read paths with opposite caching.** ADR-0013's `Cache-Control: immutable`
is correct for a hash-addressed Blob and wrong for a mutable path. Linked assets
need weak validators and revalidation. This is the easiest part of the feature
to get wrong and the failure is silent — users see stale content.

**Heyarr begins reading outside its data directory.** Library roots become an
allowlist, and path resolution must be validated against that allowlist *after*
symlink resolution, not before. This enlarges the surface §74's OS-level
containment has to cover.

**"Missing" splits in two.** A managed asset whose bytes are gone is an
integrity incident. A linked asset whose file is gone means someone moved it,
which is ordinary. Reporting them through one signal makes that signal useless.

**Linked assets exist on exactly one peer.** So the catalog must be able to say
"available at Bartley, not at Cove" *by design* rather than as a replication
failure. If Milestone 4's read routing (§32) is built assuming every asset is
convergent, this is an expensive retrofit — which is why the discriminator lands
in the Milestone 1 schema even though nothing reads it until later. Same
reasoning as ADR-0010.

**Scale differs by two orders of magnitude.** Thousands of films against
hundreds of thousands of photos. The scanner's fingerprint cache stops being an
optimisation and becomes the thing that makes a rescan finish.

## Alternatives rejected

- **Mutable Blobs for external files.** Discussed above; destroys the meaning of
  every integrity and replication guarantee in the system.
- **A separate application for personal media.** Defensible, and it is the right
  answer for a household that already runs one. But "browse everything in one
  place" is most of the value users are asking for, and a read-only catalog
  entry is a cheap way to provide it.
- **Automatic promotion of linked to managed.** Silently doubling someone's
  storage and taking custody of irreplaceable files is not a default.
