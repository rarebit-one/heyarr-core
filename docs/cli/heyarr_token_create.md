## heyarr token create

Mint an API token

### Synopsis

Mint an API token for a named service.

The token is printed once and cannot be recovered. Creating a second token for
the same name is how rotation works: both are valid until you revoke the old
one.

With --executor the token is RESTRICTED (ADR-0104): it acts as an executor
principal, carries read and write, and reaches only the vault surface and the
encrypted spaces an owner's device has granted that principal. It cannot touch
the media library, the catalog, MCP, admin routes, or rotate or delete a key.
A name is either an executor or not: --executor on an ordinary token's name, or
an ordinary token on an executor's name, is refused.

```
heyarr token create <name> [flags]
```

### Options

```
      --executor         mint a restricted executor token: vault surface and granted spaces only (ADR-0104)
      --expires string   expiry as a duration from now, e.g. 90d, 12h, 1y (default: never)
      --json             emit machine-readable JSON
      --scopes string    comma-separated scopes: read, write, admin (default "read")
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr token](heyarr_token.md)	 - Manage API tokens (ADR-0011)
