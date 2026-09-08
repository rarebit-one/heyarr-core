## heyarr works show

Show one work

```
heyarr works show <id> [flags]
```

### Options

```
      --addr string         where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --device-dir string   where this machine's device key lives, used with --peer (default: your config directory; VOIDBIND_DEVICE_DIR overrides)
      --json                emit machine-readable JSON
      --peer string         read from a REMOTE peer as this machine's enrolled device (ADR-0048): a fresh possession proof is minted per request, never a cached token. The value is the peer's http(s):// URL. Mutually exclusive with --token
      --timeout duration    how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string        bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string   read the bearer token from this file (default: <data_dir>/cli.token when it exists)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr works](heyarr_works.md)	 - Browse the catalog
