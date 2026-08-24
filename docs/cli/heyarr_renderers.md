## heyarr renderers

Find media renderers on the local network (§68)

### Synopsis

Find UPnP MediaRenderers — televisions, speakers, projectors — on the network
this machine is attached to.

A renderer is not yet a Device. Discovery reports what answered and what it
says it can play; registering one as a Device, so the planner can decide
against it, is a separate step.

Discovery is multicast and does not leave the local segment: run it on the same
network as the screen you are looking for.

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr renderers discover](heyarr_renderers_discover.md)	 - Search the local network for media renderers
