## heyarr recipient remove

Withdraw a service recipient, and every space-key copy wrapped for it

### Synopsis

Withdraw a service recipient. In the same step the controller deletes every
copy of a space key wrapped for it, on every space, so the executor can unwrap
nothing more from this node. Its grants stay; revoke them with
`heyarr space revoke-executor`.

This is not forward secrecy: the executor keeps any key it already unwrapped,
and a peer that replicated a copy keeps it until a rotation. See
`heyarr space revoke-executor --help`.

```
heyarr recipient remove <id | x25519:<hex>> [flags]
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
