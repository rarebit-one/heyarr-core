## heyarr

Self-hosted content lifecycle, replication and consumption

### Synopsis

Heyarr manages content across its full lifecycle — discovery, acquisition,
ingest, identification, storage, replication and consumption — while brokering
encrypted user state across trusted devices and peers.

One logical library, multiple complete sovereign peers.

### Options

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr all](heyarr_all.md)	 - Run every role in one process (small deployments)
* [heyarr assets](heyarr_assets.md)	 - Browse the files behind the catalog
* [heyarr backup](heyarr_backup.md)	 - Take a whole-database backup of this peer's control plane (§49, ADR-0044)
* [heyarr blobs](heyarr_blobs.md)	 - Inspect and read stored bytes
* [heyarr config](heyarr_config.md)	 - Inspect configuration
* [heyarr controller](heyarr_controller.md)	 - Own coordinated mutable state: catalog, policy, jobs, API
* [heyarr desired](heyarr_desired.md)	 - Say what should exist, whether or not it does yet
* [heyarr device](heyarr_device.md)	 - Manage this machine's device key (§40, ADR-0032)
* [heyarr events](heyarr_events.md)	 - Follow the event log
* [heyarr fsck](heyarr_fsck.md)	 - Check stored bytes against the catalog (§57, ADR-0018)
* [heyarr gc](heyarr_gc.md)	 - Reclaim bytes nothing references (ADR-0018)
* [heyarr jobs](heyarr_jobs.md)	 - Inspect and retry durable work
* [heyarr library](heyarr_library.md)	 - Manage libraries and their roots
* [heyarr peer](heyarr_peer.md)	 - Serve and replicate bytes
* [heyarr peers](heyarr_peers.md)	 - Inspect and manage the peers of this instance
* [heyarr play](heyarr_play.md)	 - Play an asset on a television, speaker or projector (§68)
* [heyarr quality-profile](heyarr_quality-profile.md)	 - Inspect the quality profiles a want is measured against
* [heyarr renderers](heyarr_renderers.md)	 - Find media renderers on the local network (§68)
* [heyarr scan](heyarr_scan.md)	 - Scan a library's roots, optionally waiting for the scan to finish
* [heyarr system](heyarr_system.md)	 - Report what a running instance is and how far behind it has drifted
* [heyarr token](heyarr_token.md)	 - Manage API tokens (ADR-0011)
* [heyarr version](heyarr_version.md)	 - Print build information
* [heyarr worker](heyarr_worker.md)	 - Execute leased jobs
* [heyarr works](heyarr_works.md)	 - Browse the catalog
