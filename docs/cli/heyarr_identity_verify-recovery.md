## heyarr identity verify-recovery

Check your written recovery secret against this identity, without using it (ADR-0022)

### Synopsis

Check the recovery secret you wrote down, before you ever need it.

The secret's checksum is verified, the identity it derives is compared with the
one stored here (its public key and its recovery encryption key), and its
fingerprint is printed so you can compare it with the one on your recovery
sheet. Nothing is signed, stored or sent: run it as often as you like, and at
least once a year.

The secret is read from --secret-file, --secret or standard input, like
`identity recover`; recovery shares, one per line, work too. A secret that
belongs to another identity is an error, so a script can gate on it.

```
heyarr identity verify-recovery [flags]
```

### Options

```
      --json                 emit machine-readable JSON
      --secret string        the recovery secret (prefer --secret-file or a pipe: a secret in argv is visible in ps)
      --secret-file string   read the recovery secret (or shares, one per line) from this file
```

### Options inherited from parent commands

```
  -c, --config string         path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string     where this machine's device key lives (default: your config directory; VOIDBIND_DEVICE_DIR overrides)
      --identity-dir string   where your user identity lives (default: your config directory; VOIDBIND_IDENTITY_DIR overrides)
```

### See also

* [heyarr identity](heyarr_identity.md)	 - Manage your user identity and enrol this machine's device (§40, ADR-0048)
