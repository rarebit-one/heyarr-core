# 0048. A cross-site grant is a signed, expiring delegation verified against a pinned key

**Status:** Proposed
**Date:** 2026-08-26

## Context

Milestone 8 moves identity from a server-issued bearer token (ADR-0011) to
device-held keys: one user identity authorising many device keys across many
peers (§40). Nothing in this record ships behaviour. It answers the one question
ADR-0038 could not, and everything M8 builds below it — delegation, capability
grants, pairing, recovery, and the cross-site cached leases of #285 — assumes the
answer. M7's decision issue (#281 → ADR-0044) worked the same way: decide the
format once, in writing, because it is cheap now and expensive to reverse.

Three facts frame the whole decision.

**ADR-0038 removed the authority you would otherwise ask.** Each peer is
authoritative for its own site and does not mind when sync fails — "a peer that
has not heard from another in a week is working correctly." That makes divergent
*desired state* fine, because two policies are two correct answers. It is exactly
what breaks for grants: a *user* is not a site, and a capability revoked at one
site and honoured at another is **a security surprise, not a policy difference**.
ADR-0038 named this the one thing git never had to solve and handed it here.

**ADR-0040 already built the shape, node-locally.** A renderer fetches bytes with
a **capability** — `v1.<blob>.<expiry>.<mime>` under an **HMAC** — that "carries
no identity and grants exactly one thing: these bytes, until this instant." It is
stateless, expiring, and *valid only at the peer that minted it*, because an HMAC
secret is symmetric and lives on one node. ADR-0040 said in as many words what M8
adds: "principals, revocation and delegation." The one property it cannot extend
is the one M8 needs — **cross-site verification** — and the reason is the
symmetric key.

