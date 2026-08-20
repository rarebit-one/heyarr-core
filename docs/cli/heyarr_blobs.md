## heyarr blobs

Inspect and read stored bytes

### Synopsis

Bytes are identified by their BLAKE3 digest and by nothing else (ADR-0005).

A blob identifier is blake3:<64 lowercase hex characters>. A malformed one is
rejected as a mistake in what you typed; a well-formed one this peer does not
hold is reported as absent. They are different answers to different questions
and the CLI keeps them apart.

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr blobs cat](heyarr_blobs_cat.md)	 - Write a blob's bytes to a file or to stdout
* [heyarr blobs stat](heyarr_blobs_stat.md)	 - Show what the catalog knows about a blob
* [heyarr blobs verify](heyarr_blobs_verify.md)	 - Read a blob back and check that it hashes to its own name
