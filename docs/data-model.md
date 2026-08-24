# Data model

The Milestone 1–2 slice of spec §79. The authority is
[the specification](spec/heyarr-spec.md); this page explains the shape and,
more importantly, **which guarantees apply to which tables**.

## The spine

```
Work ── Edition ── Asset ── Blob
 │         │         │        └── bytes, immutable, BLAKE3-addressed
 │         │         └── a usable local representation
 │         └── a specific version, cut or release
 └── the conceptual work
```

## Three authority models, in one database

Spec §8 separates these deliberately, and conflating them is how a distributed
system acquires a consistency bug it cannot debug.

| Authority | Tables | Rules |
|---|---|---|
| **Controller-authoritative** | `libraries`, `library_roots`, `principals`, `api_tokens`, `peers` (membership) | Single-writer. Changed only by the controller (ADR-0003). Never replicated live. |
| **Convergent content state** | `blobs`, `replicas` | No primary replica (§8). A destination always verifies bytes itself and never trusts a claimed hash (§21). |
| **Catalog** | `works`, `editions`, `external_ids`, `assets` | Controller-authoritative today. Milestone 4 gives peers a read-only materialised snapshot (§52), which is explicitly **not** independently writable. |

`scanned_files` is neither — it is a local cache of what the scanner last saw,
and deleting it costs a rescan and nothing else.

## Asset source classes (ADR-0020)

| | `managed` | `linked` | `vault` |
|---|---|---|---|
| Bytes live | in the CAS | at a path Heyarr does not own | in the CAS, encrypted |
| `blob_hash` | set | **NULL** | set (hash of the ciphertext) |
| `source_path` | provenance only | **required** — where the bytes are | provenance only |
| Replicated, verified, GC'd | yes | never | yes |

The invariant is a `CHECK` constraint rather than a caller's responsibility:

```sql
CHECK ((source_class = 'linked' AND blob_hash IS NULL AND source_path IS NOT NULL)
    OR (source_class IN ('managed','vault') AND blob_hash IS NOT NULL))
```

A linked asset having **no blob at all** is what keeps §14's immutability
absolute. Replication, placement, integrity and GC need no special cases,
because they operate on blobs and for a linked asset there is nothing to operate
on. Milestone 1 only ever writes `managed`.

## Decisions worth knowing before adding a table

**Everything is `STRICT`.** SQLite's default affinity will happily store the
string `'banana'` in an `INTEGER` column. A catalog that silently accepts
nonsense is worse than one that rejects it.

**Per-content-type fields go in `attributes` JSON, not columns.** §12 lists
thirteen specialisations; the failure mode this avoids is a `works` table with
forty nullable columns. Registering a fourteenth content type must not be a
migration, and a test asserts it isn't.

**Blob hashes are format-checked at the boundary.**
`CHECK (hash GLOB 'blake3:[0-9a-f]*' AND length(hash) = 71)`. The primary key is
the canonical byte identity (ADR-0005), so a malformed one must never enter the
catalog.

**Exactly one peer may be `is_self`,** enforced by a partial unique index. Two
rows claiming to be this node is unrecoverable once replication has run.

**`assets.blob_hash` is `ON DELETE RESTRICT`.** Bytes are never removed out from
under a live asset; deletion is logical and GC reclaims later (ADR-0018).

**`peers` and `replicas` exist with exactly one peer** (ADR-0010), so Milestone
4 is a protocol addition rather than a schema migration plus a rewrite of every
read-path query.

## Not here yet

`desired_items`, `quality_profiles`, `providers`, `releases`, `acquisitions`,
`download_jobs`, `artifacts` (Milestone 3); `devices`, `delegations`, `grants`
(Milestone 8); `encrypted_spaces`, `wrapped_keys`, `private_state_heads`
(Milestone 9); `jobs`, `events` (M1-05 and M1-06, alongside this).

Milestone 5's chunk tables have landed and are no longer on that list:
`chunk_manifests` (one row per blob that has a manifest, carrying the chunker
parameters and the manifest's own digest), `manifest_chunks` (the ordered chunk
sequence — the rows ARE the manifest) and `local_chunks` (this node's index of
which chunks it already holds and inside which blob, which is the question chunk
reuse asks). `blobs.chunked` survives as a deprecated boolean computed from
them; the field to read is `chunk_manifest`, which can say all three of §16's
answers (ADR-0034).
