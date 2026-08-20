# Deployment

Heyarr ships as a single static binary, a hardened systemd unit, and a
distroless container image. Pick one; they install the same thing in the same
places.

- **[`hyperion-1.md`](hyperion-1.md)** — the reference deployment, measured on
  the real host: filesystem layout, the service account/group/ACL model from
  §74, the recorded `systemd-analyze security` score, and what the absence of
  block cloning on that machine actually costs.
- **[`../../deploy/systemd/heyarr.service`](../../deploy/systemd/heyarr.service)** —
  the unit. Read its comments before changing it; two of the directives that
  look missing are absent on purpose.
- **[`../../deploy/docker/Dockerfile`](../../deploy/docker/Dockerfile)** — the
  image. No shell, no package manager, runs as uid 65532.
- **[`toolchain.md`](toolchain.md)** — FFmpeg and ffprobe: optional, pinned by
  digest, and what a node without them can still do (ADR-0023).

## Two invariants worth knowing before you start

**Heyarr expects OS-level containment as well as application-level
capabilities** (§74). Do not run it as root, and do not give it write access to
a library it is only meant to read. The unit does both for you; a hand install
has to do it deliberately.

**It refuses to serve the library unauthenticated on a routable address**
(ADR-0011). This is a refusal to start, not a warning, and it holds inside the
container too — a published port with `http.auth.enabled=false` will not run.
If a deployment appears to hang at startup, read the first error line: it is
usually this, and it is telling you something true.

## Verifying

`scripts/acceptance.sh` — `make demo` — is how you verify a build, including on
the host you just deployed to. See the README's *Verifying a build*, and
`hyperion-1.md` for running it against a packaged binary on a machine with no Go
toolchain.
