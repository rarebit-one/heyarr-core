# Security policy

## Reporting a vulnerability

Please report security issues privately through
[GitHub Security Advisories](https://github.com/rarebit-one/heyarr-core/security/advisories/new)
rather than a public issue. We will acknowledge within 7 days.

Heyarr is pre-alpha and has had no security review. Do not expose it to an
untrusted network.

## Threat model

Heyarr's design makes a few security commitments worth stating explicitly,
because they shape what counts as a vulnerability:

- **Encrypted personal state is opaque to the server.** Heyarr infrastructure
  replicates user state without decrypting it (spec §38). A peer sees space
  IDs, opaque change IDs, causal metadata and ciphertext — never playlist names,
  ratings, annotations or history contents. Any path by which a peer, an
  operator, or a controller-side MCP agent can read that plaintext is a
  vulnerability, not a feature.
- **Full Peers store wrapped keys they cannot unwrap** (spec §41).
- **Blob bytes are verified by the destination, always** (spec §21). A transfer
  that trusts a source's claimed hash is a bug.
- **Heyarr must be safe over a hostile network.** Peer-to-peer authentication is
  Heyarr's own responsibility; it does not assume a VPN or site-to-site tunnel is
  the security boundary.
- **Application-level capabilities are necessary but not sufficient.** Heyarr
  expects OS-level containment too — service accounts, groups, POSIX ACLs,
  restricted mounts, cgroups (spec §74).

## Not in scope

Heyarr does not police what you acquire, nor how. Reports about content sources
are out of scope.
