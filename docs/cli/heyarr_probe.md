## heyarr probe

Media probe operations

### Synopsis

A probe is what ffprobe said about a blob's bytes (§29): its container, its
streams, and for a picture its size and colour. It is taken once, at ingest,
and read by playback planning and copy selection.

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr probe backfill](heyarr_probe_backfill.md)	 - Re-probe video whose probe cannot say whether it is HDR
