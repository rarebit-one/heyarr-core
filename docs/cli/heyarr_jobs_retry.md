## heyarr jobs retry

Put a finished job back on the queue

### Synopsis

Retry a succeeded, failed or dead job.

This is an operator action: it says that whatever was wrong has been fixed. A
job that is still pending or leased cannot be retried, because it has not
stopped.

```
heyarr jobs retry <id> [flags]
```

### Options

```
      --addr string         where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --json                emit machine-readable JSON
      --timeout duration    how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string        bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string   read the bearer token from this file (default: <data_dir>/cli.token when it exists)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr jobs](heyarr_jobs.md)	 - Inspect and retry durable work
