## heyarr peers attach

Attach to a controller over mTLS and report what it records this node as

### Synopsis

Attach to a controller and print the attachment it answers with (ADR-0029,
ADR-0033).

The credential is this node's Ed25519 peer identity — the same one `peers ping`
presents and the same one another peer pins. There is no per-peer token to
distribute, store or revoke separately: membership is the only trust root, and
removing the membership record is the revocation.

Two requests are made. The first asks the controller what it derived from this
node's certificate; the second sends that id back as a DECLARATION and the
controller compares it against the certificate again. A peer cannot act as
another peer by putting a different id in that body — the acting peer comes
from the certificate and never from the request — and this command sends the
declaration so that a node whose identity has drifted from its configuration
finds out here rather than after it has reported something.

A peer is not an admin. This credential reaches the peer surface only; token
management, peer enrolment and policy are the admin surface, on the
controller's client API, behind an admin-scoped bearer token.

```
heyarr peers attach <name-or-id> [flags]
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
