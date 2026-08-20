## heyarr fsck

Check stored bytes against the catalog (§57, ADR-0018)

### Synopsis

Reconcile expected hashes against the bytes actually on disk.

A shallow check confirms every blob the catalog knows about exists and is the
right length. It is fast and it catches a deleted or truncated file, but it
cannot catch a file that was rewritten in place at the same length.

--deep re-hashes everything. That is the check that matters on a hardlink-
ingested library, where a blob shares its inode with the file it was adopted
from and an external tool writing to that file rewrites the blob. Any blob whose
bytes no longer hash to their own name is moved to quarantine and recorded —
never deleted, because on such a library the "corruption" may be the operator's
original (ADR-0018).

Bytes with no catalog row and partial writes are reported too, but they are
waste rather than damage. fsck exits non-zero only for damage.

```
heyarr fsck [flags]
```

### Options

```
      --deep   re-hash every blob instead of checking existence and length
      --json   emit machine-readable JSON
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
