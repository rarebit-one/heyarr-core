## heyarr space grant

Let an executor fetch and decrypt a space (ADR-0104)

### Synopsis

Grant an executor access to an encrypted space.

Two gates open together. The executor may FETCH the space's ciphertext (a
grant, read or --write), and it can DECRYPT it, because this device wraps the
space's current key for the executor's registered service recipient (register
it first with `heyarr recipient add`). With the current key the executor
also reads everything written before, through the space's key history.

This device must be one the space is wrapped for, enrolled, and authorised for
management; if the space has a recorded owner, it must be that owner's device.

The grant and the wrap are one request, recorded in one transaction: either
both land or neither does. If the space is re-keyed while this runs, the
controller refuses the stale wrap and nothing is recorded; run the command
again. If the reply is lost, running it again is safe: the grant is renewed and
the copy replaced.

Undo with `heyarr space revoke-executor`.

```
heyarr space grant <space-id> --executor <principal> --recipient x25519:<hex> [flags]
```

### Options

```
      --addr string        where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --executor string    the executor principal, by name or id
      --expires string     end the FETCH grant after a duration (720h) or at an RFC 3339 time; the wrapped key itself has no expiry
      --json               emit machine-readable JSON
      --recipient string   the executor's registered public key, x25519:<hex>
      --timeout duration   how long one request may take (default 30s)
      --write              also let the executor push changes and snapshots (read,write)
      --yes                skip the confirmation
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOID_WHICH_BINDS_DEVICE_DIR overrides)
```

### See also

* [heyarr space](heyarr_space.md)	 - Create and read encrypted personal-state spaces (§38, §42, ADR-0049)
