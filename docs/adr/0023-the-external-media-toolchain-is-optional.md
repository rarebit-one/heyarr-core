# 0023. The external media toolchain is optional, capability-routed and pinned

**Status:** Accepted
**Date:** 2026-08-20

## Context

Spec §10 delegates audiovisual processing to FFmpeg, and §29 has workers probe
media with ffprobe. Milestone 2 is the first time Heyarr depends on a binary it
does not ship.

There is no `ffmpeg` or `ffprobe` on the development machines, on the CI
runners, or on `hyperion-1` — all three checked. `hyperion-1` additionally has
no Go toolchain; Milestone 1 shipped there by cross-compiling and copying a
static binary onto it.

So the question is not "which version do we require" but whether requiring one
at all is compatible with what Heyarr is: a single static Go binary someone
copies onto a NAS. Making a 45 MB C toolchain a precondition turns "run Heyarr"
into "run Heyarr, and first solve packaging" — on exactly the machines least
able to do that.

## Decision

**The toolchain is optional at runtime.** A node resolves `ffprobe` and
`ffmpeg` at startup. A node that finds neither starts, scans, ingests,
catalogues, verifies, garbage-collects and serves byte ranges exactly as
before.

**Availability is expressed as a job capability, not a startup requirement.** A
worker advertises `ffprobe` and/or `ffmpeg` only if it resolved them. Jobs that
need one declare `RequiredCapability`, so on a node without it they stay
**pending and visible** rather than failing into a retry backoff. This reuses
capability routing, which has existed since M1-05 and M1-09 and until now had
no user at all: `internal/worker/worker.go` built its runtime with an empty
capability set.

**A configured path and a discovered one are not symmetrical.** A path given in
configuration that does not resolve, or that resolves to something not
answering `-version` like FFmpeg, is a startup error. A binary merely absent
from `PATH` is not. Someone naming a specific binary and silently getting none
is worse than not starting; nobody asked for the `PATH` copy.

**Installation is pinned by digest.** `scripts/toolchain.lock` records a
version, a SHA-256 and a URL per platform; `scripts/toolchain.sh` verifies
before installing. Not a distro package, which drifts. Not a marketplace
Action, which is another supply-chain dependency in a public AGPL repository
and offers nothing for a deployment host with no CI.

**`GET /api/v1/system` reports what this node resolved**, so a degraded node
can say it is degraded.

## Consequences

`heyarr all` on a machine with no FFmpeg is a supported, tested configuration
rather than an accident. §29 already made whole-blob materialisation the
fallback to Range probing; "no probing on this node" is a further rung on the
same ladder, not a new kind of decision.

**The pinned versions differ per platform, and macOS CI is deliberately left
bare.** No static-build publisher offers one FFmpeg version across both Linux
and macOS/arm64 — the upstream release tagged `b6.1.1` ships 7.0.2 for Linux
and 6.0 for darwin/arm64, which was discovered while pinning it. Rather than
claim a uniformity that does not exist, Linux is the canonical toolchain
platform: golden probe output is generated and asserted there, and the macOS
runners run *without* a toolchain. That turns a platform gap into coverage —
the claim "a Heyarr with no ffprobe still works" is exercised on a real machine
on every build, instead of only in a unit test. A developer on macOS may see
probe output that differs from CI's; when it does, CI is right.

**`/api/v1/system` describes the node answering the request, not the fleet.**
The toolchain that decides whether a probe runs is the one on the *worker* that
claims the job, and in a split-process deployment that is another machine. The
peer model has a mode per peer, not a capability list, so there is nowhere to
read a fleet-wide answer from yet. Under `heyarr all` the report is complete;
split across hosts it is one datum and `/api/v1/jobs` showing pending probes is
the other. A fleet-wide view needs worker capability advertisement, which is
tracked separately rather than invented here.

**Two mechanisms hold the degrade path up, and only one of them is
load-bearing.** A worker that resolved no toolchain does not *register* the
probe and remux handlers at all, and separately the registrations declare a
`RequiredCapability`. The registration guard is what actually holds: sabotaging
it makes a bare worker fail to start, which the acceptance demo catches.
Sabotaging the registration's `RequiredCapability` changes nothing observable,
because a handler is never registered without its capability — both derive from
the same `Available` flag.

The capability on the registration is therefore defence in depth that cannot
currently fire, kept because a registered type that declares what it needs is
self-documenting and correct if the two ever diverge. What genuinely routes
work is the `RequiredCapability` on the *job*, which the queue filters on
(M2-04). This is written down because the discovery cost two rounds of
sabotage, and the second one is the sort of thing a reader would otherwise
assume was tested.

**A job that no worker can claim waits forever, silently, by design.** That is
the intended degradation and it is also a way to be confused for hours. The
mitigation is that the node says so on `/api/v1/system` and the job is visible
on `/api/v1/jobs` — not a timeout, because failing a job whose handler simply
is not deployed yet would lose work that a later `apt install` would have
completed.

## Evidence

A mixed fleet is exercised on every build: the acceptance demo starts a second
worker against the same database with a scrubbed `PATH`, and asserts it becomes
ready, advertises nothing, registers no media handler, claims no probe job it
cannot run — and that the capable worker alongside it keeps probing. Before
that existed, a fleet with one incapable member was the one configuration this
ADR describes and nothing tested.

There is also a CI job that runs the whole demo on Linux with no toolchain,
alongside the Linux job that has one. macOS already ran bare, but that
confounded two variables: a failure there could have been the missing FFmpeg or
it could have been macOS. The degraded job changes exactly one thing.

## What would make us revisit

A publisher offering matched static builds across Linux and darwin/arm64, which
would remove the version skew and let macOS CI carry the toolchain too.

Evidence that ffprobe's JSON output differs across the pinned versions in a way
the tests can see — at which point the skew stops being a documented asymmetry
and becomes a correctness problem.

A capability that turns out to need finer grain than a binary's presence — a
specific encoder, or hardware acceleration — which would make a boolean per
tool the wrong shape.
