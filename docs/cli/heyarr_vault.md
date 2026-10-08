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

An executor (ADR-0104) is not a device. It authenticates with its restricted
token and opens spaces with its sealed service-recipient key instead:
--sealed-key <file> (from `heyarr recipient init`), its PIN read from the
systemd credential heyarr-recipient-pin or --pin-file. It sees only the
spaces an owner's device granted it, and decrypts only those wrapped for it.

### Options

```
      --device-dir string   where this machine's device key lives (default: your config directory; VOID_WHICH_BINDS_DEVICE_DIR overrides)
      --pin-file string     an owner-only file holding the sealed key's PIN (default: vault.sealedfile.pin_file, else the systemd credential heyarr-recipient-pin)
      --sealed-key file     the file holding the sealed recipient key from heyarr recipient init (implies --unwrapper sealedfile; default: vault.sealedfile.key_file)
      --unwrapper string    the custody backend that opens space keys (default: vault.unwrapper); sealedfile is an executor's service-recipient key (ADR-0104)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr vault get-ref](heyarr_vault_get-ref.md)	 - Read one sealed object by its vault ref, decrypting it on this machine
* [heyarr vault ls](heyarr_vault_ls.md)	 - List the live files in a vault
* [heyarr vault pull](heyarr_vault_pull.md)	 - Read a file from the vault, decrypting it on this device
* [heyarr vault push](heyarr_vault_push.md)	 - Seal a local file into the vault and record it at a vault path
* [heyarr vault put-ref](heyarr_vault_put-ref.md)	 - Seal one JSON object into a vault space and print its new ref
