## heyarr desired unblock

Let a want choose a blocked release again

### Synopsis

Lift a block, so the want may choose that release again.

For a block that was wrong — most often one recorded when this node could not
place the download (no library for the content type, a completed path that
needed mapping) rather than because the release itself was bad. Fix the
configuration first; the next search can then pick the release up again, and a
fanned-out want is re-applied without one. The candidate is the id
`heyarr desired blocked` lists.

```
heyarr desired unblock <id> <candidate> [flags]
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
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr desired](heyarr_desired.md)	 - Say what should exist, whether or not it does yet
