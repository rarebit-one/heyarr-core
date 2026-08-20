## heyarr library

Manage libraries and their roots

### Synopsis

A library is a named collection of roots — directories Heyarr scans (§10).

Libraries can also be declared in the configuration file, which the controller
reconciles at start. These commands are the runtime equivalent and write
through the API, so they work against a controller on another host.

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr library add](heyarr_library_add.md)	 - Create a library, optionally with roots
* [heyarr library list](heyarr_library_list.md)	 - List libraries and their roots
