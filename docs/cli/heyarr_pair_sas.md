## heyarr pair sas

Compute the short authentication string for two keys and a salt

### Synopsis

Derive the short authentication string (SAS) that binds two public keys and
a session salt — the same primitive the handshake compares. It is a utility for
scripts and for demonstrating that SUBSTITUTING a key changes the code: run it
with an honest responder key and again with a different one, and the two codes
differ, which is exactly why a man-in-the-middle is caught.

```
heyarr pair sas [flags]
```

### Options

```
      --initiator string   the initiator (user identity) public key, ed25519:<hex>
      --responder string   the responder (device) public key, ed25519:<hex>
      --salt string        the session salt, hex-encoded
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr pair](heyarr_pair.md)	 - Authorise a new device from an already-enrolled one (§40, ADR-0022)
