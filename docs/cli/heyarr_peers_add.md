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

The endpoint is checked HERE rather than when something first dials it. Give it
as `https://host:port`, or as a bare `host:port`, which is read as https:
the inter-peer path is mutually authenticated TLS (ADR-0012), so there is one
scheme to guess and http is refused rather than upgraded. A `unix:///path`
socket is accepted for a peer on this host. Anything else is refused before a
record exists, because registration is idempotent on the key: a typo would
otherwise replace a working endpoint and leave the peer looking healthy in
`peers list` while being unreachable.

Membership is the only trust root in the inter-peer path, and revocation is
`heyarr peers remove`. It is consulted on every request, so a removed
peer loses access on the connection it is already holding open.

REACHABILITY MUST BE MUTUAL, and it is checked here (#186, ADR-0037). Heyarr
does not support a one-way pairing, because the two flows replication needs run
in opposite directions: a peer PUSHES its inventory to the controller, and a
destination PULLS bytes from the source. A link that carries only one direction
deadlocks whichever node is the destination, and it deadlocks SILENTLY — the
controller is never told the far node holds a blob, so reconciliation correctly
emits no work and nothing is reported as wrong.

So when --endpoint is given, this command dials the peer and then asks that
peer whether it can reach back, and REPORTS what it found. Nothing is refused.
Each peer is authoritative for its own site (ADR-0038): a peer that can be
reached but cannot reach back fetches what it lacks from the peer it can reach,
and both sites keep serving everything already on their own disks either way.
A one-way pairing is an ordinary participant, and a peer that cannot be reached
at all is usually a machine that is not up yet.

```
heyarr peers add [flags]
```

### Options

```
      --addr string         where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --endpoint string     where to reach the peer, as https://192.168.1.10:8443, a bare host:port or unix:///path; not its identity
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
