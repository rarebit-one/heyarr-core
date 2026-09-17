## heyarr device seal-tpm

Seal this device's encryption key to the TPM for hardware-gated custody (ADR-0098)

### Synopsis

Seal this device's X25519 encryption key to the local TPM, so the vault's
`tpm` custody backend opens spaces by unsealing after a TPM gate — a PCR
policy (the boot state) AND a PIN — rather than reading a key off disk.

It seals the EXISTING device key, so the public point stays the same and every
space already wrapped for this device keeps opening. The seed is written into a
TPM sealed object; the on-disk blob holds only the public point and ciphertext
the TPM alone can open.

The PIN is the sealed object's auth value, read from vault.tpm.pin_file or the
HEYARR_VAULT_TPM_PIN environment variable — the same source the backend
presents at unseal, so provisioning and opening agree. Requires a TPM 2.0
(Linux); the household Framework laptops (fTPM/PTT) are the target.

After sealing, select it with vault.unwrapper: tpm and confirm a space opens;
then you may remove the plaintext device key to complete the hardening — your
paper recovery secret still restores access if the TPM is ever lost or reset.

```
heyarr device seal-tpm [flags]
```

### Options

```
      --out string          where to write the sealed-key blob (default: vault.tpm.sealed_key_file)
      --pcr ints            PCR indices to bind the policy to (default: 7, the UEFI Secure Boot state) (default [7])
      --tpm-device string   TPM device to open (default: /dev/tpmrm0, or vault.tpm.device)
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOIDBIND_DEVICE_DIR overrides)
```

### See also

* [heyarr device](heyarr_device.md)	 - Manage this machine's device key (§40, ADR-0032)
