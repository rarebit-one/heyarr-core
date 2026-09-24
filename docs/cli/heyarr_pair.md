## heyarr pair

Admit a new device from one that can already vouch for you (§40, ADR-0022)

### Synopsis

Admit a NEW device from one that can already vouch for your identity, over a
dumb relay.

Run `heyarr pair authorise` where your user identity lives, or on a
device that is already a member. It opens a session on a running Heyarr's relay
and prints an invite. Run `heyarr pair enrol --invite <invite>` on the
NEW device. Each side prints a short code. Compare them, and if they match the
authorising side signs a membership op that admits the new device. The server
only relays public values and one sealed message. It learns no key material and
vouches for nothing (ADR-0038).

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr pair authorise](heyarr_pair_authorise.md)	 - Existing side: admit a new device by signing its membership op
* [heyarr pair enrol](heyarr_pair_enrol.md)	 - New device: join through an invite and store the membership op
* [heyarr pair sas](heyarr_pair_sas.md)	 - Compute the short authentication string for two keys and a salt
