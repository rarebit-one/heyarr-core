## heyarr vault

Retire the legacy vault data a media node still holds (#823)

### Synopsis

Host administration for the vault blobs a media node kept from before the
vault moved to the personal profile (ADR-0107). The vault client is Mnemosyne's
("mnemosyne vault"); heyarr serves media.

```
heyarr vault
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr vault purge](heyarr_vault_purge.md)	 - Release every legacy vault placement pin so GC can reclaim it (#823)
