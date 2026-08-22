## heyarr peers show

Show one peer, by name or id, and how stale its catalog snapshot is

### Synopsis

Show one peer, including its materialised catalog snapshot (§52, M4-13).

The snapshot is the read-only copy of the controller's catalogue a Full Peer
keeps so that it has something to serve from when the controller is not
reachable (§53). This is where its VERSION and its AGE are reported, because
those are the two facts that decide whether it is worth anything: a snapshot
whose version has not moved in a week is one whose refresh has been failing in
silence.

A peer that has never built one reports "none", not version 0. Those are
different answers — "the library is empty" and "this peer cannot help you" —
and Milestone 7's degraded read path depends on nobody having collapsed them.

```
heyarr peers show <name-or-id> [flags]
```

### Options

```
      --addr string         where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
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
