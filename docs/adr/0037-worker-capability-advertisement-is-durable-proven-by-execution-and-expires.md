# 0037. Worker capability advertisement is durable, proven by execution, and expires

**Status:** Accepted
**Date:** 2026-08-23

## Context

ADR-0023 established capability routing and it works. The job queue matches a
job's single `required_capability` against the set a worker holds, `""` meaning
"anyone". The worker side is a set; the job side is a scalar. That routing is
sound and this ADR does not touch it.

What did not exist is **any record of which worker can do what**. Capabilities
lived only as an in-memory `Config.Capabilities` on a running `Runtime`, passed
as an argument to `Claim`. No table, no registration, no expiry, no read path. A
worker's abilities were observable only by watching which jobs it took.

ADR-0023 says so in its own consequences:

> `/api/v1/system` describes the node answering the request, not the fleet. […]
> A fleet-wide view needs worker capability advertisement, which is tracked
> separately rather than invented here.

§6 already reserves the destination — **Compute Peer**, "provides
worker/transcode resources but little or no persistent content storage" — and
`peers.mode` has accepted `'compute'` since migration `00002`. The concept was
reserved; nothing was built under it.

This is not an extension of ADR-0023. That ADR is about an optional binary on
ONE node, it has aged well, and blurring it with fleet-wide hardware routing
would spoil a clean decision.

## Decision

**A capability is proven by EXERCISING it, never by parsing a list.**

The motivating measurement, from heterogeneous hardware: `ffmpeg` will happily
**list** a hardware AV1 encoder on silicon that cannot encode AV1, and then fail
at runtime with `No capable devices found`. The same device decodes AV1 without
trouble. Encode and decode support are asymmetric per hardware generation, and
the asymmetry is **not derivable from anything FFmpeg prints** — not
`-encoders`, not `-hwaccels`, not the codec flags.

A node that advertises a capability it cannot deliver is worse than a node that
advertises nothing, because the job routes to it and then fails — after the
queue has already decided this was the right home for the work, and after it has
passed over the node that could have done it.

So a hardware probe is a real execution: encode a handful of frames from a
synthetic source to a null sink with each candidate encoder, and advertise only
what exits successfully. `ffmpeg -encoders` is still consulted, but only to
**skip** candidates the binary does not have. It is a way of doing less work and
never a way of deciding the answer.

**Binary presence stays startup-only. Hardware capability is re-verified on a
beat, and the advertisement NARROWS.**

ADR-0023's stance on the binary is right and unchanged: installing ffmpeg under
a running Heyarr and expecting it to be noticed is not a supported flow, and
polling for it would mean every job carried the possibility of a different
answer than the one the node advertised.

It is wrong for hardware. A device can be claimed by another process, and a
driver can break after a kernel update — neither of which changes the binary or
its path. So the two halves are re-verified on different rules, the `source`
column on every row records which rule applies, and the stored set is made
**equal** to each new advertisement rather than merged into it. An advertisement
that can only ever grow is one that lies after the first driver update.

**An advertisement expires, and expiry is enforced at the READ.**

A worker that dies cannot tidy up after itself: a power cut, an OOM kill and a
severed partition are exactly the deaths that skip a shutdown hook, and they are
the deaths that matter. Every row carries its own `expires_at`, chosen by the
advertiser rather than by the reader, and every read filters on it. A sweep of
expired rows happens on the write path, but that is housekeeping — a stale claim
must not be honoured whether or not anything has got round to deleting it.

**The vocabulary is structured, hierarchical and dotted** — `ffmpeg`,
`ffmpeg.encoder.hevc`, `ffmpeg.encoder.av1.<accel>`. The existing exact-string
match handles this with no schema change, no query change and no migration to
the jobs table. **Exactness is load-bearing**: `ffmpeg` is a prefix of
`ffmpeg.encoder.hevc`, so a `LIKE` or substring comparison anywhere would route
AV1 work to a node that merely has the binary installed.

