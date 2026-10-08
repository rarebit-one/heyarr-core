## heyarr recipient list

List the registered service recipients

```
heyarr recipient list [flags]
```

### Options

```
      --addr string        where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --json               emit machine-readable JSON
      --timeout duration   how long one request may take (default 30s)
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOID_WHICH_BINDS_DEVICE_DIR overrides)
```

### See also

* [heyarr recipient](heyarr_recipient.md)	 - Register executors' public keys as space-key recipients (ADR-0104)
