## heyarr blobs cat

Write a blob's bytes to a file or to stdout

### Synopsis

Read a blob's bytes (ADR-0013).

With --resume and an output file that already exists, the transfer continues
from the end of that file: the request carries Range: bytes=<offset>- and
If-Range with the blob's own validator, which is derived from the digest, so
nothing has to be remembered between runs. If the server answers 200 rather
than 206 the range was not honoured, and the only correct response is to start
over — appending a whole object to a partial one produces a file that is the
right length for nothing.

--json requires --output. Writing a JSON summary and the bytes to the same
stream would produce neither.

```
heyarr blobs cat <hash> [flags]
```

### Options

```
      --addr string         where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --json                emit machine-readable JSON
  -o, --output string       write to this file instead of stdout
      --resume              continue an interrupted transfer into --output
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
