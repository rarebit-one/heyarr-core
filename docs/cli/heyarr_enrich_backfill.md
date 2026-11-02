## heyarr enrich backfill

Enrich held works now, ignoring the background cadence

### Synopsis

Queue an enrich job for every held work in scope that is still under-enriched:
an album or book without a cover or canonical id, a film or series without a
poster, a backdrop or its TMDB id.

Enrichment otherwise runs on a background beat that paces itself with a backoff
schedule (ADR-0087). This is the one-time lever to enrich a scope NOW — after
wiring an enrich provider, or after ingesting a shelf of books.

A scope is required so "this one author" is never one forgotten flag from "every
book on the node":

  heyarr enrich backfill --work <work-id>
  heyarr enrich backfill --author "Reads"
  heyarr enrich backfill --library <library-id>
  heyarr enrich backfill --all
  heyarr enrich backfill --all --content-type movie

--content-type narrows any scope to one kind (movie, series, music, book) — the
films, after wiring TMDB, without re-asking Open Library about every book.

Only under-enriched works are touched, the job fetches only the images a work is
missing, and it dedupes per work — so a re-run is cheap and safe.

```
heyarr enrich backfill [flags]
```

### Options

```
      --addr string           where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --all                   every under-enriched work on this node
      --author string         only works filed under this author/artist
      --content-type string   only works of this type: movie, series, music or book
      --json                  emit machine-readable JSON
      --library string        only works in this library id
      --timeout duration      how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string          bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string     read the bearer token from this file (default: <data_dir>/cli.token when it exists)
      --work string           only this work id
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr enrich](heyarr_enrich.md)	 - Enrich held works with artwork, descriptions and canonical ids
