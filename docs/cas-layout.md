# CAS on-disk layout

**Layout version 1.**

This is private to `internal/storagefabric/cas`. Nothing outside that package may
assume any of it — the content domain cannot even import the package
(ADR-0006, ADR-0007), enforced by `depguard`. It is documented so that an
operator can reason about a directory on their disk, and so a layout change is
visible in review rather than discovered in production.

```
<cas_root>/
  HEYARR_CAS                                  marker; see below
  blobs/blake3/aa/bb/aabb…<64 hex>            mode 0440
  tmp/put-<random>.part                       in-flight writes
  quarantine/<hex>.<unix-nanos>               failed verification
```

## The marker

```json
{
  "version": 1,
  "algo": "blake3",
  "fanout": [2, 2]
}
```

A root whose `version` is **higher** than the binary understands is refused
rather than read. Same reasoning as the schema downgrade guard (ADR-0003): an
old binary interpreting a new layout does not fail loudly, it silently does the
wrong thing, and by the time anyone notices the damage predates every backup.
A root recorded with a different hash algorithm is refused for the same reason.

## Fanout

Two levels of two hex characters, taken from the front of the digest. At most
65 536 leaf directories — flat enough that `find` and the filesystem's directory
index stay usable, deep enough that no single directory holds a million entries.

The full hex digest is also the filename, so a path is self-describing and
`Walk` can recover a blob's identity from its name alone.

## Why writes go through `tmp/`

`Put` streams into `tmp/` while hashing, then renames into place. The rename is
atomic because it stays on one filesystem, so **a process killed mid-write
leaves nothing addressable** — only a reapable `.part` file. A half-written file
sitting at a content address would be corruption that looks exactly like data.

Both the file and its parent directory are `fsync`ed before the write is
reported as done. Syncing only the file lets a crash lose the rename while
keeping the contents, which leaves the catalog referencing a blob the filesystem
never recorded.

`ReapTemp` removes abandoned `.part` files; nothing else will.

## Deduplication is a consequence, not a feature

The path is derived from the content, so identical bytes cannot occupy two
files. `Put` notices the target already exists, discards its temporary file and
reports `Deduplicated`.

## Materialisation ladder

`Link` tries copy-on-write cloning, then a hardlink, then a byte copy
(ADR-0014), and records which rung it used.

**A deduplicating `Link` records `none`, not a rung.** The store already held
the bytes, so nothing was materialised and no rung was reached — `Deduplicated`
is the field that says what happened. Reporting the rung it was *asked* for
made every deduplicating ingest on a filesystem without block cloning claim
`reflink`, the one value that filesystem can never produce, in the same run
where the ingests that actually moved bytes reported `copy` (#223). `materialised`
is what an operator greps to check the ladder is reaching the rung they paid
for, so it must never assert an outcome no operation reached.

**Measured**, in `TestReflinkCostsMetadataNotBytes`:

| | Disk consumed for a 256 MiB file |
|---|---|
| Clone (APFS `clonefile`) | **0 KiB** |
| Ordinary copy | 262,168 KiB |

That is the difference between adopting an existing library in place and asking
the operator to double their storage first.

⚠️ **Measure free space, not `du`.** On APFS `du` reports the full logical size
for every clone, so it shows 100% "growth" for an operation that consumed
nothing. The first version of that test believed `du` and reported a failure
that was not real.

Support: btrfs, XFS with `reflink=1`, ZFS 2.2+ with block cloning enabled, and
APFS. Elsewhere `Link` degrades — an unsupported filesystem is ordinary, not a
failure.

⚠️ A **hardlink** shares the inode with the source, so an external tool writing
in place would corrupt the blob. That is what integrity scanning is for, and why
corrupt blobs are quarantined rather than deleted — the "corruption" may be the
original file legitimately changing.

## Quarantine, not deletion

`Verify` re-reads a blob and confirms it still hashes to its own name. On
mismatch it moves the file to `quarantine/` and returns `ErrCorrupt`. It is
never deleted: it may be the only copy, and it is evidence (ADR-0018).

Nothing prunes `quarantine/` automatically. A quarantined blob is a thing an
operator should have to look at.
