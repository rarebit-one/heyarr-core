## heyarr stores sync

Want every game you own on GOG

### Synopsis

List the games the logged-in GOG account owns and want each one, so the
store fetch backs it up: downloaded by lgogdownloader, verified file by file
against GOG's listing, and ingested as a game (ADR-0120).

Run it as the service user whose lgogdownloader login the GOG download
client uses:

  sudo -u heyarr lgogdownloader --login        # once
  sudo -u heyarr heyarr stores sync gog

Re-running is safe: a game already wanted is reported and left alone.
--dry-run asks the node what it would do with each game and changes nothing,
so already-wanted games and refusals (a title another product holds) show up
before anything is written. Listing
a large library is slow (lgogdownloader asks GOG for every game's details); pass
--from-file with a saved 'lgogdownloader --list json' to sync from that instead.

```
heyarr stores sync gog [flags]
```

### Options

```
      --addr string              where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --dry-run[=true|false]     ask the node what it would do with each game — would want, already wanted or refused — and change nothing
      --from-file string         read a saved 'lgogdownloader --list json' instead of running it
      --json[=true|false]        emit machine-readable JSON
      --quality-profile string   the profile each game is wanted under (defaults to the game default, owned)
      --timeout duration         how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string             bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string        read the bearer token from this file (default: <data_dir>/cli.token when it exists)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr stores](heyarr_stores.md)	 - Back up the games you own on a store
