## heyarr device generate

Generate this machine's device key

### Synopsis

Generate the Ed25519 signing key and X25519 encryption key that identify this
machine.

--custody decides where the private keys are held:

  software    (the default) two seed files in the device directory, mode 0600.
  sealedfile  one passphrase-sealed file, device.sealed in the device directory
              (void-which-binds-go ADR-0021). The seeds are drawn in memory and
              sealed straight into it, so no seed is ever written in the clear.
              The passphrase is asked for twice on the terminal, without echo,
              or read from --passphrase-file (or HEYARR_DEVICE_PASSPHRASE_FILE).
              It must be at least 12 characters.
              Every later command that signs or unwraps with this device asks
              for it again, once per command.

A sealed file protects the keys at rest — a stolen disk, a lost laptop, a backup
— as strongly as the passphrase does. It is not hardware: anyone with the file
can guess offline, slowed only by its Argon2id cost, and code running as you can
read the keys while they are unlocked.

No private key is ever printed, logged or returned by any command here — only
the public halves, as ed25519:<64 hex> and x25519:<64 hex>.

Regenerating replaces the keys, which is unrecoverable: space keys are wrapped
for a public key (§41), and a key that has been replaced cannot unwrap what the
old one could. So a second generate refuses unless you pass --force.

```
heyarr device generate [flags]
```

### Options

```
      --custody string           where the private keys are held: software (seed files) or sealedfile (a passphrase-sealed file) (default "software")
      --force                    replace an existing key — unrecoverable
      --json                     emit machine-readable JSON
      --name string              what to call this device (default: this machine's hostname)
      --passphrase-file string   with --custody sealedfile, read the passphrase from this file's first line (- for stdin) instead of the terminal
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOID_WHICH_BINDS_DEVICE_DIR overrides)
```

### See also

* [heyarr device](heyarr_device.md)	 - Manage this machine's device key (§40, ADR-0032)
