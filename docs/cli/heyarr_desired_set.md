## heyarr desired set

Change the conditions, the monitoring or the note

### Synopsis

Change what a want is measured against, whether it keeps looking, or its note.

The target cannot be changed: repointing a want at different content is not an
edit, it is a different want. Remove it and want the other thing.

```
heyarr desired set <id> [flags]
```

### Options

```
      --addr string              where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --json                     emit machine-readable JSON
      --monitor                  keep looking for something better
      --no-monitor               stop looking once it is satisfied
      --quality-profile string   measure this want against a different profile, by name
      --reason string            replace the note
      --timeout duration         how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string             bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string        read the bearer token from this file (default: <data_dir>/cli.token when it exists)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr desired](heyarr_desired.md)	 - Say what should exist, whether or not it does yet
