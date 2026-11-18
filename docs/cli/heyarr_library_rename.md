## heyarr library rename

Rename a library, keeping its id, roots and content

### Synopsis

Rename a library, named by id or name (ADR-0124).

The library's id is its identity and its name is a label, so a rename changes
nothing else: its roots, its assets and everything that references it stay as
they are. The new name must be unique, as it must be for `library add`.

A library declared in the configuration file is reconciled by name at every
start, so rename it there too — otherwise the next start creates a new, empty
library under the old name.

```
heyarr library rename <library> <new-name> [flags]
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

* [heyarr library](heyarr_library.md)	 - Manage libraries and their roots
