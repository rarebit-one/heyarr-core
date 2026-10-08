## heyarr space rotate

Revoke recipients from a space by rotating its key (§41, #361)

### Synopsis

Revoke one or more recipients from an encrypted space.

Rotation mints a FRESH space key and moves the space to its next key epoch. The
new key is wrapped for every REMAINING recipient (your recovery key included),
and the previous key is sealed under the new one as an opaque history row, so a
remaining device still reads everything written before the rotation. Nothing is
re-encrypted, snapshotted or compacted. The controller drops every copy of the
old key in the same step, so a revoked recipient never receives the new key and
can read nothing written from here on. It keeps whatever it could already read —
revocation is forward-looking, not retroactive.

This device must itself be a current recipient (only a device that can read a
space may re-key it), and at least one recipient must remain. Two rotations
racing from the same epoch cannot both land: the second is refused, and is
simply run again. The same goes for a recipient added to or removed from the
space while the rotation runs: the rotation is refused rather than dropping the
new recipient or re-admitting the removed one, and is simply run again.

Every kind of space rotates the same way — a playlist, a vault drive, starred,
play history or reading position — because no content is touched: a remaining
device reaches every earlier key through the history (ADR-0103).

```
heyarr space rotate <space-id> --revoke <recipient> [flags]
```

### Options

```
      --addr string          where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --json                 emit machine-readable JSON
      --revoke stringArray   a recipient (x25519:<hex>) to revoke; repeatable
      --timeout duration     how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string         bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string    read the bearer token from this file (default: <data_dir>/cli.token when it exists)
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOID_WHICH_BINDS_DEVICE_DIR overrides)
```

### See also

* [heyarr space](heyarr_space.md)	 - Create and read encrypted personal-state spaces (§38, §42, ADR-0049)
