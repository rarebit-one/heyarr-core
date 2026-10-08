## heyarr vault put-ref

Seal one JSON object into a vault space and print its new ref

### Synopsis

Seal a JSON object, read from the file or from stdin (-), into the space under
a fresh random object id, and print its vault ref and where it landed as JSON.
The object is written at .jumpdrive/objects/<object uuid>.json in the space's drive.

The object must be a versioned, typed envelope: a JSON object with "v": 1 and a
non-empty "type"; the rest is the caller's. It is at most 8 MiB. The plaintext
is sealed on this machine and never uploaded, and never echoed in an error.

An executor (ADR-0104) needs a read,write grant on the space and a copy of its
key wrapped for its sealed recipient key. Exit status as for get-ref.

```
heyarr vault put-ref --space <space | hv1:<space>> <file | -> [flags]
```

### Options

```
      --addr string         where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --json                emit machine-readable JSON
      --space string        the vault space: its id, or its collection ref hv1:<space uuid> (required)
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
