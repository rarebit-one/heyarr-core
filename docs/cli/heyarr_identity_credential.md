## heyarr identity credential

Print an Authorization credential for this enrolled device

### Synopsis

Print the value this device presents to authenticate as your user.

It is the held enrolment cert joined to a FRESH possession proof, signed here
with the device private key — so it proves both that a user vouched for this
device AND that this caller holds the device key (ADR-0048). The proof is
short-lived, so mint one per request rather than caching it.

With --header it prints the whole `Authorization: Device …` header,
ready to pass to a client. Without it, just the credential value.

```
heyarr identity credential [flags]
```

### Options

```
      --header   print the whole Authorization header line, not just the value
```

### Options inherited from parent commands

```
  -c, --config string         path to the configuration file (default: built-in defaults plus HEYARR_ environment)
      --device-dir string     where this machine's device key lives (default: your config directory; VOIDBIND_DEVICE_DIR overrides)
      --identity-dir string   where your user identity lives (default: your config directory; VOIDBIND_IDENTITY_DIR overrides)
```

### See also

* [heyarr identity](heyarr_identity.md)	 - Manage your user identity and enrol this machine's device (§40, ADR-0048)
