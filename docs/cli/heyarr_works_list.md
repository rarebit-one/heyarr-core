## heyarr works list

List works

```
heyarr works list [flags]
```

### Options

```
      --addr string           where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --content-type string   only works of this content type
      --json                  emit machine-readable JSON
      --library string        only works with an asset in this library (id or name)
      --limit int             stop after this many rows (default: every row, following pagination cursors)
      --q string              only works whose sort title contains this
      --timeout duration      how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string          bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string     read the bearer token from this file (default: <data_dir>/cli.token when it exists)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr works](heyarr_works.md)	 - Browse the catalog
