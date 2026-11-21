## heyarr space compact

Pin the snapshot as the compaction base and drop what every peer confirmed (§44)

### Synopsis

Compact the space's change log against the snapshot a cold-starting device
would be offered, so a long-lived space is not an unbounded log.

That snapshot is pinned as the space's compaction base: devices prefer it for a
cold start, and replication carries it to every trusted Full Peer. A change is
dropped only if the base subsumes it AND every trusted Full Peer has confirmed
holding both the change and this base, as recorded by this node's audited
reconcile. Until each has, nothing is dropped and the peers are listed; run it
again after the next replication (`POST /api/v1/state/replicate` audits at
once).

Before compacting, this device opens the space and verifies that the snapshot's
frontier is sealed inside its ciphertext (#681): a frontier anyone with a write
token could have rewritten would decide what is dropped. --unverified skips that
check, for an operator without a key to the space; the peer still compacts only
what every trusted Full Peer confirmed.

```
heyarr space compact <space-id> [flags]
```

### Options

```
      --addr string               where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --json[=true|false]         emit machine-readable JSON
      --timeout duration          how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string              bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string         read the bearer token from this file (default: <data_dir>/cli.token when it exists)
      --unverified[=true|false]   compact without opening the space to verify the snapshot's frontier
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOID_WHICH_BINDS_DEVICE_DIR overrides)
```

### See also

* [heyarr space](heyarr_space.md)	 - Create and read encrypted personal-state spaces (§38, §42, ADR-0049)