**The beat belongs to the worker, not to the controller.** Every other periodic
sweep here is enqueued by the controller and claimed by whichever worker is
free, because the answer is about the fleet. This answer is about the machine
the worker is running on. A queued pass would be claimed by whoever was free,
and that worker can only honestly advertise about itself — the busy one would go
unrenewed and expire while perfectly healthy. A per-worker dedupe key does not
fix it, because nothing stops one worker claiming another's job. What keeps this
inside invariant 4 is unchanged: the answer goes into the **database**, not into
memory, and the controller reads rows.

**`GET /api/v1/capabilities` is the fleet read path**, answering "which nodes
hold capability X" with an exact match.

## Consequences

**Two things the scalar job side genuinely cannot express are deliberately out
of scope.** *Conjunction* — "needs A and B" — is encodable as one opaque string,
at the cost of being unable to ask "which nodes have B at all". *Ordering and
preference* belongs to the planning issue. Moving the job side to a set means a
join table or a serialised column plus a rewritten claim query; it buys
conjunction, which nothing has yet asked for. **Revisit when a second real user
of conjunction appears** — recorded as the trigger rather than built
speculatively.

**A false negative is cheap and a false positive is not, and the whole design is
biased accordingly.** A probe that fails for an environmental reason — a VAAPI
render node that is not at the expected path, a device momentarily busy — costs
this node the work. A probe that passes wrongly costs the FLEET the work,
because the queue hands the job over and nobody else looks at it again. This is
why the VAAPI device path is hard-coded to the first render node rather than
being left out: omitting it would make every VAAPI candidate fail forever, a
permanent false negative that looks exactly like "this hardware cannot encode".
Making it configurable is a revisit trigger, not a shipped knob.

**The worker identity is per process.** A restarted worker advertises under a
new id and its predecessor's rows expire rather than being inherited, which is
the honest behaviour: the old process's proof was about a process that no longer
exists. The cost is that the table accumulates one dead advertisement per
restart until a write sweeps it.

**A narrowing emits one event, `system.worker.capabilities_changed`, with the
transition in the payload.** There is deliberately no `capability.gained` and no
`capability.lost`: two types would be two places to forget to emit, and the
interesting half — losing a hardware encoder without the binary changing and
without a restart — is the half that would get forgotten. Nothing is emitted by
a pass that found the world unaltered, which is almost every pass.

## Evidence, and what is NOT evidence

This is stated plainly because the alternative is a reader assuming more was
measured than was.

**Proven, on every machine, with nothing skipped.** The "listed but not capable"
behaviour is asserted against a real subprocess: the test binary re-execs itself
as a fake FFmpeg that lists `av1_qsv` and then exits 1 with `No capable devices
found`, receiving the exact argv the production code builds. There is no `t.Skip`
anywhere in that package and a guard test fails the package if its exec-level
assertions did not actually launch subprocesses — the `ok`-having-run-nothing
failure that cost this repo hours three times in Milestone 4. The narrowing, the
expiry, the exact-match filter and the two-node fleet query are asserted against
a real SQLite database with an injected, movable clock.

**Not proven here.** No test in this change has encoded a single frame on real
silicon. There is no `ffmpeg` on the development machine this was written on and
the macOS runners deliberately carry none (ADR-0023). The fixture reproduces a
measurement taken on heterogeneous hardware elsewhere; it does not re-take it.
The two-node fleet assertion is two peer identities against one database, not
two machines — the second real peer is what would make it mean anything, and
that is this milestone's work, not this issue's.

So: the *mechanism* is exercised, and the *hardware claim it encodes* is
inherited from a measurement this repository cannot currently repeat.

## What would make us revisit

A second real user of conjunction — a job that genuinely needs A *and* B, where
encoding it as one opaque string stops anyone being able to ask about B alone.

A machine whose VAAPI render node is not at the first one, which would turn the
hard-coded device path from a conservative default into a bug.

Hardware on which a probe passes and the real work then fails anyway — which
would mean eight frames of a synthetic source is not a sufficient exercise, and
the probe has to look more like the job.

A capability whose exercise is expensive enough that a five-minute beat is not
affordable, at which point re-verification needs to become adaptive rather than
periodic.
