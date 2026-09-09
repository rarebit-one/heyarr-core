## heyarr subtitles backfill

Extract embedded subtitles from already-ingested video

### Synopsis

Queue an embedded-subtitle extraction for every managed video in scope whose
Edition has no subtitle yet.

Extraction otherwise runs at INGEST (ADR-0084), so a video ingested before that
existed — or before this node had ffmpeg — has embedded tracks that were never
lifted out, and a rescan will not fix it (the bytes are unchanged, so the ingest
deduplicates and enqueues nothing). This is the one-time lever to reprocess that
existing content.

A scope is required so "this one show" is never one forgotten flag from "every
video on the node":

  heyarr subtitles backfill --work <work-id>
  heyarr subtitles backfill --library <library-id>
  heyarr subtitles backfill --all

Only videos with no subtitle are touched, and the extraction is a no-op on a
video with no embedded TEXT tracks — so a re-run is cheap and safe.

```
heyarr subtitles backfill [flags]
```

### Options

```
      --addr string         where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --all                 every video on this node missing subtitles
      --json                emit machine-readable JSON
      --library string      only videos in this library id
      --timeout duration    how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string        bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string   read the bearer token from this file (default: <data_dir>/cli.token when it exists)
      --work string         only videos in this work id
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr subtitles](heyarr_subtitles.md)	 - Subtitle operations
