## heyarr peers remove

Revoke a peer's membership

### Synopsis

Remove a peer's membership record, which is what revocation is (ADR-0012).

There is no revocation list and no certificate to expire: the record IS the
trust, so deleting it withdraws the trust. Membership is checked on every
request, so the removed peer stops being able to read bytes immediately — on
the connection it already has open, not at its next reconnect.

The peer's replica rows go with it. A peer this instance will not talk to is
not a peer whose copy counts towards placement.

This node cannot remove itself.

```
heyarr peers remove <name-or-id> [flags]
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

* [heyarr peers](heyarr_peers.md)	 - Inspect and manage the peers of this instance
