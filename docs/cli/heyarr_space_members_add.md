## heyarr space members add

Share a space with a user: record the membership and wrap the key for their devices

### Synopsis

Add a user as a member of an encrypted space.

Two steps, one command. The controller records the membership and names the
user's enrolled device encryption keys; then this device opens the space with
its own copy of the key, wraps the current key for each of those devices and
pushes the copies. From then on the member's devices read the space, and
everything written before it, through the key history.

This device must be one the space is wrapped for, enrolled, and authorised for
management; if the space has a recorded owner, it must be that owner's device
(on a family space, any such device will do). A user with no enrolled device is
refused: they enrol first.

If the space is re-keyed while this runs, the controller refuses the stale
wraps and the membership stands without them; run the command again to wrap
at the new epoch. Running it again is always safe.

Undo with 'space members remove'.

```
heyarr space members add <space-id> --user <principal> [flags]
```

### Options

```
      --addr string        where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --json               emit machine-readable JSON
      --timeout duration   how long one request may take (default 30s)
      --user string        the user to add, by principal name or id
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOID_WHICH_BINDS_DEVICE_DIR overrides)
```

### See also

* [heyarr space members](heyarr_space_members.md)	 - List, add and remove the members of a shared space (ADR-0108)
