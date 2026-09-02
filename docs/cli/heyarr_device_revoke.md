## heyarr device revoke

Revoke a device at this peer and re-key the spaces it could read (ADR-0068, ADR-0049)

### Synopsis

Revoke one of a user's devices at this peer, then rotate away from it.

The revocation is the peer's own tombstone (DELETE /identities/devices): the
device stops authenticating here at once, whatever the identity's membership
ops say, and stays refused if re-presented. It is NOT a membership op — the peer
is not a member of the identity and its word stays local. To remove the device
from the identity itself, a member device signs a remove and pushes it
(POST /membership/{usr}).

Revocation is forward-looking, not retroactive (ADR-0049): the device keeps
whatever it already decrypted, so every space whose key is wrapped for its
encryption key is re-keyed — a fresh key, re-wrapped for the remaining
recipients, the revoked copy deleted, a snapshot under the new key. THIS device
must itself be a recipient of a space to re-key it; a space it cannot read is
reported as skipped, for a device that can. Only the named device is re-keyed
away: the devices it admitted are untouched.

```
heyarr device revoke <device-key> [flags]
```

### Options

```
      --addr string         where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --json                emit machine-readable JSON
      --no-rotate           only tombstone the device; leave every space it could read wrapped for it
      --timeout duration    how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string        bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string   read the bearer token from this file (default: <data_dir>/cli.token when it exists)
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOIDBIND_DEVICE_DIR overrides)
```

### See also

* [heyarr device](heyarr_device.md)	 - Manage this machine's device key (§40, ADR-0032)
