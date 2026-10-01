## heyarr admin user rekey

Replace a user identity's pinned key in place (void-which-binds ADR-0022)

### Synopsis

Replace a pinned user identity's signing key and recovery encryption key in
place — the gen1 to gen2 cutover (void-which-binds ADR-0022, C2 step 6).

<principal> is a principal id, a user identity id or a principal name, and must
name exactly one pinned user. The principal and its user identity keep their
ids, so nothing keyed by them changes; the identity is NOT revoked and re-pinned.
The membership ops recorded under the old key are deleted, since every one of
them was signed by it.

Devices are untouched. Revoke each old-generation device separately with
'heyarr device revoke --no-rotate <device-key>'; new devices enrol under the new
key. Once this runs, nothing signed by the old key authenticates here, and
there is no undo short of rekeying back.

```
heyarr admin user rekey <principal> <ed25519:public-key> [flags]
```

### Options

```
      --json                  emit machine-readable JSON
      --recovery-key string   the identity's new x25519 recovery encryption PUBLIC key (x25519:<hex>; required)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr admin user](heyarr_admin_user.md)	 - Administer pinned user identities
