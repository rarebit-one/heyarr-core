## heyarr recipient init

Create this executor's recipient key in a sealed file (run on the executor's host)

### Synopsis

Create this executor's X25519 recipient key (ADR-0104). The key is drawn in
memory and sealed straight into a passphrase-sealed file at --sealed; its
private half is never written anywhere in the clear. An existing file is never
replaced: every space wrapped for its key would become unreadable.

The PIN that seals it comes from the systemd credential heyarr-recipient-pin
($CREDENTIALS_DIRECTORY, delivered by LoadCredentialEncrypted=), or from an
owner-only --pin-file. It is never an argument or an environment variable.

It prints the public key and its fingerprint. Read them off this host's console
and give them to the owner, who types the fingerprint into `heyarr recipient add`
on their own device: nothing that relays the key is trusted to publish it.

```
heyarr recipient init --sealed <path> [flags]
```

### Options

```
      --json                    emit machine-readable JSON
      --pin-credential string   the systemd credential name the PIN is delivered under (default "heyarr-recipient-pin")
      --pin-file string         an owner-only file holding the PIN (default: the systemd credential)
      --sealed string           where to write the sealed key file (required)
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOID_WHICH_BINDS_DEVICE_DIR overrides)
```

### See also

* [heyarr recipient](heyarr_recipient.md)	 - Register executors' public keys as space-key recipients (ADR-0104)
