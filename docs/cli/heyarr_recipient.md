## heyarr recipient

Register executors' public keys as space-key recipients (ADR-0104)

### Synopsis

Manage service recipients: executors' X25519 public keys that a space key may
be wrapped for.

An executor runs work for you against a few encrypted spaces. It is not one of
your devices and is never enrolled as one, so enrol-before-wrap refuses to wrap
a space key for it. Registering its public key here, from your own
management-authorised device, is the explicit act that lets you then grant it
spaces with `heyarr space grant`.

add, list and remove authenticate as this machine's enrolled device
(--device-dir), never with a bearer token: an admin token cannot register a
recipient.

init and show run on the EXECUTOR's host and need no controller: init draws the
executor's key straight into a passphrase-sealed file and prints its public key
and fingerprint, which the owner then types into `recipient add`.

### Options

```
      --device-dir string   where this machine's device key lives (default: your config directory; VOID_WHICH_BINDS_DEVICE_DIR overrides)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr recipient add](heyarr_recipient_add.md)	 - Register an executor's public key as a wrap recipient
* [heyarr recipient init](heyarr_recipient_init.md)	 - Create this executor's recipient key in a sealed file (run on the executor's host)
* [heyarr recipient list](heyarr_recipient_list.md)	 - List the registered service recipients
* [heyarr recipient remove](heyarr_recipient_remove.md)	 - Withdraw a service recipient, and every space-key copy wrapped for it
* [heyarr recipient show](heyarr_recipient_show.md)	 - Show this executor's recipient key and fingerprint, without unsealing it
