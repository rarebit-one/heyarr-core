## heyarr library root set-ingest-mode

Change how an existing root materialises ingested bytes

### Synopsis

Change a root's ingest mode after it was created, without tearing the root
down and rebuilding it.

The mode is one of reflink, hardlink, copy or link (ADR-0014, ADR-0020). It
governs how the NEXT ingest materialises; bytes already in the store keep the
inode they arrived with, so this is safe to run on a live root.

Why you would: a store created on the reflink default lands a full byte COPY on
a filesystem that cannot block-clone but could hardlink — a ZFS pool with block
cloning off is the case #222 pins. Switching such a root to hardlink stops the
copy for everything ingested from then on.

```
heyarr library root set-ingest-mode <library> <root> <mode> [flags]
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
