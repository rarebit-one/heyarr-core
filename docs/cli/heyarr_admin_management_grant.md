## heyarr admin management grant

Authorise a device for write

### Synopsis

Authorise a device for write. Idempotent: granting an authorised device again
updates its reason. --reason is required, so the event log says why.

```
heyarr admin management grant <ed25519:device-key> [flags]
```

### Options

```
      --json            emit machine-readable JSON
      --reason string   why this device is authorised (required)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr admin management](heyarr_admin_management.md)	 - Authorise enrolled devices for write (ADR-0065), without the HTTP API
