## heyarr pair enrol

New device: join through an invite and store the membership op

### Synopsis

Run this on the NEW device with the invite `heyarr pair authorise` printed.
It generates (or reuses) this machine's device keys, contributes them to the
handshake, derives the short code, and, once you confirm the other side shows the
same code, receives the membership add op that admits this device, with the ops
that authorise it. It checks that the op admits THIS device into the invite's
identity, signed by the side it compared codes with, and stores both
(ADR-0068). Afterwards this device authenticates as your user.

--relay overrides the relay the invite names, for when this device reaches the
node by a different address. It takes the same forms as authorise's --relay.

```
heyarr pair enrol [flags]
```

### Options

```
      --confirm-sas string             proceed only if the derived code equals this value — the scripted stand-in for a human comparison
      --device-dir string              where this machine's device key lives (default: your config directory; VOIDBIND_DEVICE_DIR overrides)
      --invite heyarr pair authorise   the voidbind:pair?... invite printed by heyarr pair authorise
      --poll duration                  how often to re-check the relay for the next handshake step (default 150ms)
      --relay string                   reach the relay at this address instead of the invite's (a unix socket path, unix:///path, http://host:port or host:port)
      --timeout duration               how long to wait for the whole handshake before giving up (default 2m0s)
      --yes                            assume the codes matched, without prompting (use only when you compared them another way)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr pair](heyarr_pair.md)	 - Admit a new device from one that can already vouch for you (§40, ADR-0022)
