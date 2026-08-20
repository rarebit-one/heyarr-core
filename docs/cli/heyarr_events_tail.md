## heyarr events tail

Print events as they happen

### Synopsis

Follow the event stream, printing each event as it arrives.

--after resumes from a sequence number: pass the last one you saw and nothing
is missed or repeated. --types filters server-side and accepts exact type names
and trailing-* namespace prefixes, e.g. --types 'content.*,job.succeeded'.

If the server reports that it dropped events for this connection — which it
does rather than quietly continuing with a hole — the notice is printed and,
unless --reconnect=false, the stream is reopened from the resume point it gave.
A dropped-events notice is never swallowed: losing events is recoverable, not
knowing you lost them is not.

--json emits one compact JSON object per line rather than an array, because a
stream has no end at which to close one.

```
heyarr events tail [flags]
```

### Options

```
      --addr string         where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --after int           resume after this sequence number
      --json                emit machine-readable JSON
  -n, --limit int           stop after this many events (default: follow forever)
      --reconnect           reopen the stream after a gap notice or a dropped connection (default true)
      --timeout duration    how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string        bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string   read the bearer token from this file (default: <data_dir>/cli.token when it exists)
      --types strings       only these event types; accepts trailing-* prefixes
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr events](heyarr_events.md)	 - Follow the event log
