## heyarr space export-recovery

Export every space's recovery-wrapped key into one recovery blob (ADR-0022)

### Synopsis

Export an encrypted recovery blob: the copy of every space's key that is
wrapped for your recovery key, gathered from this node's control database into
one small file you can keep anywhere — a USB stick, a cloud drive, an email to
yourself.

With it, `heyarr space recover --from-blob` recovers your space keys from the
recovery secret even when no control database survives. It is a DURABILITY aid,
not extra protection: the file is sealed so that only your recovery secret opens
it, and it holds nothing the peers do not already hold (ADR-0022).

The blob is sealed to your recovery PUBLIC key, so exporting needs no secret —
the key comes from your user identity (--identity-dir) or --recipient. It is a
snapshot: a space created or re-keyed afterwards is missing from it, so
re-export after either.

This reads the control database directly (--config), so it works with the
controller running or stopped.

```
heyarr space export-recovery --out <file> [flags]
```

### Options

```
      --identity-dir string   where your user identity lives (default: your config directory; VOIDBIND_IDENTITY_DIR overrides)
      --json                  emit machine-readable JSON
      --out string            write the recovery blob to this file (required)
      --recipient string      the recovery key to export for (x25519:<hex>); default: your user identity's
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOIDBIND_DEVICE_DIR overrides)
```

### See also

* [heyarr space](heyarr_space.md)	 - Create and read encrypted personal-state spaces (§38, §42, ADR-0049)
