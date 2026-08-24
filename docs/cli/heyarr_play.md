## heyarr play

Play an asset on a television, speaker or projector (§68)

### Synopsis

Play an asset on a renderer found on the controller's network.

The renderer is named the way you would say it out loud — "living room" — and
matched against what the device calls itself. Run `heyarr renderers discover`
to see what is there.

This plans the playback first, and a plan that is not DIRECT is refused with
the reason: your device does not declare this codec, or the only replica is on
another peer. That refusal is the answer, not an error to work around.

```
heyarr play <asset-id> <renderer> [flags]
```

### Options

```
      --addr string         where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --json                emit machine-readable JSON
      --timeout duration    how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string        bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string   read the bearer token from this file (default: <data_dir>/cli.token when it exists)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
