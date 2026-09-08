## heyarr library root rm

Remove a root from a library

### Synopsis

Stop scanning one of a library's roots, named by its id or its exact path.

Assets already ingested through the root are unaffected — an asset belongs to
its library, not to a root — so this stops future scans of the directory
without removing content (#228).

```
heyarr library root rm <library> <root> [flags]
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

* [heyarr library root](heyarr_library_root.md)	 - Add or remove a library's roots
