## heyarr admin user list

List the user identities pinned on this controller

### Synopsis

List every user identity pinned in this controller's database: its name, its
signing and recovery public keys, when it was pinned, and how many of its
devices are enrolled here (active, expired and revoked).

A device authenticates at a controller only if its user is pinned THERE: pins
are local and peers do not replicate them. A controller that lists no users
refuses every device credential, with the same 401 any bad credential gets.

```
heyarr admin user list [flags]
```

### Options

```
      --json   emit machine-readable JSON
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr admin user](heyarr_admin_user.md)	 - Administer pinned user identities
