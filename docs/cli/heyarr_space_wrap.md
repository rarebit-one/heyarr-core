## heyarr space wrap

Wrap a space's key for another of your own enrolled devices (ADR-0049)

### Synopsis

Give another of your own devices the key of a space this device holds.

Pairing or enrolling a new device admits it to your identity but wraps it no
space key: until a device that holds the key wraps it for the new one, the new
device shows the space but cannot open it. The controller cannot do this for
you — it holds no key to wrap with (Invariant 6). This command is that step.

It asks the controller which of your user's enrolled devices lack a copy of the
space's current key, opens the space with this device's own copy, wraps the
current key for each device you name (or, with --all-devices, every active one
lacking a copy) and pushes the copies, which replicate with the space like any
other wrapped key. Only your own devices are targets: a device of another user
is shared with through 'space members add'. A target must be enrolled, not
revoked, and carry an encryption key (enrol-before-wrap); the controller checks
that again when the copies arrive, along with the space's membership gate.

--device takes the device identity id that pairing reports, its device key
(ed25519:...) or its encryption key (x25519:...), and may be repeated. A device
that already holds the current key is reported and skipped, so running the
command again is always safe. --dry-run lists what would be wrapped and opens
nothing.

Run it on a device that holds the space, once per space:

  mnemosyne space wrap <space-id> --all-devices

```
heyarr space wrap <space-id> (--device <id> ... | --all-devices) [flags]
```

### Options

```
      --addr string                where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --all-devices[=true|false]   wrap for every one of your active devices that lacks a copy of the current key
      --device stringArray         one of your devices to wrap the key for: its device identity id, device key or encryption key (repeatable)
      --dry-run[=true|false]       list the devices that would be wrapped for, and wrap nothing
      --json[=true|false]          emit machine-readable JSON
      --timeout duration           how long one request may take (default 30s)
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOID_WHICH_BINDS_DEVICE_DIR overrides)
```

### See also

* [heyarr space](heyarr_space.md)	 - Create and read encrypted personal-state spaces (§38, §42, ADR-0049)
