## heyarr space create

Mint an encrypted space and wrap its key for the authorised devices

### Synopsis

Mint a new encrypted space of a given kind (personal, family, shared, research)
and seal its key for each recipient — this device (unless --no-self), your
recovery key (unless --recovery=false), plus every --recipient encryption key you
name.

The space key is generated here and never leaves: the controller receives the
opaque space and the wrapped copies of the key, and can open none of them.

By default the space is also wrapped for your user identity's RECOVERY key, so the
space key survives the loss of every device — your recovery secret alone
regenerates it (§79, #360). This is silently skipped if you have no user identity
yet; run `heyarr identity generate` to enable it, or pass --recovery=false.

```
heyarr space create [flags]
```

### Options

```
      --addr string             where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --identity-dir string     where your user identity lives (default: your config directory; VOIDBIND_IDENTITY_DIR overrides)
      --json                    emit machine-readable JSON
      --kind string             space kind: personal, family, shared, or research (default "personal")
      --recipient stringArray   an authorised device's encryption key (x25519:<hex>); repeatable
      --recovery                also wrap the key for your user identity's recovery key, so it survives losing every device (#360) (default true)
      --self                    also wrap the key for this device, so it can read the space (default true)
      --timeout duration        how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string            bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string       read the bearer token from this file (default: <data_dir>/cli.token when it exists)
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOIDBIND_DEVICE_DIR overrides)
```

### See also

* [heyarr space](heyarr_space.md)	 - Create and read encrypted personal-state spaces (§38, §42, ADR-0049)
