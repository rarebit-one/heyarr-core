## heyarr system

Report what a running instance is and how far behind it has drifted

### Synopsis

Ask a running Heyarr what it is: its build, its schema version, its peer
identity, and whether the things it depends on are working.

`system drift` compares that against what it was expected to be, and
answers with a distance rather than a yes or no.

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr system drift](heyarr_system_drift.md)	 - Report how far a running instance has drifted from what was expected
* [heyarr system info](heyarr_system_info.md)	 - Print what the instance is running
