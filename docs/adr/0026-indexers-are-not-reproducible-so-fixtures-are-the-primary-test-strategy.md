# 0026. Indexers are not reproducible, so fixtures are the primary test strategy

**Status:** Accepted
**Date:** 2026-08-21

## Context

Heyarr depends on two external services in Milestone 3: an indexer for discovery
(§59) and a download client for transfer (§58). Neither is present on the
development machines or on the CI runners.

The obvious move is to reach for ADR-0023's answer, which solved the same-shaped
problem for FFmpeg by pinning a version and installing it from a checksummed
archive. Applied here that would mean pinned container images and a lane that
starts both services before running the suite.

Two things make that the wrong answer, and they are different for each service.

## Decision

**Neither service is ours to install, so neither is pinned.**

ADR-0023 pinned FFmpeg by digest **because Heyarr installs FFmpeg** —
`scripts/toolchain.sh` downloads it, `scripts/toolchain.lock` records the
SHA-256, and the verification happens before it runs. That is a supply-chain
decision about something we put on the machine.

An indexer and a download client are **operator-managed services that Heyarr
targets by configuration** (§58: "external clients perform protocol-specific
transfer mechanics"). Heyarr does not install them, does not supervise them and
does not have an opinion about how they got there. There is no supply chain to
secure, so there is nothing to pin.

An earlier draft of this decision proposed a digest-pinned container behind
`make integration`. It carried the FFmpeg analogy one step too far: it would
have bought a single smoke test in exchange for two container runtimes to
resolve, image digests to keep current, and a lane living off the merge path —
and a gate nobody runs is a gate people stop running, which
`scripts/acceptance.sh` already says about itself.

**What replaces pinning is compatibility, checked at connect.** Not controlling
the version does not mean ignoring it. A provider reads the service's reported
API version when it connects, records it in the registry's health, and reports a
version outside the supported range as **unhealthy naming both numbers** — never
as a startup failure. That is the real analogue of ADR-0023's `-version` probe,
and it is what makes "acquisitions stopped after I upgraded the service" one
request away from an answer.

**An indexer is not merely absent — it is not reproducible.**

This is the asymmetry that decides the test strategy, and the two services sit
on opposite sides of it:

| | indexer | download client |
|---|---|---|
| absent from dev and CI | yes | yes |
| reproducible at all | **no** — it proxies third-party services with real credentials | yes — self-contained given a local transfer |
| who runs it | the operator, out of band | the operator, out of band |
| merge-path testing | recorded fixtures + fake provider | recorded fixtures + fake download client |
| live exercise | out of band, by hand, against a real instance | opt-in test pointed at any running instance |

An indexer cannot be in CI at any price. It is a proxy to services that are not
ours, reached with credentials that are not ours, returning results that change
without us. So **recorded fixtures and an in-process fake are not a convenience
to add later — they are the primary and only test strategy**, and the only live
exercise is somebody running it by hand against their own instance.

A download client is merely absent, which is a much weaker constraint: given a
locally-generated transfer it is deterministic. It still gets fixtures on the
merge path, and its live exercise is **an opt-in test pointed at whatever
instance you have** — supplied by environment variable, skipped when unset. No
infrastructure to own, and it becomes a one-command verification the moment a
real instance exists.

**This constrains the interface, not just the tests.** A fixture replayer, an
in-process fake and a real client must be **indistinguishable to every caller**,
which is only possible if the provider interface is defined in **values**: a
query value in, `[]acquisition.ReleaseCandidate` out. The registry never hands
out an `*http.Client`, a base URL or anything else transport-shaped. Credentials,
retries, timeouts and rate limits live behind the interface.

Get that wrong and every test downstream inherits a suite that cannot run — so
it is asserted rather than intended: `internal/providers` has a test that parses
its own source and fails if anything in the package imports `net`, `net/http`,
`net/http/httptest` or `crypto/tls`.

**Each PR records whether the live exercise ran, and against what version —
including "not run".** A test that might not have run is worth nothing unless
somebody says which it was.

## Consequences

**The fixture corpus carries the burden the network usually carries.** Every
response shape a client handles must exist as a capture from a real instance,
with its provenance recorded. A branch of a parser with no fixture behind it has
never seen reality and should be either fixtured or deleted. M3-08 owns the
corpus and its capture procedure.

**Redaction at capture time is a correctness requirement, not tidiness.** Real
captures contain real API keys and real URLs, and this repository is public with
a permanent git history. The corpus needs a scanner that fails CI on anything
credential-shaped, and that scanner needs its own teeth tested.

**The fake is production code, not a test fixture.** `providers.Fake` lives in
the package rather than in a `_test.go` file, because the acceptance demo needs
it: the milestone's central claim is that Heyarr decides what should exist and
explains why *without a real indexer being present*, and demonstrating that on a
real machine over a real socket requires something that behaves exactly like a
provider and needs no service. Keeping it in the package means the demo
exercises the same registration, routing and health paths as production, rather
than a parallel set that agrees only until one of them changes.

**We will be less confident about the indexer client than about anything else in
the codebase, and that should be said plainly rather than papered over.** The
honest position is that its parsing is well tested against recorded reality and
its behaviour against live reality is tested by whoever runs it. Claiming more
would be the "measurement without its conditions" failure this project has
already recorded twice.

## What would make us revisit

An indexer that ships a deterministic offline mode — one that answers from a
local corpus with no third-party dependency. That would make it reproducible and
move it to the download client's column.

Evidence that the recorded corpus has drifted from what a current service
actually returns, which would mean the capture procedure needs to run on a
schedule rather than when somebody remembers.

A second download client, at which point "an opt-in test pointed at whatever you
have" needs to say *which* client it was pointed at, and the environment
variable becomes a small matrix.
