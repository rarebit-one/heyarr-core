## heyarr device pair-offload

Pair this desktop with your phone for cruciform-offload custody (ADR-0098)

### Synopsis

Pair this desktop with your phone (one.rarebit.cruciform) so the vault's
`cruciform` custody backend can open spaces without any device key on this
machine: each unwrap wakes the phone, which hardware-gates and returns the key.

Run this once. It creates a rendezvous on the node's voidbind relay and prints a
`voidbind:offload-pair?…` invite — the payload the phone scans as a QR (a
desktop GUI renders it; the CLI prints the text). Both screens then show a short
code; compare them, and on a match this desktop pins the phone's keys and the
phone pins this desktop's transport key.

The transport key it pins is NOT a device encryption key (this backend holds
none) — it only authenticates unwrap requests as coming from this paired
terminal. Re-running reuses the existing transport key if one is already paired.

```
heyarr device pair-offload [flags]
```

### Options

```
      --confirm-sas string   proceed only if the derived code equals this value — the scripted stand-in for a human comparison
      --out string           where to write the pairing config (default: cruciform-pairing.json in the device directory)
      --poll duration        how often to re-check the relay for the phone's next step (default 150ms)
      --relay string         the node's voidbind relay base the phone also reaches: unix:///path, http://host:port/pair, or host:port/pair
      --timeout duration     how long to wait for the whole pairing before giving up (default 2m0s)
      --yes                  assume the codes matched, without prompting (use only when you compared them another way)
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOIDBIND_DEVICE_DIR overrides)
```

### See also

* [heyarr device](heyarr_device.md)	 - Manage this machine's device key (§40, ADR-0032)
