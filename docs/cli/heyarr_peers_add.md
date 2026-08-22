## heyarr peers add

Enrol another peer by its public key

### Synopsis

Register another node as a member of this fabric (§26, ADR-0012).

Enrolment is operator-mediated and explicit. There is no discovery, no join
token and no trust on first use: you read the other node's public key out of
`heyarr peers list` at that site, and register it here. The operator at
the other site does the same in the other direction. Two nodes, two commands.

A peer is registered BY ITS PUBLIC KEY. --endpoint is where to reach it and may
change freely: run this again with the same --name and --public-key and a new
--endpoint, and the peer keeps its identity, its id and its enrolment date.
--public-key is required, and there is no form of this command without it —
registering a hostname and learning the key afterwards is trust on first use.

Membership is the only trust root in the inter-peer path, and revocation is
`heyarr peers remove`. It is consulted on every request, so a removed
peer loses access on the connection it is already holding open.

```
heyarr peers add [flags]
```

### Options

```
      --addr string         where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --endpoint string     where to reach the peer; not its identity
      --json                emit machine-readable JSON
      --mode string         full, partial, cache, archive or compute (§9) (default "full")
      --name string         the peer's name, unique within this instance
      --public-key string   the peer's Ed25519 public key as ed25519:<64 hex characters> — who it is (required)
      --site string         the peer's failure domain (§35)
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
