## heyarr library root

Add or remove a library's roots

### Synopsis

A root is one directory a library is scanned from (§10). A library can
gain a root at any time, not only at creation, and lose one without losing the
content already ingested through it (#228).

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr library](heyarr_library.md)	 - Manage libraries and their roots
* [heyarr library root add](heyarr_library_root_add.md)	 - Add a root to an existing library
* [heyarr library root rm](heyarr_library_root_rm.md)	 - Remove a root from a library
* [heyarr library root set-ingest-mode](heyarr_library_root_set-ingest-mode.md)	 - Change how an existing root materialises ingested bytes
