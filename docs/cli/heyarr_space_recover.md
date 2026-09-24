## heyarr space recover

Recover vault space keys from your recovery secret, offline (ADR-0022, ADR-0049)

### Synopsis

Recover the space keys of your encrypted vaults from the recovery secret you
saved at `heyarr identity generate` — on a machine with NO Heyarr running.

Every space is stored with a copy of its key sealed for your recovery encryption
key (ADR-0049). This re-derives that key's private half from the secret, offline,
and unwraps those copies from THIS node's control database. It is distinct from
`heyarr recover` (which rebuilds a peer's control plane) and
`heyarr identity recover` (which rebuilds your signing identity): this
one recovers the ability to READ vault content.

With --rewrap it also re-seals each recovered key for THIS machine's device key
(ADR-0022's recovery tail), so the recovered machine keeps reading the spaces
without the paper secret. That writes to the control database, so run it with the
controller stopped; the device must be enrolled first (`heyarr identity recover`
does that).

The whole flow is offline. Key material is never printed — only which spaces were
opened — so the output is safe to log.

With --from-blob the wrapped copies come from an exported recovery blob
(`heyarr space export-recovery`) instead of the control database, so recovery
needs no database at all. Anyone who knows your recovery PUBLIC key could make a
blob, so a key from one is not trusted for writing on its word: with --rewrap,
each key must first decrypt the newest content this node's database holds for
its space, and a key that does not (a stale blob from before the space was
re-keyed, or a forged one) is refused (ADR-0022 addendum).

The secret is read from --secret-file, or from --secret, or from standard input
— prefer a file or a pipe, since a secret in argv is visible in ps and shell
history.

```
heyarr space recover [flags]
```

### Options

```
      --from-blob string     take the wrapped copies from this exported recovery blob instead of the control database
      --json                 emit machine-readable JSON
      --rewrap               also re-seal the recovered keys for THIS machine's device so it keeps reading the spaces (writes the control DB; run with the controller stopped)
      --secret string        the recovery secret (prefer --secret-file or stdin; argv is visible in ps)
      --secret-file string   read the recovery secret from this file
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOIDBIND_DEVICE_DIR overrides)
```

### See also

* [heyarr space](heyarr_space.md)	 - Create and read encrypted personal-state spaces (§38, §42, ADR-0049)
