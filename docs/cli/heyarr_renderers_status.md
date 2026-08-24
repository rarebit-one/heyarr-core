## heyarr renderers status

Report what a renderer is playing and how far in

### Synopsis

Report transport state and position.

A renderer that reports no position is not broken. Some report none until they
have parsed enough of the stream, and some never report one at all — the field
is simply omitted rather than shown as zero.

```
heyarr renderers status <renderer> [flags]
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

* [heyarr renderers](heyarr_renderers.md)	 - Find media renderers on the local network (§68)
