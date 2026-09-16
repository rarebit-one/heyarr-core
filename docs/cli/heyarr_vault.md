## heyarr vault

Push, pull and list files in an encrypted media vault (ADR-0021, ADR-0095)

### Synopsis

Work with an encrypted media vault — a person's files as a path-addressed
drive whose bytes the peer holds only as ciphertext it cannot open.

A file is sealed into fixed ciphertext frames under the space key on THIS device
(never on the peer), content-addressed, and uploaded as opaque blobs; its manifest
blob id is then recorded at a vault path in the space's drive CRDT. Reading
reverses it entirely on the device. The peer stores ciphertext blobs, encrypted
drive changes and opaque placement pins, and can open none of it (Invariant 6).

Like the space commands these need both a running controller (--config) and this
machine's device key (--device-dir): the controller stores the ciphertext, the
device holds the only key that opens it.

### Options

```
      --device-dir string   where this machine's device key lives (default: your config directory; VOIDBIND_DEVICE_DIR overrides)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr vault ls](heyarr_vault_ls.md)	 - List the live files in a vault
* [heyarr vault pull](heyarr_vault_pull.md)	 - Read a file from the vault, decrypting it on this device
* [heyarr vault push](heyarr_vault_push.md)	 - Seal a local file into the vault and record it at a vault path
