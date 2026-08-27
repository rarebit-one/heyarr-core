## heyarr identity enrol

Sign an enrolment cert binding this machine's device to your identity

### Synopsis

Sign a user-signed enrolment certificate for this machine's device key and
store it beside the device key.

This is the client half of enrolment: your user identity vouches for this
device. The device then authenticates as you by presenting the cert. It stays
LOCAL — nothing is sent to a server here. An operator still pins your user
public key at each peer (out of band) before any peer will honour the cert; a
cert signed by a key no peer has pinned is refused there (ADR-0032).

Signing takes the private key of your user identity, so run it where that
identity lives. It reads the local device key (generate one with
`heyarr device generate` first).

```
heyarr identity enrol [flags]
```

### Options

```
      --json                emit machine-readable JSON
      --lifetime duration   how long the cert is valid (default: the 90-day enrolment lifetime)
```

### Options inherited from parent commands

```
  -c, --config string         path to the configuration file (default: built-in defaults plus HEYARR_ environment)
      --device-dir string     where this machine's device key lives (default: your config directory; VOIDBIND_DEVICE_DIR overrides)
      --identity-dir string   where your user identity lives (default: your config directory; VOIDBIND_IDENTITY_DIR overrides)
```

### See also

* [heyarr identity](heyarr_identity.md)	 - Manage your user identity and enrol this machine's device (§40, ADR-0048)
