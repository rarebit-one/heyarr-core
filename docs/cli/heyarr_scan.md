## heyarr scan

Scan a library's roots, optionally waiting for the scan to finish

### Synopsis

Enqueue a scan of every enabled root of a library.

The library may be named by id or by name. One job is enqueued per root, so a
library with three roots produces three jobs — and --wait waits for all of
them, because exiting 0 once the first finished would report success for a scan
that is a third done.

--wait exits 0 only when every job succeeded. If any job reaches dead it exits
non-zero and prints the last error, because a CLI that exits 0 when the work
failed is worse than no CLI: it will be put in a script, its output will stop
being read, and its silence will be trusted.

It waits by subscribing to the event stream before reading the jobs' state,
never the other way round: a job that finishes in between would otherwise be
waited on forever. A job that is already finished when the wait starts returns
immediately.

```
heyarr scan <library> [flags]
```

### Options

```
      --addr string              where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --json                     emit machine-readable JSON
      --poll-interval duration   how often --wait re-reads job state as a backstop to the event stream; 0 waits on events alone (default 1s)
      --timeout duration         how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string             bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string        read the bearer token from this file (default: <data_dir>/cli.token when it exists)
      --wait                     wait for every enqueued scan job to finish
      --wait-timeout duration    give up waiting after this long (default: wait indefinitely)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
