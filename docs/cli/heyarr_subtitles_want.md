## heyarr subtitles want

Request subtitles for held content that has none

### Synopsis

Create a subtitle want for every managed video in scope that holds no subtitle
in the requested language. The fetch driver then acquires each from a subtitle
provider (ADR-0085).

This is for a subtitle that exists NOWHERE — neither shipped beside the video nor
embedded in its container (subtitles backfill covers those). A scope is required
so "this one show" is never one forgotten flag from "every video on the node":

  heyarr subtitles want --work <work-id> --lang en
  heyarr subtitles want --library <library-id> --lang en
  heyarr subtitles want --all --lang en

Only videos with no subtitle in the language get a want, and an existing want is
left alone — so a re-run is cheap and safe.

```
heyarr subtitles want [flags]
```

### Options

```
      --addr string         where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --all                 every video on this node missing the subtitle
      --json                emit machine-readable JSON
      --lang string         subtitle language as an ISO-639-1 code (default "en")
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
