## heyarr renderers seek

Jump to a position

### Synopsis

Jump to a position, given as seconds or as [h:]mm:ss.

The position is ABSOLUTE — measured from the start — not a delta from where
the renderer is now. "90", "1:30" and "0:01:30" are the same place.

```
heyarr renderers seek <renderer> <position> [flags]
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
