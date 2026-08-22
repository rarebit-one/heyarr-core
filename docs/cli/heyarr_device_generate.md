## heyarr device generate

Generate this machine's device key

### Synopsis

Generate the Ed25519 keypair that identifies this machine.

The private key is written with mode 0600 and is never printed, logged or
returned by any command here — only its public half, as ed25519:<64 hex>.

Regenerating replaces the key, which is unrecoverable: Milestone 8 wraps space
keys for a public key (§41), and a key that has been replaced cannot unwrap
what the old one could. So a second generate refuses unless you pass --force.

```
heyarr device generate [flags]
```

### Options

```
      --force         replace an existing key — unrecoverable
      --json          emit machine-readable JSON
      --name string   what to call this device (default: this machine's hostname)
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; HEYARR_DEVICE_DIR overrides)
```

### See also

* [heyarr device](heyarr_device.md)	 - Manage this machine's device key (§40, ADR-0032)
