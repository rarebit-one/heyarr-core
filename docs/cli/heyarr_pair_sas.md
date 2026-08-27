## heyarr pair sas

Compute the short authentication string for two keys and a salt

### Synopsis

Derive the short authentication string (SAS) that binds two public keys and
a session salt — the same primitive the handshake compares. It is a utility for
scripts and for demonstrating that SUBSTITUTING a key changes the code: run it
with an honest responder key and again with a different one, and the two codes
differ, which is exactly why a man-in-the-middle is caught.

The v2 SAS also binds each device's X25519 ENCRYPTION key (§41, ADR-0049): pass
--responder-enc (and --initiator-enc) and substituting only the encryption key
changes the code too, so a relay that swaps the wrap-target key is caught.

```
heyarr pair sas [flags]
```

### Options

```
      --initiator string       the initiator (user identity) public key, ed25519:<hex>
      --initiator-enc string   the initiator's X25519 encryption key, x25519:<hex> (optional)
      --responder string       the responder (device) public key, ed25519:<hex>
      --responder-enc string   the responder's X25519 encryption key, x25519:<hex> (optional)
      --salt string            the session salt, hex-encoded
```

### Options inherited from parent commands

```
  -c, --config string   path to the configuration file (default: built-in defaults plus HEYARR_ environment)
```

### See also

* [heyarr pair](heyarr_pair.md)	 - Authorise a new device from an already-enrolled one (§40, ADR-0022)
