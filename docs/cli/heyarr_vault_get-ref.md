## heyarr vault get-ref

Read one sealed object by its vault ref, decrypting it on this machine

### Synopsis

Read the sealed object a vault ref names — hv1:<space uuid>/<object uuid>, the
form an outside system that never decrypts holds — and write its JSON to -o, or
to stdout. The object is the drive file .jumpdrive/objects/<object uuid>.json.

This is the read an executor makes (ADR-0104): with its restricted token, it
sees only the spaces an owner's device granted it, and with its sealed
recipient key (--sealed-key) it decrypts only those whose key was wrapped for
it. The plaintext goes to stdout or to the -o file (created owner-only, never
over an existing file) and nowhere else: never to stderr, never to a log. A
caller should give a path on a tmpfs it wipes.

Exit status: 3 the sealed key or its PIN is unavailable or wrong; 4 the space is
not visible to this credential (no grant, revoked, or no such space); 5 the
space cannot be decrypted with this key; 6 the ref names no object; 7 the node
served bytes that do not match their content address (a substituted or
corrupted manifest or content blob); 1 anything else.

```
heyarr vault get-ref hv1:<space>/<object> [flags]
```

### Options

```
      --addr string         where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --json                emit machine-readable JSON
  -o, --out string          write the object to this new file (mode 0600) instead of stdout (default "-")
      --timeout duration    how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string        bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string   read the bearer token from this file (default: <data_dir>/cli.token when it exists)
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOID_WHICH_BINDS_DEVICE_DIR overrides)
      --pin-file string     an owner-only file holding the sealed key's PIN (default: vault.sealedfile.pin_file, else the systemd credential heyarr-recipient-pin)
      --sealed-key file     the file holding the sealed recipient key from heyarr recipient init (implies --unwrapper sealedfile; default: vault.sealedfile.key_file)
      --unwrapper string    the custody backend that opens space keys (default: vault.unwrapper); sealedfile is an executor's service-recipient key (ADR-0104)
```

### See also

* [heyarr vault](heyarr_vault.md)	 - Push, pull and list files in an encrypted media vault (ADR-0021, ADR-0095)
