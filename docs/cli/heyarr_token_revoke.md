## heyarr token revoke

Revoke an API token

### Synopsis

Revoke an API token by id, as printed by 'heyarr token list'.

Revocation takes effect on the very next request: the server reads the token
row on every call, so nothing is cached past it.

```
heyarr token revoke <id> [flags]
```

### Options

```
      --json   emit machine-readable JSON
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr token](heyarr_token.md)	 - Manage API tokens (ADR-0011)
