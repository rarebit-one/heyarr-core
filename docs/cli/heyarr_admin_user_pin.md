## heyarr admin user pin

Pin a user identity's public key (ADR-0032), without the HTTP API

### Synopsis

Pin a user identity: create its principal (kind 'user') and register its
signing key and recovery encryption key, the out-of-band step that ADR-0032
requires before anything the user signs is honoured — the same write as
POST /api/v1/identities/users.

It exists for a controller that does not mount that route: the Mnemosyne
personal profile mounts no identities API, so this is how its first user is
pinned. On heyarr it is equivalent to the route.

Both keys are PUBLIC. --recovery-key is required: an identity pinned without
one cannot have new spaces wrapped for its paper recovery secret (§41).

A pin is local to this controller's database; peers do not replicate it. Pin
the user on every controller its devices enrol at.

```
heyarr admin user pin <name> <ed25519:public-key> [flags]
```

### Options

```
      --json                  emit machine-readable JSON
      --recovery-key string   the identity's x25519 recovery encryption PUBLIC key (x25519:<hex>; required)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr admin user](heyarr_admin_user.md)	 - Administer pinned user identities
