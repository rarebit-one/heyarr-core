## heyarr assets

Browse the files behind the catalog

### Synopsis

An asset is one file belonging to an edition (§14).

A managed asset has a blob; a linked asset has a path and no blob at all
(ADR-0020), which is why the blob column can legitimately be empty.

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr assets list](heyarr_assets_list.md)	 - List assets
