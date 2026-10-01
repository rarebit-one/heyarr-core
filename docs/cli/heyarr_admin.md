## heyarr admin

Host administration of the controller database

### Synopsis

Host administration that no API route performs.

These commands operate on the controller database directly and must be run on
the host, as the user that owns the data directory. They work with the
controller stopped or running: the database is single-writer (ADR-0003), so a
command waits for the controller's write lock (up to the busy timeout) rather
than racing it.

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr admin user](heyarr_admin_user.md)	 - Administer pinned user identities
