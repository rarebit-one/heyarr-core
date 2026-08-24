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
* [heyarr renderers pause](heyarr_renderers_pause.md)	 - Hold position on a renderer
* [heyarr renderers resume](heyarr_renderers_resume.md)	 - Continue a paused renderer
* [heyarr renderers seek](heyarr_renderers_seek.md)	 - Jump to a position
* [heyarr renderers status](heyarr_renderers_status.md)	 - Report what a renderer is playing and how far in
* [heyarr renderers stop](heyarr_renderers_stop.md)	 - Stop a renderer and release the content
