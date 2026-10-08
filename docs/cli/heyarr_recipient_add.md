## heyarr recipient add

Register an executor's public key as a wrap recipient

### Synopsis

Register an executor's X25519 public key as a service recipient for the
executor principal (the name given to `heyarr token create --executor`).

Read the key and its fingerprint off the executor's own host, not off anything
that relayed them. This command prints the fingerprint it computes and asks you
to type the one the host displayed (or pass --fingerprint); a mismatch registers
nothing. Registering wraps nothing yet: `heyarr space grant` does that, per space.

```
heyarr recipient add --executor <principal> --pub x25519:<hex> [flags]
```

### Options

```
      --addr string          where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --executor string      the executor principal, by name or id
      --fingerprint string   the fingerprint the executor's host displayed (prompted for when absent)
      --json                 emit machine-readable JSON
      --label string         a note to recognise the executor by (no secrets: the controller stores it in the clear)
      --pub string           the executor's X25519 public key, x25519:<hex>
      --timeout duration     how long one request may take (default 30s)
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOID_WHICH_BINDS_DEVICE_DIR overrides)
```

### See also

* [heyarr recipient](heyarr_recipient.md)	 - Register executors' public keys as space-key recipients (ADR-0104)
