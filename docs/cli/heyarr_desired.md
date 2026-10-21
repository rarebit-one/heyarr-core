## heyarr desired

Say what should exist, whether or not it does yet

### Synopsis

A desired item is "this content should exist under these conditions" (§55).

The point is that you can want something Heyarr has never seen. Name it by what
it is — a content type, a title and a year — and the Work is created for you,
using the same normalisation the scanner uses, so wanting a film and later
scanning it converge on one Work rather than producing two.

Known media types use a starting quality profile when none is named. Pass
--quality-profile to choose a different standard.

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr desired add](heyarr_desired_add.md)	 - Want something
* [heyarr desired block](heyarr_desired_block.md)	 - Stop a want choosing a release again
* [heyarr desired blocked](heyarr_desired_blocked.md)	 - List the releases a want will not choose again, and why
* [heyarr desired list](heyarr_desired_list.md)	 - List what should exist
* [heyarr desired reingest](heyarr_desired_reingest.md)	 - Re-drive a wedged ingest — a finished download that never imported
* [heyarr desired rm](heyarr_desired_rm.md)	 - Stop wanting something
* [heyarr desired set](heyarr_desired_set.md)	 - Change the conditions, the monitoring or the note
* [heyarr desired unblock](heyarr_desired_unblock.md)	 - Let a want choose a blocked release again
