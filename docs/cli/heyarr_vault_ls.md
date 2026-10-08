## heyarr vault ls

List the live files in a vault

### Synopsis

Materialise the space's drive on this device and list its live files — each
file's vault path, plaintext size, and whether the path is currently conflicted
(more than one live version).

```
heyarr vault ls <space-id> [flags]
```

### Options

```
      --addr string         where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --json                emit machine-readable JSON
      --timeout duration    how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string        bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string   read the bearer token from this file (default: <data_dir>/cli.token when it exists)
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOID_WHICH_BINDS_DEVICE_DIR overrides)
      --pin-file string     an owner-only file holding the sealed key's PIN (default: vault.sealedfile.pin_file, else the systemd credential heyarr-recipient-pin)
      --sealed-key file     the file holding the sealed recipient key from heyarr recipient init (implies --unwrapper sealedfile; default: vault.sealedfile.key_file)
      --unwrapper string    the custody backend that opens space keys (default: vault.unwrapper); sealedfile is an executor's service-recipient key (ADR-0104)
```

### See also

* [heyarr vault](heyarr_vault.md)	 - Push, pull and list files in an encrypted media vault (ADR-0021, ADR-0095)
