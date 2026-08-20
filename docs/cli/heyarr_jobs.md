## heyarr jobs

Inspect and retry durable work

### Synopsis

Every unit of work Heyarr does is a durable, leased, idempotent job (§75,
ADR-0008).

The state worth understanding is the difference between failed and dead:
"failed" is a spent attempt that the queue will retry with backoff, and "dead"
is terminal — attempts are exhausted and nothing further will happen until an
operator retries it.

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr](heyarr.md)	 - Self-hosted content lifecycle, replication and consumption
* [heyarr jobs list](heyarr_jobs_list.md)	 - List jobs
* [heyarr jobs retry](heyarr_jobs_retry.md)	 - Put a finished job back on the queue
* [heyarr jobs show](heyarr_jobs_show.md)	 - Show one job
