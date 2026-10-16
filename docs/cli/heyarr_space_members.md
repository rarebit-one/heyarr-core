## heyarr space members

List, add and remove the members of a shared space (ADR-0108)

### Synopsis

Manage who an encrypted space is shared with.

The space is the unit of sharing (ADR-0108): a member's enrolled devices may
each hold a wrapped copy of the space key, and a user who is not a member may
not be given one. Only the space owner's device changes membership (any
management-authorised device on a family space, or on a space with no recorded
owner); members may not re-share.

Adding a member is two steps, both done here: the controller records the
membership and names the member's enrolled device keys, then THIS device wraps
the space's current key for each of them and pushes the copies. The controller
never wraps — it holds no key to wrap with (Invariant 6).

Removing a member records the removal, after which no new copy of the key is
admitted for their devices. It does not itself re-key the space: the devices
keep the copies they hold until a rotation leaves them out. The command prints
that rotation; see 'space members remove --help' for why it may be refused.

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOID_WHICH_BINDS_DEVICE_DIR overrides)
```

### See also

* [heyarr space](heyarr_space.md)	 - Create and read encrypted personal-state spaces (§38, §42, ADR-0049)
* [heyarr space members add](heyarr_space_members_add.md)	 - Share a space with a user: record the membership and wrap the key for their devices
* [heyarr space members list](heyarr_space_members_list.md)	 - List a space's members, current and removed
* [heyarr space members remove](heyarr_space_members_remove.md)	 - Stop sharing a space with a user: record the removal and name the rotation that completes it
