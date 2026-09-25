## heyarr pair authorise

Existing side: admit a new device by signing its membership op

### Synopsis

Run this where your user identity lives, or on a device that is already a
member of your identity. It opens a session on the relay, prints an invite for
the new device, derives the short code, and, once you confirm the new device
shows the same code, signs a membership add op for the new device's keys and
hands it over sealed to the new device's encryption key (ADR-0068).

--as picks what signs:
  identity  your user identity (the genesis key), read from --identity-dir;
            the private key is used through a signer and never leaves its store
  device    this machine's device, which must already be a member
  auto      identity when one is present here, otherwise device (the default)

Either way, a local device enrolled under the same identity contributes the
membership ops it knows and records the new add afterwards.

When stdout is a terminal the invite is also drawn as a QR code, so the new
device can scan it off the screen. --qr draws it anyway and --no-qr never does.
The invite string is printed either way, on its own line, for scripts.

```
heyarr pair authorise [flags]
```

### Options

```
      --as string             what signs the admission: identity, device, or auto (identity when present here) (default "auto")
      --confirm-sas string    proceed only if the derived code equals this value — the scripted stand-in for a human comparison
      --device-dir string     where this machine's device key lives (default: your config directory; VOIDBIND_DEVICE_DIR overrides)
      --identity-dir string   where your user identity lives (default: your config directory; VOIDBIND_IDENTITY_DIR overrides)
      --lifetime duration     how long an admission signed as the identity is valid (default: the enrolment lifetime)
      --no-qr                 never draw the invite as a QR code
      --poll duration         how often to re-check the relay for the next handshake step (default 150ms)
      --qr                    draw the invite as a QR code even when stdout is not a terminal (default: drawn only on a terminal)
      --relay string          the running Heyarr's relay: a unix socket path, unix:///path, http://host:port or host:port
      --timeout duration      how long to wait for the whole handshake before giving up (default 2m0s)
      --yes                   assume the codes matched, without prompting (use only when you compared them another way)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr pair](heyarr_pair.md)	 - Admit a new device from one that can already vouch for you (§40, ADR-0022)
