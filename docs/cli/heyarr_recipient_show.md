## heyarr recipient show

Show this executor's recipient key and fingerprint, without unsealing it

### Synopsis

Print the public key and fingerprint of the recipient key sealed at --sealed:
the same two values `recipient init` printed. They are read from the file's clear
header, so no PIN is asked for and the seal is never opened.

```
heyarr recipient show --sealed <path> [flags]
```

### Options

```
      --json            emit machine-readable JSON
      --sealed string   the sealed key file (required)
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOID_WHICH_BINDS_DEVICE_DIR overrides)
```

### See also

* [heyarr recipient](heyarr_recipient.md)	 - Register executors' public keys as space-key recipients (ADR-0104)
