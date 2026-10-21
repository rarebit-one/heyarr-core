## heyarr desired block

Stop a want choosing a release again

### Synopsis

Block a release by hand, so the want will not choose it again.

For a release that is bad in a way nothing caught: a fake (a program named like
an episode), a wrong cut, the wrong language. The candidate is the provider's id
for the release, as the want's candidates list (GET /desired/{id}/candidates)
shows it; for a torrent it is often `infohash:<hash>`. Pass --provider when the
want's last search did not offer that candidate (a fanned-out release), or
offered it from two providers. Pair with `heyarr desired unblock` to lift it.

```
heyarr desired block <id> <candidate> [flags]
```

### Options

```
      --addr string         where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --detail string       why, for whoever reads the block later
      --json                emit machine-readable JSON
      --provider string     the provider whose candidate id this is, when the last search did not offer it or two providers did
      --timeout duration    how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string        bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string   read the bearer token from this file (default: <data_dir>/cli.token when it exists)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr desired](heyarr_desired.md)	 - Say what should exist, whether or not it does yet