**ADR-0032 fixed the ordering.** Device keys land before they authorise anything;
a key "issued and immediately trusted is a key nobody had to enrol." The device
CLI already exists (`internal/device`, #159), generating a client-side Ed25519
keypair labelled `not_enrolled` and `unproven` on the wire, "because someone will
trust it" otherwise. This ADR is what gives that key something to authorise, and
ADR-0032's revisit clause obliges the labels to come off **in the same change**
that makes them false.

The acceptance sentence this milestone owes is two halves, and each is a trap:

> A device proves it is a user's, on either peer, without a server issuing it
> anything — and a capability granted to that user is honoured until it expires,
> whether or not the two peers can reach each other.

A device identity that needs a live server to vouch for it has not left the token
model. A grant that needs both peers reachable to agree on revocation is not a
peer-repo grant.

## Decision

**A cross-site grant is a signed, expiring delegation. It binds a principal, a
resource, a capability set and an expiry; it is signed with an Ed25519 key; and a
peer honours it iff that key is already pinned in the peer's membership, the four
bindings match the request, and the peer's own clock says it has not expired.**

Three layers, one verification rule.

### The three keys, and why authentication and authorisation are separated

| Key | Held by | Lifetime | Authorises |
|---|---|---|---|
| **User identity** (Ed25519) | the user (recovery secret restores it, ADR-0022) | account-lifetime | it is the root of authority; its *public* half is pinned into each peer's membership |
| **Device key** (Ed25519) | one device, client-side (ADR-0032) | device-lifetime | **nothing on its own** |
| **Capability grant** (signed record) | issued, cached, bearer-presented | short — the number below | one principal, one resource, one capability set, until one instant |

The load-bearing separation is between the middle row and the bottom row.

- **A device key *authenticates*** — it proves "I am this device, speaking for
  user U" — by presenting an **enrolment certificate** the user identity signed:
  *"device key D belongs to user U."* That is the delegation of §40, "one user
  identity authorising many device keys," and it is signed by the **user**, not by
  a server — which is the acceptance sentence's first half, exactly. It works on
  *either* Full Peer because both pin U's public key in membership. It authorises
  nothing.
- **A capability grant *authorises*** — it proves "principal U may `read`
  resource R until instant T." This is the short-lived, self-limiting thing, and
  it is the whole of the answer to the hard question.

Folding authentication into authorisation is the easy failure. It produces a
long-lived credential that both says who you are and opens the library, so a lost
phone is a lost library until someone reaches every site. Keeping them apart means
a lost phone can still *say* it is user U (its enrolment cert is long-lived and
authorises nothing) but can only *read* what it already holds an unexpired grant
for — and those age out. §54 states this separation outright: "long-lived device
identity plus short/medium-lived cached grants."

### 🔴 A grant is a signed expiring delegation, not a per-site fact

This is the question ADR-0038 posed. A **per-site fact** would mean: to grant
access, the granting site records a row, and to be honoured elsewhere the row must
have *propagated* to the other site, and revocation is deleting the row locally.
It is rejected, on the acceptance sentence's second half.

Under ADR-0038 there is no reachability guarantee — a week of silence is Tuesday.
A per-site fact honoured at peer C is a row C is holding, and C revokes it only
when C independently *hears* it was revoked, which C may never do. There is no
self-limiting bound: the stale row is honoured **forever**. That is precisely the
security surprise — authorisation that outlives its authority with no ceiling.

A **signed, expiring delegation** puts the bound *inside the grant*. The grant
carries its own death, so staleness is self-limiting **without anyone being
reachable**: revocation is "stop re-issuing, and wait out the window," which needs
no peer to be up and no authority to be asked. This is the answer ADR-0038
predicted ("signed, expiring grants… make staleness self-limiting without
requiring anyone to be reachable") and the shape §54 and ADR-0040 already point
at.

**What the per-site model would have bought, and how we keep most of it.** Its one
real advantage is *instant* revocation with no clock dependence — delete the row,
done, here. We keep that as an *addition*, not the guarantee: a peer may hold a
**local denylist** of grant ids or principals it refuses immediately. But a
denylist binds only where it is applied and does not cross a partition, so it is a
latency improvement on the reachable path, never the safety property. The safety
property is the expiry, because it is the only one that holds when no one is
reachable.

### 🔴 Ed25519, not HMAC — because the verifier does not hold the issuer's secret

ADR-0040's capability is symmetric (HMAC-SHA256), which is why it is "only valid
at the peer that minted it." A cross-site grant is verified at a peer that is *not*
the issuer and *cannot reach* the issuer, so the verifier cannot share a secret
with the signer. It must verify with a **public** key it already holds.

So a grant is **Ed25519-signed**, and the verifying peer holds the issuer's public
key because it **pinned it at enrolment** — the same trust root ADR-0012
established for peers (pinned self-signed keys, "membership is the only trust
root"), extended to pin *user* identities too. Verification is offline by
construction: it needs a public key in hand and a clock, never a live reach. This
is the one structural break from ADR-0040, and it is deliberate — ADR-0040's HMAC
capability **stays** for what it is good at (per-blob, node-local, handed to a
television that has no identity); M8 grants are the identity-carrying, cross-site,
expiry-revocable layer above it. Two mechanisms, two trust roots, as
`internal/api/blobs` already frames it.

**Two issuers, one verify path.** A grant is signed either by

- **the user identity**, delegating to its own principals (a user authorising its
  devices to read its own library) — the self-sovereign M8 core; or
- **a peer**, issuing a lease for a read of its own site's holdings to a principal
  it recognises, cached to sibling peers *ahead* of an outage (#285, §54) — "the
  signature of the peer that issued it."

Both are the same signed structure and the same verification; the only difference
is *which pinned key* signed, and both keys are pinned through membership before
they are ever needed. A grant fetched during an outage is a grant that does not
exist, so caching is on the ordinary peer surface alongside §50's backup cycle.

### 🔴 The revocation window is 24 hours

The number this milestone owes: **how long a grant is honoured after it is revoked
elsewhere.** The answer is its remaining lifetime, and the cap on that is the
grant TTL.

**Capability-grant TTL = 24 hours, maximum.** So a capability revoked at site A is
honoured at an unreachable site B for **at most 24 hours** — the worst case, when
the two cannot reach each other for the whole window. It is chosen against two
pressures pulling opposite ways:

- **Long enough** that a grant *cached ahead of an outage* outlives a plausible
  outage. A partition of hours, even most of a day, must not lock a user out of
  a library that is sitting on the peer in front of them. §53's degraded-read
  mode has no authorisation story without this.
- **Short enough** that a revoked grant honoured for its remainder is tolerable.
  A day is the outer edge of that. A week — which "does not mind when sync fails"
  otherwise permits — is not.

When the peers **can** reach each other, effective revocation is far faster than
24 hours, because grants are **re-issued on a short cadence** (minutes) the way
ADR-0039 re-proves worker capability: a device online continuously carries a fresh
short grant, and the moment issuance stops the next renewal simply does not come.
The 24-hour cap is the *partition* bound, not the steady-state latency. Revocation
that "takes effect when the peers are back" (#285) is therefore the honest
statement: a grant revoked during a partition is honoured until it expires, and
that is a stated consequence, not a bug — 24 hours is how short the expiry has to
be for it to be acceptable.

**The device enrolment certificate is long-lived and this is safe**, because it
authorises nothing (above). A stale enrolment cert lets a device keep *claiming*
to be user U; it opens no resource without a separate, short grant. So the 24-hour
window is the only revocation window the milestone owes as a number; the
enrolment cert's lifetime is an authentication-freshness question, not an
authorisation-exposure one, and a lost device is contained by (a) the user
ceasing to issue it grants and (b) the ≤24-hour ageing-out of what it holds, with
re-wrapping (ADR-0022, M9) as the forward-looking measure.

### 🔴 Clock skew fails toward refusal

A grant refused by a peer's own clock makes skew a **security parameter**. State
the direction plainly: **skew must fail toward refusing a valid grant (safe,
annoying), never toward honouring an expired one (unsafe).**

Expiry is evaluated strictly on the honouring peer's **own** clock, with **no
grace period** — `now >= expiry` refuses, exactly as ADR-0040 already does
(`!now.Before(c.ExpiresAt)`). A grace period would honour expired grants and is
the unsafe direction; there is none.

The residual hazard is a peer whose clock is **behind** true time: it believes
less time has passed and so *over-honours* — the unsafe direction. We cannot
detect our own skew offline, so we do not pretend to. Instead:

1. Any skew allowance is applied to **shorten** the honoured window, never extend
   it — the verifier subtracts a fixed margin (**5 minutes**) so a peer within the
   margin of correct refuses *slightly early*. The margin can only make refusal
   earlier, never expiry later.
2. `not_before` is not used to gate the common case — grants are valid from
   issue — so the reverse skew failure (a peer ahead honouring a not-yet-valid
   grant) has no window to open; `not_yet_valid` remains a refusal reason for
   defence in depth.
3. The **TTL, not the clock, is the guarantee.** A peer whose clock is wrong by
   more than the margin in the unsafe direction over-honours by exactly that
   excess — bounded, and small against a 24-hour TTL on NTP-synced homelab hosts.
   The design never rests on clocks agreeing; it rests on the window being short.

So: within the margin, skew fails safe by refusing early; beyond it, the damage is
bounded by the TTL rather than unbounded. That is the strongest offline statement
available, and it is why the TTL is short.

### What binds a grant, and the proof each of the four constrains

A grant binds **principal, resource, capabilities, expiry** (§54), and every one
is **inside the signed payload**, the way ADR-0040 signs all four of its fields so
"a token pointed at a different blob, given a longer life or relabelled… fails to
verify." A grant is therefore not a bearer token with extra fields nobody checks —
the easy failure ADR-0038 and #285 both warn against. Each binding is proven to
constrain by a test whose *refusal reason* is asserted by name (`assert_eq`, never
`assert_contains` — the enum values share words: `expired`, `not_yet_valid`,
`principal_mismatch`, `resource_mismatch`, `capability_denied`, `unknown_issuer`):

- **principal** — a grant for user U, presented by a device of user V, is refused
  `principal_mismatch`.
- **resource** — a grant for resource R does not open resource R′
  (`resource_mismatch`).
- **capabilities** — a `read` grant does not permit a write, asserted against one
  of §53's "No" operations (`capability_denied`).
- **expiry** — an expired grant is refused by the honouring peer's own clock with
  the controller/issuer unreachable and an injected clock, not a sleep (ADR-0017).

And the enrolment gate makes an un-enrolled issuer unspellable: a grant signed by
a user or peer key **not pinned** at the honouring peer is refused `unknown_issuer`
— which is ADR-0012 already working, and the mechanism by which "a key issued and
immediately honoured" (ADR-0032) cannot be written down. Keys are pinned
out-of-band at enrolment (pairing by short authentication string, ADR-0022) before
any grant naming them is honoured.

## Consequences — the assertions this milestone now owes

This ADR produces no code; it produces the list of assertions the rest of M8 must
make provable in `make demo` (#187: an assertion whose subject is absent is not
coverage), and the decomposition issues each carry "what would make it provable."

- **Authentication without a server** → a device authenticates as user U against
  **both** peers presenting only its device key and a **user-signed** enrolment
  cert; no bearer token is issued, and the `not_enrolled`/`unproven` labels come
  off in that change (ADR-0032).
- **Honoured across a partition** → a cached grant permits a read with the issuer
  unreachable, and the same read **without** a grant is refused — the second half
  is what makes the first mean anything (#285).
- **Self-limiting revocation** → a grant revoked at one site is still honoured at
  an unreachable site, and **stops being honoured within 24 hours**, asserted with
  an injected clock; the refusal names `expired`.
- **Each of the four binds** → the four `assert_eq` refusals above, plus
  `unknown_issuer` for an un-enrolled signer.
- **Skew is safe** → a peer whose clock is ahead refuses a still-valid grant
  (safe); the verifier never honours a grant past `expiry` on its own clock.
- **Every transition emits an event** (invariant 7) → a grant issued, cached,
  honoured, refused.

The two numbers the milestone owes, stated: **revocation window = 24 hours**
(the partition worst case; steady-state revocation is a re-issue cadence of
minutes), **skew fails toward refusal** (strict local-clock expiry, no grace, a
5-minute margin that only shortens the window).

## Relationship to existing records

- **ADR-0011** (bearer tokens) is additive scaffolding, not preserved as the
  identity model. Bearer tokens remain for loopback and service principals; for
  *user* principals, device identity plus grants supersede them.
- **ADR-0040** (HMAC render capability) is unchanged and retained. M8 does not
  replace it; the two coexist as separate trust roots, symmetric-node-local vs.
  asymmetric-cross-site.
- **ADR-0022** (enrolment and recovery) stays `Proposed`. This ADR builds on its
  *enrolment-is-pairing* and *recovery-secret* decisions and depends on them; it
  moves to `Accepted` when the pairing and recovery **behaviour** ships (its
  deliverables), the way ADR-0038 was accepted on evidence rather than argument —
  not by a document that ships nothing asserting it.
- **ADR-0032** (device keys land before they authorise) is the ordering this ADR
  honours; its revisit clause fires here.

## What would make us revisit this

- **Cross-user sharing (M9).** This record decides a user delegating to its **own**
  devices, and a peer leasing its **own** holdings. §41/§47's shared spaces —
  "wrapped for User A and User B" — add one user granting *another* user access,
  which is a grant whose issuer is not the recipient's own identity. The verify
  path here already accommodates it (any pinned issuer key), but the *policy* of
  who may issue a grant over whose resource is M9's, and may want a capability that
  itself carries the right to delegate. That is a new record if it does.
- **The 24-hour window proving wrong in practice.** If real partitions routinely
  outlast a day, the answer is a longer TTL *with the revocation cost stated
  louder*, not an unbounded grant — the moment a grant has no expiry, ADR-0038's
  security surprise is back.
- **A hardware root of trust for the user key.** If the user identity key can live
  in a TPM/passkey/1Password-style store, enrolment and recovery change shape and
  ADR-0022's recovery-secret mechanism becomes one option among several.
- **Clock skew stopping being small.** The whole skew argument rests on homelab
  hosts being NTP-synced to well under the 5-minute margin. A deployment where that
  is false wants a shorter TTL, not a wider margin — a wider margin is just a
  grace period wearing a disguise.
