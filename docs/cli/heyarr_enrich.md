## heyarr enrich

Enrich held music and book works with covers and canonical ids

### Synopsis

Held albums and books are identified from their filenames at ingest — a title,
maybe a year, no cover. Enrichment fills that in from a keyed lookup (MusicBrainz
and the Cover Art Archive for music, Open Library for books): it writes the work's
canonical id, fetches its cover as an ordinary artwork asset the library already
serves, and — when the match is confident — corrects a noisy filename-derived
title and author (ADR-0087, ADR-0088). The sources are keyless; no credential is
needed.

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr enrich backfill](heyarr_enrich_backfill.md)	 - Enrich held music/book works now, ignoring the background cadence
* [heyarr enrich status](heyarr_enrich_status.md)	 - Show how many held music/book works still lack a cover or id
