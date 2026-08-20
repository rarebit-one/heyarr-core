## heyarr library list

List libraries and their roots

```
heyarr library list [flags]
```

### Options

```
      --addr string           where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --content-type string   only libraries of this content type
      --json                  emit machine-readable JSON
      --limit int             stop after this many rows (default: every row, following pagination cursors)
      --q string              only libraries whose name contains this
      --timeout duration      how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string          bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string     read the bearer token from this file (default: <data_dir>/cli.token when it exists)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr library](heyarr_library.md)	 - Manage libraries and their roots
