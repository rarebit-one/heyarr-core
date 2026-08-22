# heyarr command reference

Generated from the cobra command tree by `make gen`. Do not edit these pages by
hand: CI regenerates them and fails on any difference.

The root command is documented in [`heyarr.md`](heyarr.md).

| Command | Description |
| --- | --- |
| [`heyarr all`](heyarr_all.md) | Run every role in one process (small deployments) |
| [`heyarr assets list`](heyarr_assets_list.md) | List assets |
| [`heyarr assets`](heyarr_assets.md) | Browse the files behind the catalog |
| [`heyarr blobs cat`](heyarr_blobs_cat.md) | Write a blob's bytes to a file or to stdout |
| [`heyarr blobs stat`](heyarr_blobs_stat.md) | Show what the catalog knows about a blob |
| [`heyarr blobs verify`](heyarr_blobs_verify.md) | Read a blob back and check that it hashes to its own name |
| [`heyarr blobs`](heyarr_blobs.md) | Inspect and read stored bytes |
| [`heyarr config print`](heyarr_config_print.md) | Print the fully resolved configuration |
| [`heyarr config`](heyarr_config.md) | Inspect configuration |
| [`heyarr controller`](heyarr_controller.md) | Own coordinated mutable state: catalog, policy, jobs, API |
| [`heyarr desired add`](heyarr_desired_add.md) | Want something |
| [`heyarr desired list`](heyarr_desired_list.md) | List what should exist |
| [`heyarr desired rm`](heyarr_desired_rm.md) | Stop wanting something |
| [`heyarr desired set`](heyarr_desired_set.md) | Change the conditions, the monitoring or the note |
| [`heyarr desired`](heyarr_desired.md) | Say what should exist, whether or not it does yet |
| [`heyarr device generate`](heyarr_device_generate.md) | Generate this machine's device key |
| [`heyarr device list`](heyarr_device_list.md) | List this machine's device keys |
| [`heyarr device mcp`](heyarr_device_mcp.md) | Run the Personal MCP for this machine's device key (§73) |
| [`heyarr device remove`](heyarr_device_remove.md) | Remove a device key |
| [`heyarr device show`](heyarr_device_show.md) | Show one device key |
| [`heyarr device`](heyarr_device.md) | Manage this machine's device key (§40, ADR-0032) |
| [`heyarr events tail`](heyarr_events_tail.md) | Print events as they happen |
| [`heyarr events`](heyarr_events.md) | Follow the event log |
| [`heyarr fsck`](heyarr_fsck.md) | Check stored bytes against the catalog (§57, ADR-0018) |
| [`heyarr gc`](heyarr_gc.md) | Reclaim bytes nothing references (ADR-0018) |
| [`heyarr jobs list`](heyarr_jobs_list.md) | List jobs |
| [`heyarr jobs retry`](heyarr_jobs_retry.md) | Put a finished job back on the queue |
| [`heyarr jobs show`](heyarr_jobs_show.md) | Show one job |
| [`heyarr jobs`](heyarr_jobs.md) | Inspect and retry durable work |
| [`heyarr library add`](heyarr_library_add.md) | Create a library, optionally with roots |
| [`heyarr library list`](heyarr_library_list.md) | List libraries and their roots |
| [`heyarr library`](heyarr_library.md) | Manage libraries and their roots |
| [`heyarr peer`](heyarr_peer.md) | Serve and replicate bytes |
| [`heyarr peers add`](heyarr_peers_add.md) | Enrol another peer by its public key |
| [`heyarr peers list`](heyarr_peers_list.md) | List peers |
| [`heyarr peers remove`](heyarr_peers_remove.md) | Revoke a peer's membership |
| [`heyarr peers show`](heyarr_peers_show.md) | Show one peer, by name or id |
| [`heyarr peers`](heyarr_peers.md) | Inspect and manage the peers of this instance |
| [`heyarr quality-profile list`](heyarr_quality-profile_list.md) | List the quality profiles |
| [`heyarr quality-profile`](heyarr_quality-profile.md) | Inspect the quality profiles a want is measured against |
| [`heyarr scan`](heyarr_scan.md) | Scan a library's roots, optionally waiting for the scan to finish |
| [`heyarr system drift`](heyarr_system_drift.md) | Report how far a running instance has drifted from what was expected |
| [`heyarr system info`](heyarr_system_info.md) | Print what the instance is running |
| [`heyarr system`](heyarr_system.md) | Report what a running instance is and how far behind it has drifted |
| [`heyarr token create`](heyarr_token_create.md) | Mint an API token |
| [`heyarr token list`](heyarr_token_list.md) | List API tokens |
| [`heyarr token revoke`](heyarr_token_revoke.md) | Revoke an API token |
| [`heyarr token`](heyarr_token.md) | Manage API tokens (ADR-0011) |
| [`heyarr version`](heyarr_version.md) | Print build information |
| [`heyarr worker`](heyarr_worker.md) | Execute leased jobs |
| [`heyarr works list`](heyarr_works_list.md) | List works |
| [`heyarr works show`](heyarr_works_show.md) | Show one work |
| [`heyarr works`](heyarr_works.md) | Browse the catalog |
