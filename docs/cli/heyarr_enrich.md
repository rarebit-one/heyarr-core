## heyarr enrich

Enrich held works with artwork, descriptions and canonical ids

### Synopsis

Held films, series, albums and books are identified from their filenames at
ingest — a title, maybe a year, no artwork. Enrichment fills that in from a keyed
lookup (TMDB for films and series, MusicBrainz and the Cover Art Archive for
music, Open Library for books): it writes the work's canonical ids and overview,
and fetches its poster (and, for a film or series, its backdrop) as ordinary
artwork assets the node stores and serves to every client. A confident book or
album match also corrects a noisy filename-derived title and author (ADR-0087,
ADR-0088); a film or series is decorated, never renamed (ADR-0122). MusicBrainz
and Open Library are keyless; TMDB needs its API read-access token.

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr enrich backfill](heyarr_enrich_backfill.md)	 - Enrich held works now, ignoring the background cadence
* [heyarr enrich status](heyarr_enrich_status.md)	 - Show how many held works still lack their artwork or id
