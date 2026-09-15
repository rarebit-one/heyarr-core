## heyarr quality-profile set

Change an existing quality profile's rules or description (§62)

### Synopsis

Change a quality profile in place, named the way "list" shows it — by name or id.

Only the parts you pass change. A rule group you OMIT is left as it was; a group
you pass as an explicit empty array is CLEARED. Those are different intentions —
"leave the terminal rules alone" and "remove them" are not the same edit:

  # prefer English on everyday, leaving its accept and terminal untouched
  heyarr quality-profile set everyday \
    --prefer '[{"attribute":"language","op":"eq","value":"en","weight":50}]'

  # clear a profile's terminal rules (make it never "finished")
  heyarr quality-profile set archival --terminal '[]'

Rename with --name; --description replaces the description. The new rules take
effect the next time each want is evaluated — its next search, upgrade scan or
satisfaction read — not as an immediate mass re-judge.

```
heyarr quality-profile set <name|id> [flags]
```

### Options

```
      --accept string        replace gate rules (JSON array; [] clears)
      --addr string          where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --description string   replace the description
      --json                 emit machine-readable JSON
      --name string          rename the profile
      --prefer string        replace scoring rules (JSON array; [] clears)
      --terminal string      replace stop rules (JSON array; [] clears)
      --timeout duration     how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string         bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string    read the bearer token from this file (default: <data_dir>/cli.token when it exists)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr quality-profile](heyarr_quality-profile.md)	 - Author and inspect the quality profiles a want is measured against
