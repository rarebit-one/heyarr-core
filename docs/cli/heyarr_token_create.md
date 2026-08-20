## heyarr token create

Mint an API token

### Synopsis

Mint an API token for a named service.

The token is printed once and cannot be recovered. Creating a second token for
the same name is how rotation works: both are valid until you revoke the old
one.

```
heyarr token create <name> [flags]
```

### Options

```
      --expires string   expiry as a duration from now, e.g. 90d, 12h, 1y (default: never)
      --json             emit machine-readable JSON
      --scopes string    comma-separated scopes: read, write, admin (default "read")
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr token](heyarr_token.md)	 - Manage API tokens (ADR-0011)
