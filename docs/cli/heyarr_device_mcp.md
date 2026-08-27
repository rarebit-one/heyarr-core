## heyarr device mcp

Run the Personal MCP for this machine's device key and personal state (§73)

### Synopsis

Serve the Personal MCP over stdio, for an agent running on THIS machine.

This is not the Heyarr MCP. The Heyarr MCP is served by the controller and
covers the library, acquisition, peers and playback; it cannot see private
state and never will (§72). This one runs here and exposes the key-management
verbs this device can perform.

With --config it also exposes the READ tools over your encrypted personal state
(§73): it fetches the ciphertext from the controller, unwraps the space key with
THIS device's key, and decrypts and merges the playlist locally — the controller
sees only ciphertext and can read none of it. Without --config it serves the
device-key tools alone.

It speaks newline-delimited JSON-RPC 2.0 on stdin and stdout, so configure your
agent to launch it as a command rather than to dial a URL. Nothing but protocol
messages goes to stdout.

```
heyarr device mcp [flags]
```

### Options

```
      --config string   connect to this controller to expose the read tools over your encrypted personal state (§73)
```

### Options inherited from parent commands

```
      --device-dir string   where this machine's device key lives (default: your config directory; HEYARR_DEVICE_DIR overrides)
```

### See also

* [heyarr device](heyarr_device.md)	 - Manage this machine's device key (§40, ADR-0032)
