# 0072. Opt-in native TLS on the client API, and an explicit public origin

**Status:** Accepted (2026-09-03)
**Date:** 2026-09-03

## Context

The client-facing HTTP API (`http.addr`, default `127.0.0.1:7777`) serves plain
HTTP. The only TLS in the process is the peer fabric's mutually-authenticated
mTLS surface (ADR-0012), whose certificate is derived from the node's Ed25519
identity and never written down — a closed system between members, not a server
a browser or a television talks to.

A first-party client — a Voidbind web/TV login (ADR-0053), a browser hitting the
resource API — needs `https://<hostname>`. Two things are missing:

1. **No way to serve the client API over TLS.** An operator's only options were
   to put a reverse proxy in front (another moving part, and one that terminates
   TLS somewhere the node cannot see) or serve plaintext.
2. **No way to state the external origin.** `renderBaseURL` derives the origin a
   renderer fetches from, and the Voidbind login uses the same value as its rp
   origin. It is derived from `http.addr` — an `IP:port` — which is exactly not
   the `https://heyarr.example.com` hostname a login needs. A login whose rp
   origin is an IP does not match the hostname in the browser and fails.

allthing hit the same wall and shipped `ALLTHING_TLS_CERT_FILE` /
`ALLTHING_TLS_KEY_FILE` and `ALLTHING_PUBLIC_ORIGIN` (allthing ADR-0013). This
is the same shape in heyarr's config idiom.

## Decision

**TLS is opt-in, under the http listener, and all-or-nothing.**

```yaml
http:
  addr: "0.0.0.0:7777"
  public_origin: "https://heyarr.example.com"
  tls:
    cert_file: /etc/heyarr/tls/heyarr.crt
    key_file:  /etc/heyarr/tls/heyarr.key
```

- **Both `cert_file` and `key_file` set → the TCP listener is served with
  `ServeTLS`.** The listener bytes are the same; only the wrapping changes.
- **Neither set → plain HTTP, exactly as before.** This is the default, and the
  zero config is unchanged: a laptop `heyarr all` needs no certificate.
- **Exactly one set → the process refuses to start.** A cert with no key, or a
  key with no cert, is almost always a half-finished change, and the failure it
  otherwise produces — a listener the operator believes is encrypted, quietly
  serving plaintext, or `ServeTLS` erroring deep in startup — is worse than a
  named config error at the top. There is **no silent fallback to plaintext**.

**The unix socket is never wrapped.** It is the local IPC transport the CLI and
the workers dial (ADR-0002's roles talk over it); it never leaves the host, and
TLS on it would break every in-process caller for nothing. Only the TCP listener
— the one that faces the network — takes the certificate.

**An explicit `http.public_origin` names the external origin.** When set, it is
what `renderBaseURL` returns, ahead of both the peer endpoint and the derived
listener address, so the Voidbind login rp origin and the rendered base URL both
become the `https://` hostname a client actually reaches. It is validated at
config load as an absolute `http(s)` origin with a host; unset keeps today's
derived behaviour exactly. It is separate from `tls:` on purpose — TLS can be
terminated at the node (with these files) or at a reverse proxy in front (no
files here), and either way the origin the browser typed is a fact the listener
address cannot supply.

**The certificate is issued out of process.** For the homelab target that
prompted this — an internal name like `heyarr.<site>.example` with no public
A record — the certificate is a Let's Encrypt cert obtained over **DNS-01** by a
systemd + `lego` timer, renewed on the same timer. **The heyarr process only
READS the two files.** A renewal is picked up on the next restart or a `SIGHUP`;
`ServeTLS` re-reads the files each time it is called, so nothing caches a stale
certificate across a restart. Heyarr issues nothing and renews nothing — that is
deliberately not the server's job.

**Graceful shutdown is identical on both paths.** `ServeTLS` and `Serve` share
one `http.Server`, so `Shutdown` drains in-flight requests the same way whether
or not TLS is on — including a range response halfway through a large blob.

## Consequences

- A node can serve `https://heyarr.example.com` natively, with no reverse proxy,
  and a Voidbind login against it has a correct rp origin. The default and every
  existing deployment are byte-for-byte unchanged: no files set, plain HTTP.
- A half-configured TLS pair is a startup error, not a running server that is not
  what the operator thinks it is.
- The renewal story stays out of the process. **Revisit if** in-process ACME ever
  earns its keep — it would mean the server holding an account key and a renewal
  loop, which is a larger trust and failure surface than reading two files a
  timer maintains, and the split here is the cheaper side of that trade for a
  homelab.
- `public_origin` and `tls:` are independent knobs. A deployment that terminates
  TLS at a proxy sets `public_origin` and no `tls:`; one that terminates at the
  node sets both. **Revisit if** a third party needs to distinguish "the origin
  clients use" from "the origin renderers use" — today they are the same value,
  and splitting them before there is a caller would be speculative.

---

*Provenance: mirrors allthing ADR-0013 in heyarr's config idiom, to serve an
internal homelab name over HTTPS (e.g. `https://heyarr.<site>.example`).*
