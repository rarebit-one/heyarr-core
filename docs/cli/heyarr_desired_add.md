## heyarr desired add

Want something

### Synopsis

Want content, creating the Work if Heyarr has never seen it.

  heyarr desired add "The Conversation" --content-type movie --year 1974 \
      --quality-profile living-room

Pass --work-id instead of a title to want something already in the catalog.

Two wants over the same content with DIFFERENT profiles are legal and are the
point — the living-room copy and the phone-sized copy are two wants. The same
content under the same profile twice is one want written twice, and is refused.

```
heyarr desired add <title> [flags]
```

### Options

```
      --addr string              where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --content-type string      what this is: movie, series, music, book (required when wanting by title)
      --edition-id string        want one edition — a season, a particular release — rather than the whole work
      --json                     emit machine-readable JSON
      --no-monitor               get it once and stop — do not keep looking for something better
      --quality-profile string   the profile this want is measured against, by name (required)
      --reason string            a note to your future self
      --timeout duration         how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string             bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string        read the bearer token from this file (default: <data_dir>/cli.token when it exists)
      --work-id string           want something already in the catalog
      --year int                 the year, when it is part of the identity
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr desired](heyarr_desired.md)	 - Say what should exist, whether or not it does yet
