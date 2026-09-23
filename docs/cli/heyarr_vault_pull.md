## heyarr vault pull

Read a file from the vault, decrypting it on this device

### Synopsis

Materialise the space's drive, resolve the vault path to its manifest blob,
and read the file back by range-fetching and decrypting only that manifest's
content frames — all on this device. Write it to -o, or to stdout.

A path that is absent, or one that currently has more than one live version (a
conflict), is refused rather than guessing which bytes were meant.

```
heyarr vault pull <space-id> <vault-path> [flags]
```

### Options

```
      --addr string         where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --json                emit machine-readable JSON
  -o, --out string          write to this file instead of stdout
      --timeout duration    how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string        bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string   read the bearer token from this file (default: <data_dir>/cli.token when it exists)
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOIDBIND_DEVICE_DIR overrides)
```

### See also

* [heyarr vault](heyarr_vault.md)	 - Push, pull and list files in an encrypted media vault (ADR-0021, ADR-0095)
