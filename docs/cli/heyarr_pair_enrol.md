## heyarr pair enrol

New device: pair with an old device and store the enrolment cert

### Synopsis

Run this on the NEW device. It generates (or reuses) this machine's device
key, contributes it to the handshake, derives the short code, and — once you
confirm the old device shows the same code — receives and stores the enrolment
cert the old device signs. Afterwards this device authenticates as your user.

```
heyarr pair enrol [flags]
```

### Options

```
      --confirm-sas string   proceed only if the derived code equals this value — the scripted stand-in for a human comparison
      --device-dir string    where this machine's device key lives (default: your config directory; VOIDBIND_DEVICE_DIR overrides)
      --poll duration        how often to re-check the relay for the next handshake step (default 150ms)
      --relay string         the running Heyarr's relay: a unix socket path, unix:///path, http://host:port or host:port
      --session string       the rendezvous session id both devices share (authorise generates one if empty)
      --timeout duration     how long to wait for the whole handshake before giving up (default 2m0s)
      --yes                  assume the codes matched, without prompting (use only when you compared them another way)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr pair](heyarr_pair.md)	 - Authorise a new device from an already-enrolled one (§40, ADR-0022)
