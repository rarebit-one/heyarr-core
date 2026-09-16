## heyarr desired reingest

Re-drive a wedged ingest — a finished download that never imported

### Synopsis

Re-run the hash-and-import for a want stuck in VERIFYING or INGESTING.

A download that completed but whose ingest was lost — a worker crash, a node
OOM, a client that dropped the completed transfer before the ingest ran — sits
with no job driving it and never advances. This queues that import again; the
ingest worker re-locates and re-verifies the bytes itself, so nothing else is
needed. Idempotent: an ingest that is actually running is left alone.

The stuck-ingest watchdog does this automatically once a want has been wedged
past a grace window; this is the manual lever for doing it now.

```
heyarr desired reingest <id> [flags]
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

* [heyarr desired](heyarr_desired.md)	 - Say what should exist, whether or not it does yet
