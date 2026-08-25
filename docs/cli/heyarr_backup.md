## heyarr backup

Take a whole-database backup of this peer's control plane (§49, ADR-0044)

### Synopsis

Write a backup of the control database that a restore can open.

This is host administration, like fsck and gc: it talks to the database
directly rather than through the API, so it works whether or not the controller
is running. The backup is a VACUUM INTO snapshot — a self-contained, consistent
database with no -wal sidecar, so there is no way to end up with a backup that
silently restored to an older state than it was taken from.

Each backup carries its own provenance: which peer's control plane it is, a
monotonic generation (the control plane's event high-water mark, so a backup of
unchanged state reports the same generation), the schema version, and when the
database was read. When this peer's identity key is present, the manifest is
signed with it, so a peer that later holds this backup can verify it came from
here rather than from whoever handed it over.

Provider credentials are never in the database (they live in the config file),
so a backup cannot carry them — it records that omission rather than surprising
a restore with it.

```
heyarr backup [flags]
```

### Options

```
      --json   emit machine-readable JSON
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
