# Deployment

Heyarr is pre-alpha and not yet deployable. This directory will carry:

- `hyperion-1.md` — the reference two-site deployment: ZFS dataset layout (the
  CAS and the controller database want very different `recordsize`), block
  cloning prerequisites for reflink ingest (ADR-0014), and the service account,
  group and POSIX ACL model from spec §74.
- `systemd.md` — running the roles as hardened units.
- `docker.md` — the container image.

The invariant worth knowing in advance: Heyarr expects OS-level containment as
well as application-level capabilities (§74). Do not run it as root, and do not
give it write access to a library it is only meant to read.
