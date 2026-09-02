# 0067. A paired device enrols itself, and earns only the read floor

**Status:** Accepted
**Date:** 2026-09-02
**Builds on:** ADR-0022 (enrolment is pairing), ADR-0032 (device keys land before they authorise anything), ADR-0048 (device-cert authentication), ADR-0065 (an enrolled device earns write by admin authorisation), ADR-0066 (the node hosts the Voidbind relay)

## Context

After pairing, a phone holds a cert the user signed for its device key. That
cert is the whole of what ADR-0048 authenticates — plus a per-request proof the
caller holds the key. Yet the peer honoured the cert only after an **admin**
posted it to `POST /api/v1/identities/devices`. The phone could prove
everything and still had to wait for a CLI or a curl. For a first-party client
(ADR-0054) that is the wrong shape: pairing on the phone, then enrolment from
somewhere else, is two rituals for one fact.

Two things must not change while closing that gap. ADR-0032's enrol-before-trust
gate — the *user* pin, made deliberately and out of band by an admin — stays the
root of trust. And ADR-0065's stance — write is an admin's authorisation of a
device key, never something a device grants itself — stays exactly as it is.

## Decision

**Add a public `POST /enrol {cert, proof, name}` that enrols the device the cert
names, judged by the verifier the `Device` scheme already uses, and grants
nothing beyond enrolment.**

- The route is outside `/api/v1` because the caller is, by definition, not yet
  enrolled and so cannot authenticate. The request *is* the credential: the
  cert and a fresh possession proof over it — the two halves of a `Device`
  credential, sent as JSON fields instead of joined by `~`.
- Verification is `deviceauth.Store.SelfEnrol`, which runs the cert and proof
  through the same `verifyCert` / `verifyPossession` the per-request
  `Verify` runs — one verifier, so the cert a device enrols with is judged by
  exactly the rule it will authenticate under. The pinned-user check holds: a
  cert from a user this peer has not pinned is refused.
- **Nothing beyond the read floor is granted.** The enrolled device
  authenticates as its user at `read`. Write is still
  `POST /api/v1/session/management-grants {device_key}` by an admin (ADR-0065),
  and the admin surface is still admin-token-only. This route adds no
  authority the cert did not already carry; it removes a manual step.
- **Idempotent on the device key**, so a phone that lost the response can
  re-submit and get its row (200, not 409). **Revocation wins**: a revoked
  device re-presenting its cert is refused, and a cert naming a different user
  than the row holds is refused — neither is a path back in.
- **Fail closed and opaque.** Every refusal — unpinned user, bad signature,
  expired cert, wrong-key or stale proof, revoked device — is the same 401 with
  the same detail the `Device` scheme gives; the reason goes to the log under
  that scheme's bounded label set. Only a body that is not a credential at all
  (not JSON, unknown fields, over the cap) is a 400.

## Consequences

- The phone's flow is one ritual: pair through the node's relay (ADR-0066),
  `POST /enrol` with the cert it just received, and it reads. The admin's two
  acts are the ones that carry authority — pin the user once, authorise the
  device key for write if it should write — and neither is on the phone's
  critical path to reading.
- The route is unauthenticated and bounded like the other public mounts: a
  16 KiB body cap and the same store-level checks. Heyarr carries no per-IP
  rate limiter on any public route today (`/login`, `/pair`, `/render`
  included); this route inherits that state rather than introducing a limiter
  for one endpoint. A limiter, when it comes, belongs on the public group.
- The client half (voidbind-kmp minting a possession proof) is tracked outside
  this repository; until it ships, `voidbind identity credential` on a machine
  produces the same `<cert>~<proof>` and the proof can be posted by hand.

## What would make us revisit

- **A per-user "any of my devices may write" stance** (ADR-0065's own revisit
  trigger). If write were ever granted at enrolment, this route would become a
  write-granting unauthenticated endpoint, and would then need the admin back
  in the loop. That is the line this ADR draws: enrolment here is trust in the
  *user's* signature; authority remains the admin's.
- **A public-route rate limiter.** When one lands, `/enrol` joins it; the ADR
  above does not need to change.
