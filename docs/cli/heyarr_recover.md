## heyarr recover

Rebuild this peer's control plane from a surviving peer (§51, §82, M7-04)

### Synopsis

Rebuild THIS peer's control plane and identity from a backup a surviving
peer holds.

This does not "recover the system" — under the peer-repo model each peer is
authoritative for its own site, so what is rebuilt is one peer's control plane.
Two of the recovery inputs have no fetch path and are what this restores: the
control database, and this node's Ed25519 identity. The content store, the
encrypted personal state and the catalog snapshot re-fill themselves from the
fabric once the control plane is back — this command does NOT copy the CAS.

It needs this node's own identity key (kept aside from the lost data
directory), and the surviving peer's endpoint and public key (as `heyarr peers`
shows them). The backup was signed by this node, so its own key is what
verifies it — a recovery trusts a signature over its identity, not the peer that
serves the file.

By default this is a DRY RUN: it fetches and verifies the backup and reports
what a restore would do, touching nothing. To actually restore, pass
--confirm with this node's data directory, matched exactly — an unrecoverable
command that accepts a partial match is one that runs on a typo, and this one
overwrites a data directory. It refuses outright if the data directory is still
live.

```
heyarr recover [flags]
```

### Options

```
      --confirm string          the data directory to overwrite, matched exactly, to perform the restore (omit for a dry run)
      --from-endpoint string    the surviving peer's https peer-surface endpoint
      --from-key heyarr peers   the surviving peer's ed25519 public key (as heyarr peers shows it)
      --generation int          which backup generation to restore (0 = the latest the peer holds)
      --identity-key string     this node's identity key file, kept aside from the lost data directory
      --json                    emit machine-readable JSON
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
