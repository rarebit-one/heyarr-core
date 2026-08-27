## heyarr space rotate

Revoke recipients from a space by rotating its key (§41, #361)

### Synopsis

Revoke one or more recipients from an encrypted space.

Rotation mints a FRESH space key, re-wraps it for every REMAINING recipient,
deletes each revoked recipient's stored copy, and pushes a snapshot of the current
state under the new key (then compacts the now-unreadable old change log the
snapshot subsumes). The revoked device keeps whatever it already decrypted —
revocation is forward-looking, not retroactive — but can read nothing encrypted
from here on.

This device must itself be a current recipient (only a device that can read a
space may re-key it), and at least one recipient must remain.

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
  -c, --config string       path to the configuration file (default: built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; HEYARR_DEVICE_DIR overrides)
```

### See also

* [heyarr space](heyarr_space.md)	 - Create and read encrypted personal-state spaces (§38, §42, ADR-0049)
