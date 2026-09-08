## heyarr quality-profile create

Author a quality profile (§62)

### Synopsis

Create a quality profile from its accept/prefer/terminal rules.

The three groups are three different KINDS of statement (§62), and each is
given as a JSON array of rules — the same shape the API takes:

  accept    a GATE  — fail it and a candidate is rejected outright
  prefer    a SCORE — a weighted preference; missing all of them is still acceptable
  terminal  a STOP  — the point at which the upgrade workflow stops looking

  heyarr quality-profile create living-room \
    --accept  '[{"attribute":"resolution","op":"gte","value":1080}]' \
    --prefer  '[{"attribute":"video_codec","op":"eq","value":"hevc","weight":20}]'

An omitted group is left empty; a profile with no terminal rules is never
"finished", which is legal — that is what the seeded "archival" profile is.
Authoring is deliberate: a name that already exists is reported as a conflict,
never silently replaced.

```
heyarr quality-profile create <name> [flags]
```

### Options

```
      --accept string        gate rules as a JSON array — a candidate failing any is rejected
      --addr string          where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --description string   a human description of what the profile is for
      --json                 emit machine-readable JSON
      --prefer string        scoring rules as a JSON array — weighted preferences, never gates
      --terminal string      stop rules as a JSON array — when the upgrade workflow stops looking
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
