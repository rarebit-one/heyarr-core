## heyarr identity

Manage your user identity and enrol this machine's device (§40, ADR-0048)

### Synopsis

Manage the Ed25519 user identity that is the root of your authority (spec §40).

Your user identity signs the enrolment certificate that says "this device key is
mine". A device authenticates as you by presenting that cert; the cert authorises
nothing on its own (ADR-0048). The private key is generated locally, stored 0600
in your own configuration directory, and never sent anywhere — an operator pins
only its PUBLIC half at each peer, out of band, which is what makes enrolment a
deliberate human act rather than something a device can claim about itself
(ADR-0032).

### Options

```
      --device-dir string     where this machine's device key lives (default: your config directory; HEYARR_DEVICE_DIR overrides)
      --identity-dir string   where your user identity lives (default: your config directory; HEYARR_IDENTITY_DIR overrides)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr identity credential](heyarr_identity_credential.md)	 - Print an Authorization credential for this enrolled device
* [heyarr identity enrol](heyarr_identity_enrol.md)	 - Sign an enrolment cert binding this machine's device to your identity
* [heyarr identity generate](heyarr_identity_generate.md)	 - Generate your user identity keypair
* [heyarr identity show](heyarr_identity_show.md)	 - Show your user identity
