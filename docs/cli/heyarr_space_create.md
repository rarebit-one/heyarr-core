## heyarr space create

Mint an encrypted space and wrap its key for the authorised devices

### Synopsis

Mint a new encrypted space of a given kind (personal, family, shared, research)
and seal its key for each recipient — this device (unless --no-self) plus every
--recipient encryption key you name.

The space key is generated here and never leaves: the controller receives the
opaque space and the wrapped copies of the key, and can open none of them.

```
heyarr space create [flags]
```

### Options

```
      --addr string             where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --json                    emit machine-readable JSON
      --kind string             space kind: personal, family, shared, or research (default "personal")
      --recipient stringArray   an authorised device's encryption key (x25519:<hex>); repeatable
      --self                    also wrap the key for this device, so it can read the space (default true)
      --timeout duration        how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string            bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string       read the bearer token from this file (default: <data_dir>/cli.token when it exists)
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; HEYARR_DEVICE_DIR overrides)
```

### See also

* [heyarr space](heyarr_space.md)	 - Create and read encrypted personal-state spaces (§38, §42, ADR-0049)
