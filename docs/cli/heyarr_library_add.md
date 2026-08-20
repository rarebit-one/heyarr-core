## heyarr library add

Create a library, optionally with roots

### Synopsis

Create a library and add its roots.

The roots are added after the library exists, so a run that fails part way
leaves a library with fewer roots rather than nothing — re-running with the
same name reports the conflict rather than silently making a second library.

```
heyarr library add <name> [flags]
```

### Options

```
      --addr string           where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --content-type string   what the library holds: movie, show, book, music (required)
      --disabled              create the library but do not scan it yet
      --ingest-mode string    how bytes are materialised for these roots: reflink, hardlink, copy or link (ADR-0014, ADR-0020) (default "reflink")
      --json                  emit machine-readable JSON
      --root stringArray      a directory to scan; repeat for several
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
