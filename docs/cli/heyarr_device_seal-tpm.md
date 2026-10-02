## heyarr device seal-tpm

Disabled: TPM custody returns with void-which-binds-go's custody/tpm (ADR-0021)

### Synopsis

Disabled until void-which-binds-go's custody/tpm backend lands, after the C2
cutover (void-which-binds-go ADR-0021).

This command sealed a device's existing X25519 seed into heyarr's legacy TPM
blob for the vault's `tpm` backend. Every device key is now gen2
(ADR-0022), and the library's TPM backend will never read the legacy blob, so a
gen2 key sealed with it would be stranded in a format nothing supports. It now
refuses, whatever its flags.

Until custody/tpm lands, a machine with no other gate holds its device keys in a
passphrase-sealed file (`heyarr device generate --custody sealedfile`),
and an unattended service device keeps a software device.

```
heyarr device seal-tpm [flags]
```

### Options

```
      --out string          ignored: the command is disabled
      --pcr ints            ignored: the command is disabled
      --tpm-device string   ignored: the command is disabled
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOID_WHICH_BINDS_DEVICE_DIR overrides)
```

### See also

* [heyarr device](heyarr_device.md)	 - Manage this machine's device key (§40, ADR-0032)
