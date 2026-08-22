## heyarr device mcp

Run the Personal MCP for this machine's device key (§73)

### Synopsis

Serve the Personal MCP over stdio, for an agent running on THIS machine.

This is not the Heyarr MCP. The Heyarr MCP is served by the controller and
covers the library, acquisition, peers and playback; it cannot see private
state and never will (§72). This one runs here, reads no network socket, and
exposes exactly the key-management verbs this device can perform.

It speaks newline-delimited JSON-RPC 2.0 on stdin and stdout, so configure your
agent to launch it as a command rather than to dial a URL. Nothing but protocol
messages goes to stdout.

```
heyarr device mcp
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; HEYARR_DEVICE_DIR overrides)
```

### See also

* [heyarr device](heyarr_device.md)	 - Manage this machine's device key (§40, ADR-0032)
