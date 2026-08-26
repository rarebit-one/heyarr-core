## heyarr pair

Authorise a new device from an already-enrolled one (§40, ADR-0022)

### Synopsis

Enrol a NEW device by an OLD, already-enrolled one, over a dumb relay.

Run `heyarr pair authorise` on the OLD device (the one that holds your
user identity) and `heyarr pair enrol` on the NEW device, pointing both
at the same running Heyarr's relay and the same session id. Each prints a short
code; compare them, and if they match the old device signs a cert the new device
stores. The server only relays public values — it learns no key material and
vouches for nothing (ADR-0038).

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr pair authorise](heyarr_pair_authorise.md)	 - Old device: authorise a new device and sign its enrolment cert
* [heyarr pair enrol](heyarr_pair_enrol.md)	 - New device: pair with an old device and store the enrolment cert
* [heyarr pair sas](heyarr_pair_sas.md)	 - Compute the short authentication string for two keys and a salt
