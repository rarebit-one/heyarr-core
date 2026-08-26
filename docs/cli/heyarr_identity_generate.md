## heyarr identity generate

Generate your user identity keypair

### Synopsis

Generate the Ed25519 keypair that is the root of your authority.

The private key is written with mode 0600 and is never printed, logged or
returned by any command here — only its public half, as ed25519:<64 hex>, which
is what an operator pins at a peer.

Regenerating replaces the identity, which is unrecoverable: every device this
identity enrolled verifies against its public key, so a replaced key invalidates
them all. A second generate refuses unless you pass --force.

```
heyarr identity generate [flags]
```

### Options

```
      --force         replace an existing identity — unrecoverable
      --json          emit machine-readable JSON
      --name string   what to call this identity (default: derived from this machine's hostname)
```

### Options inherited from parent commands

```
  -c, --config string         path to the configuration file (default: built-in defaults plus HEYARR_ environment)
      --device-dir string     where this machine's device key lives (default: your config directory; HEYARR_DEVICE_DIR overrides)
      --identity-dir string   where your user identity lives (default: your config directory; HEYARR_IDENTITY_DIR overrides)
```

### See also

* [heyarr identity](heyarr_identity.md)	 - Manage your user identity and enrol this machine's device (§40, ADR-0048)
