## heyarr space revoke-executor

Withdraw an executor's grant on a space and delete its copy of the key

### Synopsis

Withdraw an executor's access to a space. In one transaction the controller
revokes the grant, so the executor's next fetch of the space is refused, and
deletes every copy of the space key wrapped for the executor's service
recipients, so it can unwrap nothing more from this node.

This is NOT forward secrecy. An executor that already unwrapped the key keeps
it, and could read anything sealed under it that reaches it by other means. A
peer that replicated its copy keeps that copy too. Only a rotation that leaves
the executor out ends that:

  heyarr space rotate <space-id> --revoke <executor-key>    # first
  heyarr space revoke-executor <space-id> --executor <name> # then

Rotate first: once this command has deleted the copy, the key is no longer a
current recipient to revoke. Rotation is refused for every space that is not a
playlist (a vault drive among them) until older clients understand key epochs
(#698, #706). Until then, revoking an executor from a vault space closes the
fetch gate and deletes the wrap, and the space keeps its key.

```
heyarr space revoke-executor <space-id> --executor <principal> [flags]
```

### Options

```
      --addr string        where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --executor string    the executor principal, by name or id
      --json               emit machine-readable JSON
      --timeout duration   how long one request may take (default 30s)
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOID_WHICH_BINDS_DEVICE_DIR overrides)
```

### See also

* [heyarr space](heyarr_space.md)	 - Create and read encrypted personal-state spaces (§38, §42, ADR-0049)
