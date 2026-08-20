## heyarr token

Manage API tokens (ADR-0011)

### Synopsis

Manage the bearer tokens that authenticate API callers.

These commands operate on the controller database directly and must be run on
the host, as the user that owns the data directory. A token's secret is shown
once, at creation, and is stored only as an argon2id hash — it cannot be
recovered afterwards.

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr token create](heyarr_token_create.md)	 - Mint an API token
* [heyarr token list](heyarr_token_list.md)	 - List API tokens
* [heyarr token revoke](heyarr_token_revoke.md)	 - Revoke an API token
