## heyarr device

Manage this machine's device key (§40, ADR-0032)

### Synopsis

Manage the Ed25519 device key that identifies this machine as one of your
devices (spec §40).

The key is generated locally, stored 0600 in your own configuration directory,
and never sent anywhere. It is not the peer identity: that belongs to the
server and lives in its data directory.

It also does not authorise anything yet. Nothing is enrolled, nothing is
wrapped for it, and every grant against a Heyarr controller is still a bearer
token scope (ADR-0011) until Milestone 8. The key exists now so that Milestone
8 populates a shape rather than retrofitting one — see ADR-0032.

### Options

```
      --device-dir string   where this machine's device key lives (default: your config directory; HEYARR_DEVICE_DIR overrides)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr device generate](heyarr_device_generate.md)	 - Generate this machine's device key
* [heyarr device list](heyarr_device_list.md)	 - List this machine's device keys
* [heyarr device mcp](heyarr_device_mcp.md)	 - Run the Personal MCP for this machine's device key and personal state (§73)
* [heyarr device remove](heyarr_device_remove.md)	 - Remove a device key
* [heyarr device show](heyarr_device_show.md)	 - Show one device key
