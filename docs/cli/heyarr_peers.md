## heyarr peers

Inspect and manage the peers of this instance

### Synopsis

One logical library, multiple complete sovereign peers (§2).

Membership is the only trust root in the inter-peer path (ADR-0012): a peer's
public key is pinned by the record `peers add` creates, and revocation is
`peers remove` deleting it. There is no CA, no join token and no discovery.

A peer is its public key, not its address. Enrol it with the key the other site
prints here; move its endpoint later by registering the same key again.

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr peers add](heyarr_peers_add.md)	 - Enrol another peer by its public key
* [heyarr peers list](heyarr_peers_list.md)	 - List peers
* [heyarr peers ping](heyarr_peers_ping.md)	 - Open a mutually authenticated connection to a peer and report who it says you are
* [heyarr peers remove](heyarr_peers_remove.md)	 - Revoke a peer's membership
* [heyarr peers show](heyarr_peers_show.md)	 - Show one peer, by name or id
