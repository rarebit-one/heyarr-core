## heyarr device remove

Remove a device key

### Synopsis

Delete a device key and its record from this machine.

There is no escrow and no copy: once removed, the key is gone. The id is
required and is matched exactly, because an unrecoverable command that accepts
"whatever is there" eventually runs against the wrong thing.

```
heyarr device remove <id> [flags]
```

### Options

```
      --json   emit machine-readable JSON
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOIDBIND_DEVICE_DIR overrides)
```

### See also

* [heyarr device](heyarr_device.md)	 - Manage this machine's device key (§40, ADR-0032)
