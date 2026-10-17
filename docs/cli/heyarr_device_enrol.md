## heyarr device enrol

Enrol this machine's paired device at a peer (POST /enrol, ADR-0067)

### Synopsis

Enrol this machine's device at a peer on the strength of its own credential:
the op that admitted it (from pairing), a fresh possession proof signed with the
device key, and the membership ops it knows. This is what the phone apps do
after pairing; it needs no admin token.

The peer must already pin the device's user (`admin user pin`), and the device
must be enrolled locally (paired with `void-which-binds pair-join` or
`heyarr pair enrol`). Any refusal — an unpinned user, an expired or revoked
admission, a bad proof — is the peer's deliberately opaque 401; its log has the
reason.

The device authenticates at the read floor once enrolled. Write remains an
admin's authorisation of the device key (ADR-0065). Enrolment is per peer: run
this against each peer the device talks to.

```
heyarr device enrol [flags]
```

### Options

```
      --json               emit machine-readable JSON
      --name string        the name the peer records for this device (default: the device's own name)
      --peer string        the peer to enrol at: its http(s):// URL (required)
      --timeout duration   how long the request may take (default 30s)
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOID_WHICH_BINDS_DEVICE_DIR overrides)
```

### See also

* [heyarr device](heyarr_device.md)	 - Manage this machine's device key (§40, ADR-0032)
