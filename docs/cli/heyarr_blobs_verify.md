## heyarr blobs verify

Read a blob back and check that it hashes to its own name

### Synopsis

Download a blob and hash the bytes as they arrive.

The check is done here, on the bytes as received, rather than by asking the
server whether they are fine — invariant 1: a destination always verifies bytes
itself and never trusts a claimed hash. Asking the server would confirm only
that its catalog agrees with itself.

It exits non-zero when the bytes do not match, so it can be a cron job.

```
heyarr blobs verify <hash> [flags]
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

* [heyarr blobs](heyarr_blobs.md)	 - Inspect and read stored bytes
