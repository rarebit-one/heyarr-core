## heyarr desired blocked

List the releases a want will not choose again, and why

### Synopsis

List the releases this want has blocked.

A release is blocked when it proved bad for this want — a dead swarm, a file
that failed verification, a download that never matched what was searched for.
The want will not choose it again, so the next search moves on. Pair with
`heyarr desired unblock` to lift a block that was wrong.

```
heyarr desired blocked <id> [flags]
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
