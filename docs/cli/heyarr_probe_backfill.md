## heyarr probe backfill

Re-probe video whose probe cannot say whether it is HDR

### Synopsis

Queue a probe for every managed video in scope whose stored probe records
neither a transfer characteristic nor a Dolby Vision record on its picture,
including videos never probed at all.

A probe runs at INGEST and the bytes never change, so a rescan enqueues
nothing. Probes taken before colour was recorded (#885, #886) read as SDR,
which steers an SDR screen onto an HDR copy and re-encodes it without a
tone-map. This is the one-time lever to re-read them.

A scope is required so "this one film" is never one forgotten flag from "every
video on the node":

  heyarr probe backfill --work <work-id>
  heyarr probe backfill --library <library-id>
  heyarr probe backfill --all

A probe that already answers is left alone, so a re-run is cheap. SDR video
whose container declares no colour at all is re-probed each run.

```
heyarr probe backfill [flags]
```

### Options

```
      --addr string         where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --all                 every video on this node whose probe does not record colour
      --json                emit machine-readable JSON
      --library string      only videos in this library id
      --timeout duration    how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string        bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string   read the bearer token from this file (default: <data_dir>/cli.token when it exists)
      --work string         only videos in this work id
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr probe](heyarr_probe.md)	 - Media probe operations
