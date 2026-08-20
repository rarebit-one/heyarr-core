## heyarr jobs list

List jobs

```
heyarr jobs list [flags]
```

### Options

```
      --addr string         where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --json                emit machine-readable JSON
      --limit int           stop after this many rows (default: every row, following pagination cursors)
      --state string        pending, leased, succeeded, failed or dead
      --timeout duration    how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string        bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string   read the bearer token from this file (default: <data_dir>/cli.token when it exists)
      --type string         only jobs of this type, e.g. scan_library
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr jobs](heyarr_jobs.md)	 - Inspect and retry durable work
