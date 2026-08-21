## heyarr quality-profile

Inspect the quality profiles a want is measured against

### Synopsis

A quality profile says three different KINDS of thing (§62):

  accept    a GATE  — fail it and a candidate is rejected outright
  prefer    a SCORE — never a gate; a candidate meeting none is still acceptable
  terminal  a STOP  — the point at which the upgrade workflow stops looking

A profile with no terminal rules is never finished, which is legal and is what
the seeded "archival" profile is.

Authoring profiles is an API operation; these commands read them.

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr quality-profile list](heyarr_quality-profile_list.md)	 - List the quality profiles
