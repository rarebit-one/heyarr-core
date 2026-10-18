# 0113. A space change names its kind, and a drive entry carries its own metadata

**Status:** Proposed (2026-10-18)
**Date:** 2026-10-18
**Supersedes clause:** ADR-0095 "What would make us revisit — rich per-version metadata grows the entry"
**Builds on:** ADR-0021 (encrypted vault content), ADR-0095 (drive CRDT), ADR-0097 (frame codec + manifest), ADR-0101 (change parents), ADR-0107 (Mnemosyne)
**Tracks:** #774 (part of #539, #733)

## Context

Device clients are building a files navigator, ingest lanes that route a phone's
photos into a space, and per-device sync choices (mirror a space, stream it on
demand, or leave it). Three gaps in the client-side encoding block them. Every one
lives inside change ciphertext; the server's view is unchanged.

1. **A space's changes do not say what CRDT they belong to.** A space was assumed
   to hold one kind. In practice a vault space written by a folder-sync daemon is
   also where its owner's albums belong. A client folding the album CRDT over that
   log reads every drive change as a malformed album change (thousands of them), and
   an untagged album `SET_TITLE` (op 1) decodes as a drive delete at the empty path.
2. **A drive entry carries no metadata.** `{op, path, blob, size, mtime}` is all
   there is. A navigator cannot show a type, a capture date or a thumbnail without
   fetching and opening every manifest, which on a 50 GB vault means thousands of
   round trips before the first screen.
3. **ADR-0095's per-device "available offline" set has no encoding.**

## Decision

1. **A change names its kind in a plaintext `k` field** inside the ciphertext:
   `drive`, `album`, or `policy`. Writers set it on every new change. An untagged
   change is legacy and is read by shape, which is a pure function of its bytes: a
   change with a `path` is a drive change, and one with an `album_id` is an album
   change. **A fold skips a change of another kind silently.** It is not
   malformed, and it is never a head that a write of this kind parents on. One space
   can therefore hold a drive, its albums and policy records without any server
   change.
2. **A drive change rejects an empty path.** Before this, a stray album change
   could tombstone `""`.
3. **A drive PUT may carry optional metadata.** The field names are
   `PhotoManifest`'s, so the two models share one vocabulary:

   | Field | Meaning |
   | --- | --- |
   | `mime` | content type |
   | `plaintext_hash` | dedupe key |
   | `taken_at` | capture time, unix seconds |
   | `width`, `height` | pixel dimensions |
   | `duration_ms` | media duration |
   | `thumbnail` | sealed blob id, kept small enough for one whole-blob read |
   | `sidecar` | sealed EXIF blob id |

   - **The metadata belongs to the write.** It travels in the write set and the
     snapshot, and an entry reports its current head's metadata.
   - **Adding metadata later is a PUT of the same manifest id**, built on the
     current head, so nothing is uploaded again.
   - **A malformed blob id rejects the change**, as `PhotoManifest.Validate`
     does. A delete carries no metadata.
   - **Absent fields are omitted**, so a drive with no metadata snapshots to the
     same bytes as before.
4. **Albums reference drive entries by manifest id.** An album item's
   `manifest` already names the same ADR-0097 object a drive entry's `blob` does,
   so putting a drive photo in an album never uploads it again.
5. **A device's sync choices are `k:"policy"` records** in the owner's home
   space. Each is a last-writer-wins map keyed by device id, holding:
   - a mode for each space: `full`, `on_demand` or `off`, plus optional subtree
     include/exclude lists;
   - the device's keep-offline set;
   - its ingest lanes.

   Any of the owner's devices can read or edit any device's record; the server
   sees ciphertext. The wire shape is pinned by golden vectors when the first
   client builds it. This ADR fixes only its kind and its home.

6. **A snapshot of a space that holds more than one kind is a composite.** The
   server keeps one latest snapshot per space and compacts against it (§44), so
   a snapshot that held one kind would let compaction strand the others. A
   composite works like this:
   - it carries every kind's state that the writer folds;
   - it carries **verbatim** the plaintext of every change the writer cannot
     fold (an unknown kind);
   - its frontier is all the space's heads.

   A reader takes its own kind's part and replays the verbatim changes of
   kinds it knows. Nothing is lost whichever client wrote the snapshot, and the
   one-latest-snapshot model stays correct. A client that cannot write a
   composite must not snapshot such a space. `heyarr space snapshot`, which
   folds only playlists, now refuses one. The composite's wire shape is pinned
   by golden vectors when the first client snapshots a space with several
   kinds; none does yet.

These are client encodings. **No route, table or server behaviour changes**, and
Invariant 6 holds as before. Where copies of a blob live across sites stays
ADR-0096's and #662's concern. A device's local holdings are never told to the
server.

## Consequences

- Readers that predate `k` ignore it and keep working. The Go drive fold
  already skipped an untagged album change as malformed, except op 1, which this
  ADR closes.
- The drive gets cross-language golden vectors (`testdata/vectors/drive.json`),
  the parity discipline the other kinds already have.

## What would make us revisit

- **A fourth kind sharing a space.** It should get its `k` here, not a new space
  type.
- **A composite snapshot that costs too much to download.** That would be the
  case if a reader that needs one kind routinely downloads a much larger
  other kind. Snapshot lanes per kind would fix it: the server keeps a latest
  snapshot per (space, opaque lane) and compacts per lane. That is a server
  change, and it reveals how many kinds a space holds.
- **Metadata that must be private from other members of a shared space.** Every
  member who can open the space reads every entry's metadata, which is the same
  boundary as the bytes.
