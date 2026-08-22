## heyarr system drift

Report how far a running instance has drifted from what was expected

### Synopsis

Compare a running instance against what it should be, and say HOW FAR behind
it is rather than whether it differs.

Two comparisons are made and they are reported separately, because they drift
separately. A binary that is current with its migrations unapplied is not a
mild case of being behind — it is a build running against a schema it was never
tested on — and one combined "up to date" flag would let either failure hide
the other.

  build   the version and commit the instance reports, against the expectation.
          Ordering comes from the semantic version when both sides carry one;
          otherwise the commits decide whether the builds are the same build,
          and the answer is "mismatch" rather than a guessed distance.
  schema  the migration version applied to its database, against the highest
          migration a binary embeds. Reported as a count of migrations, which
          is the number that says whether an upgrade is routine or wants a
          backup taken first.

By default the expectation is THIS binary: its own version and commit, and the
migrations it was compiled with. That answers the question somebody at a
terminal actually has — "is that host running what I have here?" — and it needs
no network access to anything but the instance itself. Override any part of it
with --expected-version, --expected-commit and --expected-schema to check
against a release you are not currently holding.

Nothing here reports "current" when it could not compare. An expectation it
cannot order is reported as "unknown", because a check that has quietly stopped
comparing looks exactly like a fleet that never drifts — which is how a
deployment ran two milestones behind with everything green (#132).

Exits non-zero when either half has drifted, so this can be a cron job.

```
heyarr system drift [flags]
```

### Options

```
      --addr string               where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --expected-commit string    the commit the instance should be built from (default: this binary's)
      --expected-schema int       the schema version its database should be at (default: the highest migration this binary embeds)
      --expected-version string   the version the instance should be running (default: this binary's)
      --json                      emit machine-readable JSON
      --timeout duration          how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string              bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string         read the bearer token from this file (default: <data_dir>/cli.token when it exists)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr system](heyarr_system.md)	 - Report what a running instance is and how far behind it has drifted
