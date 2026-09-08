## heyarr library rm

Remove an empty library and its roots

### Synopsis

Remove a library, named by id or name (#228).

The library must hold no content: remove its works first
(DELETE /api/v1/works/{id}) — the empty library then goes with its roots.
Logical in ADR-0018's sense: catalog rows go, the bytes stay for the GC sweep.

```
heyarr library rm <library> [flags]
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

* [heyarr library](heyarr_library.md)	 - Manage libraries and their roots
