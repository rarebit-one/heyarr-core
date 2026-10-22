## heyarr vault purge

Release every legacy vault placement pin so GC can reclaim it (#823)

### Synopsis

Remove every placement pin on this media node, recording release intent for
each blob nothing else wants (#779, ADR-0114), so a later
"heyarr gc --honour-releases --apply" can reclaim its last copy.

On a media node every placement pin is legacy vault: the media profile mounts no
vault routes and writes none. Since no device can reach those pins any more,
nothing else would ever release them, and GC's last-copy rule would keep the
bytes forever.

Library media is never released. A pin on a blob that an asset or a scanned
library file reaches is removed, and the bytes stay. A blob also taken from a
sibling is released only once that sibling withdraws it.

Like gc, this works on this node's database directly, as host administration,
and changes nothing unless you pass --yes. Run it as the service's user.

After a real run:

  heyarr gc --honour-releases --apply    starts each released blob's grace window
  (wait out the window; the default is a week)
  heyarr gc --honour-releases --apply    reclaims them, with the basis "released"

It refuses on the personal profile, where pins are devices' live retention
decisions.

```
heyarr vault purge [flags]
```

### Options

```
      --json   emit machine-readable JSON
      --yes    remove the pins; without it nothing is changed
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr vault](heyarr_vault.md)	 - Retire the legacy vault data a media node still holds (#823)
