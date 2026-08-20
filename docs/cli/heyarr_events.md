## heyarr events

Follow the event log

### Synopsis

Every state transition Heyarr makes emits an event (§76, ADR-0009).

The stream is the integration model: an external tool watches it instead of
polling, and reconnection is gapless — a client that saw sequence N reconnects
with --after N and receives everything since, with no hole and no duplicate.

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr events tail](heyarr_events_tail.md)	 - Print events as they happen
