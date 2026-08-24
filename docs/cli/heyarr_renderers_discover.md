## heyarr renderers discover

Search the local network for media renderers

### Synopsis

Search the local network for UPnP MediaRenderers.

An empty result means nothing answered within the search window — NOT that
there is nothing there. A television in standby closes every listener and on
some models leaves the network entirely, so a screen that is switched off is
indistinguishable from a screen that does not exist. Switch it on and search
again before concluding anything.

With --profile, each renderer is also asked what formats it accepts, and the
answer is mapped into the capability profile the playback planner uses. That is
one extra round trip per device.

```
heyarr renderers discover [flags]
```

### Options

```
      --json               emit machine-readable JSON
      --profile            also ask each renderer what it can play
      --timeout duration   how long to listen for answers; devices may wait up to 3s before replying (default 5s)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr renderers](heyarr_renderers.md)	 - Find media renderers on the local network (§68)
