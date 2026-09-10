## heyarr enrich backfill

Enrich held music/book works now, ignoring the background cadence

### Synopsis

Queue an enrich job for every held music/book work in scope that still lacks a
cover or a canonical id.

Enrichment otherwise runs on a background beat that paces itself with a backoff
schedule (ADR-0087). This is the one-time lever to enrich a scope NOW — after
wiring an enrich provider, or after ingesting a shelf of books.

A scope is required so "this one author" is never one forgotten flag from "every
book on the node":

  heyarr enrich backfill --work <work-id>
  heyarr enrich backfill --author "Reads"
  heyarr enrich backfill --library <library-id>
  heyarr enrich backfill --all

Only works missing a cover or an id are touched, and the enrich job is idempotent
— so a re-run is cheap and safe.

```
heyarr enrich backfill [flags]
```

### Options

```
      --addr string         where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --all                 every under-enriched music/book work on this node
      --author string       only works filed under this author/artist
      --json                emit machine-readable JSON
      --library string      only works in this library id
      --timeout duration    how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string        bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string   read the bearer token from this file (default: <data_dir>/cli.token when it exists)
      --work string         only this work id
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr enrich](heyarr_enrich.md)	 - Enrich held music and book works with covers and canonical ids
