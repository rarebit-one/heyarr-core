## heyarr backup push

Take a backup and push it to every trusted Full Peer (§50, ADR-0046)

### Synopsis

Take a control-plane backup and push it to every trusted Full Peer.

A backup on the same disk as the database protects against a mistake, not
against a site (§50). This sends a signed copy to each Full Peer over the mTLS
peer surface — push, not pull, because the bytes are this node's own small state
(ADR-0046). Each peer verifies the signature and the digest before it stores
anything, and holds what it receives inert.

One unreachable peer does not stop the others: the cycle makes progress with
whoever it has and reports who it could not reach, rather than failing. What each
peer confirmed holding is recorded, so this node can later say a peer is a
generation behind even while that peer is unreachable.

```
heyarr backup push [flags]
```

### Options

```
      --json   emit machine-readable JSON
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr backup](heyarr_backup.md)	 - Take a whole-database backup of this peer's control plane (§49, ADR-0044)
