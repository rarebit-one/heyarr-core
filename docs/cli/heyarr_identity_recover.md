## heyarr identity recover

Reconstruct your user identity from its recovery secret, offline (ADR-0022)

### Synopsis

Rebuild your user identity from the recovery secret you saved at
`heyarr identity generate` — on a machine with no surviving device and with
NO Heyarr running.

The secret derives the SAME identity keypair, so the reconstructed identity has
the public key peers already pinned: a recovered user re-issues device
enrolments and nothing is re-pinned (ADR-0048). This command then enrols THIS
machine's device under the recovered identity, so it can authenticate as you
immediately — generating a device key first if there is none.

A mistyped secret is rejected loudly by its checksum rather than reconstructing
a different, wrong identity (ADR-0022). The whole flow is offline: it reads the
secret, derives the key and signs a cert, touching no server.

The secret is read from --secret-file, or from --secret, or from standard input
— prefer a file or a pipe, since a secret in argv is visible in ps and shell
history.

```
heyarr identity recover [flags]
```

### Options

```
      --force                recover over an existing identity here — unrecoverable if it differs
      --json                 emit machine-readable JSON
      --lifetime duration    how long the fresh device cert is valid (default: the 90-day enrolment lifetime)
      --name string          what to call the recovered identity (default: derived from this machine's hostname)
      --secret string        the recovery secret (prefer --secret-file or a pipe: a secret in argv is visible in ps)
      --secret-file string   read the recovery secret from this file
```

### Options inherited from parent commands

```
  -c, --config string         path to the configuration file (default: built-in defaults plus HEYARR_ environment)
      --device-dir string     where this machine's device key lives (default: your config directory; HEYARR_DEVICE_DIR overrides)
      --identity-dir string   where your user identity lives (default: your config directory; HEYARR_IDENTITY_DIR overrides)
```

### See also

* [heyarr identity](heyarr_identity.md)	 - Manage your user identity and enrol this machine's device (§40, ADR-0048)
