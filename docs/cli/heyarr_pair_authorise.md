## heyarr pair authorise

Old device: authorise a new device and sign its enrolment cert

### Synopsis

Run this on an already-enrolled device. It contributes your USER identity
public key to the handshake, derives the short code, and — once you confirm the
new device shows the same code — signs an enrolment cert for the new device's
key. Signing needs your user identity private key, so run it where that identity
lives.

```
heyarr pair authorise [flags]
```

### Options

```
      --confirm-sas string    proceed only if the derived code equals this value — the scripted stand-in for a human comparison
      --identity-dir string   where your user identity lives (default: your config directory; HEYARR_IDENTITY_DIR overrides)
      --lifetime duration     how long the signed cert is valid (default: the 90-day enrolment lifetime)
      --poll duration         how often to re-check the relay for the next handshake step (default 150ms)
      --relay string          the running Heyarr's relay: a unix socket path, unix:///path, http://host:port or host:port
      --session string        the rendezvous session id both devices share (authorise generates one if empty)
      --timeout duration      how long to wait for the whole handshake before giving up (default 2m0s)
      --yes                   assume the codes matched, without prompting (use only when you compared them another way)
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr pair](heyarr_pair.md)	 - Authorise a new device from an already-enrolled one (§40, ADR-0022)
