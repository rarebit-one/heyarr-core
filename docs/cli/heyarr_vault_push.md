## heyarr vault push

Seal a local file into the vault and record it at a vault path

### Synopsis

Seal a local file into fixed ciphertext frames under the space key on THIS
device, upload the content blob and the sealed manifest (both self-pin on upload,
so they are retained), and record the manifest's blob id at --path in the space's
drive CRDT as one encrypted change.

The plaintext is never uploaded — the frames and the manifest are sealed here; the
peer stores ciphertext it cannot open.

```
heyarr vault push <file> [flags]
```

### Options

```
      --addr string         where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --json                emit machine-readable JSON
      --path string         the vault path to write the file at (required)
      --space string        the vault's space id (required)
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

* [heyarr vault](heyarr_vault.md)	 - Push, pull and list files in an encrypted media vault (ADR-0021, ADR-0095)
