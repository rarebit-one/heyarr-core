## heyarr library retype

Correct a library type Heyarr does not understand, keeping its content

### Synopsis

Give a library, named by id or name, a content type Heyarr understands when
its stored one is not (#896, ADR-0127). The type is one of: movie, series, music, book, document, game, rom.

A library created before Heyarr checked its type can carry one like `show`,
which identification ignores and acquisition never targets; `library list`
marks it (unknown). A retype changes only the type: the library's id, roots
and assets stay as they are. Works already identified under the old type are
not re-identified.

Only an unknown type can be retyped. Changing one known type to another would
re-identify every work in the library, so it is refused; create a new library
instead. Retyping a library to the type it already has does nothing and is
not an error.

A library declared in the configuration file must say the new type there too.
If the configuration already says it, the node refuses to start until the row
matches: remove the library from the configuration, start, retype it, then
restore it.

```
heyarr library retype <library> <content-type> [flags]
```

### Examples

```
heyarr library retype tv series
```

### Options

```
      --addr string         where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --json[=true|false]   emit machine-readable JSON
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
