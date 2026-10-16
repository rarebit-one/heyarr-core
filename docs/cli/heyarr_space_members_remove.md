## heyarr space members remove

Stop sharing a space with a user: record the removal and name the rotation that completes it

### Synopsis

Remove a user from an encrypted space.

The controller records the removal. From then on no new copy of the space key
is admitted for the user's devices — not by a re-wrap, and not by a rotation,
which must name them in --revoke instead. The same caller rule as 'add'.

This is NOT yet forward secrecy. The user's devices keep every copy they hold,
and read everything sealed under that key, until the space is re-keyed without
them (ADR-0103). The command prints that rotation, with the recipients to
revoke, for an admin token to run:

  heyarr space rotate <space-id> --revoke <device-key> [--revoke ...]

Rotation is refused for every space that is not a playlist (a vault drive
among them) until older clients understand key epochs (#698, #706). Until that
guard lifts, removing a member from such a space closes the gate on new copies
and leaves the current key with the devices that already hold it — say so to
the owner rather than letting the record suggest otherwise.

```
heyarr space members remove <space-id> --user <principal> [flags]
```

### Options

```
      --addr string        where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --json               emit machine-readable JSON
      --timeout duration   how long one request may take (default 30s)
      --user string        the user to remove, by principal name or id
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOID_WHICH_BINDS_DEVICE_DIR overrides)
```

### See also

* [heyarr space members](heyarr_space_members.md)	 - List, add and remove the members of a shared space (ADR-0108)
