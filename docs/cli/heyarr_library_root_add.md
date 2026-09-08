## heyarr library root add

Add a root to an existing library

### Synopsis

Add a directory for an existing library to scan.

The second root of a library, and every one after it, is added here rather than
by creating a second library over the same tree — two libraries over one tree
are a different statement about the content than one library with two roots
(#228).

```
heyarr library root add <library> <path> [flags]
```

### Options

```
      --addr string          where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --disabled             add the root but do not scan it yet
      --ingest-mode string   how bytes are materialised for this root: reflink, hardlink, copy or link (ADR-0014, ADR-0020) (default "reflink")
      --json                 emit machine-readable JSON
      --timeout duration     how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string         bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string    read the bearer token from this file (default: <data_dir>/cli.token when it exists)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr library root](heyarr_library_root.md)	 - Add or remove a library's roots
