## heyarr gc

Reclaim bytes nothing references (ADR-0018)

### Synopsis

Reclaim blobs that no asset references, plus orphaned partial writes and
bytes in the store with no catalog row.

This command changes nothing unless you pass --apply. That is the default
because a garbage collector is the one piece of Heyarr whose bugs are not
recoverable by re-running it.

Reclamation is two-pass. The first sweep that sees a blob with no references
records when it noticed and reclaims nothing; a later sweep, once the grace
window has passed, frees the bytes. So a mistaken delete stays reversible for
the length of the window, and a blob that regains a reference gets a fresh
window rather than a partly spent one.

```
heyarr gc [flags]
```

### Options

```
      --apply                 actually reclaim; without it nothing is changed
      --dry-run               report what would be reclaimed without doing it (the default) (default true)
      --grace duration        how long a blob must have been unreferenced before its bytes may be freed (default 168h0m0s)
      --json                  emit machine-readable JSON
      --temp-grace duration   how old a partial write must be before it is swept (default 24h0m0s)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
