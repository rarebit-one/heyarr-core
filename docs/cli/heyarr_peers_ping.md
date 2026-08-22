## heyarr peers ping

Open a mutually authenticated connection to a peer and report who it says you are

### Synopsis

Dial another peer over mTLS and print the identity it derived from this node's
certificate (§26, ADR-0012).

Both ends pin. This node presents a self-signed certificate carrying its
Ed25519 public key, the other end refuses it unless a membership record there
pins that key, and this node refuses the other end unless the certificate it
presents carries the key the local membership record pins. Neither end has
anything else to go on: there is no CA and no PKI in the inter-peer path.

The identity printed is the OTHER end's conclusion about this node, not
anything this node asserted. If it names this peer, the pin held in both
directions.

A refusal is a failed handshake rather than an error status, because a refused
peer must never reach a request handler. So this command's failure output is
the point of it: "not a member" means the key is not enrolled at the other
site, and a connection error means the endpoint is wrong or nothing is
listening there.

```
heyarr peers ping <name-or-id> [flags]
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
