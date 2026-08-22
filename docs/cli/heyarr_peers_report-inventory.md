## heyarr peers report-inventory

Tell a controller what this node's content store actually holds

### Synopsis

Report this node's inventory to a controller (§19, §20, ADR-0029, ADR-0033).

`replicas` on the controller is what the CONTROLLER believes. This node's
inventory is what is on its DISK. This command is where the two are compared,
and the controller's table is corrected to match the disk — including
downwards: a blob this node no longer holds becomes a `missing` replica rather
than staying present or quietly vanishing.

The inventory is derived from the content store, never from this node's
catalog. Quarantined blobs are reported as `corrupt` — the bytes are here and
cannot be served, and both "present" and "gone" are lies about that.

The credential is this node's Ed25519 peer identity, the same one `peers attach` presents. The report declares which peer this node believes it is, and the
controller compares that against the certificate: a peer reports its own
inventory and only its own.

By default the report is FULL — everything this node holds, so a blob it does
not mention is asserted absent. `--incremental` sends only what changed since the
last report in this process, which for a scheduled reporter is the ordinary
cycle; run without it to correct drift.

```
heyarr peers report-inventory <name-or-id> [flags]
```

### Options

```
      --addr string         where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --incremental         send only what changed since this process's previous report, rather than the whole set
      --json                emit machine-readable JSON
      --timeout duration    how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string        bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string   read the bearer token from this file (default: <data_dir>/cli.token when it exists)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr peers](heyarr_peers.md)	 - Inspect and manage the peers of this instance
