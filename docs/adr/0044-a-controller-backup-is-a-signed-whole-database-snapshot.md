# 0044. A controller backup is a signed whole-database snapshot, a restore verifies it, and it carries the controller's identity wrapped

**Status:** Proposed
**Date:** 2026-08-25

## Context

Milestone 7 makes losing the controller survivable. Nothing about it exists yet:
`internal/persistence/sqlite/db.go` says resilience "comes from backup streams
to Full Peers (§50) and restore tooling (§51)" and stops there;
`internal/peer/degraded` is a two-line `doc.go`. A backup format, and what a
restore is entitled to believe about it, are decided once and then assumed by
every deliverable below this one. M6's hardest lesson was that a mechanism with
no caller cannot be proven and the gap was found late; M7's equivalent is
cheaper to get wrong and far more expensive to reverse, so it is decided in
writing first.

This record answers six questions. It ships no behaviour.

**Read "the controller" throughout as "a peer's own control plane."** This ADR
was written before ADR-0038 (each peer is authoritative for its own site) moved
to Accepted (#292). Under ADR-0038 there is no single hub controller whose loss
is the disaster; every peer runs its own single-writer control plane (ADR-0003 in
its strongest form — one database per peer), and the backup and restore here are
of a peer's **own** control plane, streamed to the peers that trust it. This does
not soften the milestone into a convenience: content, personal state and the read
catalogue re-fetch themselves from the fabric, but a peer's control plane and its
Ed25519 identity have **no fetch path** — nothing else holds an authoritative
copy of this peer's control state, and nothing can reissue its identity — so
recovery is necessarily control-plane-first (ADR-0038's ratification note records
this, correcting its own "recovery is a fetch, not a restore" clause). The six
answers below are unchanged by the reframing; only the noun and the reconvergence
story move — and the identity question (Q4) gets **sharper**, not moot.

Invariant 5 / ADR-0003 rules the whole thing: **never active-active SQLite**.
This milestone is backup and restore, not replication, and any answer that
drifts toward two writers has taken a wrong turn.

A fact that shapes every answer below: **the control database holds no secrets
and no provider configuration.** Provider endpoints and credentials, libraries
and the peer's own name live in the operator's **config file** — "a node's whole
identity is one reviewable document" (`00015_providers.sql`,
`internal/config/config.go`) — and the identity private key is a separate file
in the data directory (`internal/peer/identity`, `peer_ed25519.key`). The
database is desired content, policy, grants, leases, membership and observed
provider *health*. So a whole-database backup omits secrets by construction, not
by stripping, and "what recovery reconstructs" (§82) necessarily spans more than
one file.

## Decision

### The artifact: a signed bundle, not a bare database file

A backup is a **bundle** a peer holds as inert bytes:

- a **manifest** — `{generation, wall-clock instant, BLAKE3 digest of the
  database snapshot, BLAKE3 digest of the wrapped-secrets blob, omission set}`;
- the manifest's **Ed25519 signature** by the controller's identity key;
- the **database snapshot** — the `VACUUM INTO` output (question 1), carrying no
  secrets because none are in the database;
- a **wrapped-secrets blob** — the controller's identity private key, and
  (deployment-policy opt-in only) the config file, encrypted under an
  operator-held recovery secret the peers cannot unwrap (questions 4, 6).

Everything a restore trusts hangs off the manifest signature (question 2);
everything a peer must not do hangs off the snapshot being inert (question 5).

### 1. "Continuous" means periodic whole-database snapshots. RPO = the snapshot interval.

A backup is a **whole-database snapshot taken with `VACUUM INTO`** on a cadence
(default **every 5 minutes**, and once more on graceful shutdown). "Continuous"
means *always maintained and always fresh within that window*, not *zero loss*.

- **RPO stated as a number:** at most the snapshot interval — by default the last
  **5 minutes** of control-plane writes. In units of work rather than seconds:
  the desired-content, policy, grant, lease, membership and provider-config edits
  made since the last successful snapshot. No user bytes are ever at risk here —
  content lives in the CAS (converged by BLAKE3, ADR-0005) and personal state in
  encrypted CRDTs (§38); neither is in this database. What is lost is a few
  minutes of operator intent, which is re-doable, not user data.

`VACUUM INTO` is chosen over the two alternatives §49 leaves open:

- **WAL frame shipping** buys a near-zero RPO and is rejected. It is the option
  that most resembles the thing ADR-0003 forbids; a shipped WAL stream is *not a
  database until replayed*, so a peer cannot open it to confirm it holds "enough
  to rebuild" (§82); and it needs frame capture, ordering and replay machinery
  whose cost is unjustified for a control plane whose write rate is low by
  construction. What it would have bought — the last few minutes of edits — is
  the cheapest thing in the system to lose.
- **A base copy plus WAL frames (hybrid)** carries the same machinery for the
  same unneeded RPO and is rejected for the same reason.

`VACUUM INTO` also earns its place mechanically: it reads a transactionally
consistent snapshot and writes a **self-contained, defragmented single file with
no `-wal` sidecar**. That directly answers the hazard `db.go`'s `Close` comment
names — *"a populated `-wal` beside a copied database file is a silently stale
backup rather than a loud failure"* — by never producing a sidecar to copy. It
is plain SQL, so it exists in the pure-Go driver (ADR-0004) without the C Online
Backup API.

Each snapshot carries a **monotonic generation** derived from the event-log
high-water mark (invariant 7 guarantees every state transition emits an event,
so this counter tracks *actual progress*, not wall-clock). A snapshot taken when
nothing changed does not advance its generation — which is honest, and is what
makes #282's "assert the generation advanced" a real assertion rather than a
tautology.

### 2. A restore trusts a signature over the controller's identity, not the peer that hands it over.

ADR-0035 answered this for bytes — *nothing it has not re-verified* — against an
independent whole-object digest. A database has no equivalent digest that came
from somewhere other than the party serving it, and ADR-0043 already settled the
shape of that problem: **a hash published by the peer serving the file is a
statement by the same party as the file.** The backup arrives from an enrolled
peer over a pinned mTLS session (ADR-0012), so it is authenticated in transit —
but it is still *that peer's claim about what the controller's database
contained*, and the control plane is the crown jewels: grants, membership,
policy, provider config.

So **the original controller signs every backup** — the manifest above, with its
**Ed25519 identity key** (ADR-0012), the same key every enrolled peer already
pins. Signing is asymmetric: the controller signs with its private key, and
anyone verifies with the **public** key they already hold. **No peer ever needs
the private key to verify, so no peer holds it** (in the clear — the wrapped copy
of question 4 is ciphertext). Verification happens twice, at the two places the
anchor genuinely exists:

- **At receive.** A Full Peer refuses to store a backup whose signature does not
  verify against the **origin peer's** public key — the key it has pinned for the
  peer whose control plane this is, in its own membership record (under ADR-0038
  enrolment is mutual, so "the controller's key" is just the pinned key of
  whichever peer produced the backup). A stored backup is therefore
  authentic-by-construction, and a compromised peer cannot quietly substitute a
  forged control plane it will later serve to a recovering operator.
- **At restore.** `recover` re-verifies the signature against the controller's
  identity fingerprint, which it prints, so a peer that stored a forgery before
  enrolment — or was itself the forger — is still caught.

The alternative — **trust membership and say so** — is defensible and rejected.
It would let any single compromised Full Peer hand a recovering operator a forged
control plane (forged grants, forged membership) with no way to detect it. What
signing costs is that the identity key must survive the disaster (question 4);
what it buys is that a restored control plane is provably the one that was
backed up, which for this data is worth the key-management burden.

### 3. No election. `recover` names the peer, but is loud about generation and age.

§51's target command already names the peer — `heyarr recover --from-peer peer-b`
— and that is deliberately an operator decision with **no election**, because an
election is a consensus protocol and this milestone is explicitly not that.

The failure to prevent is that restoring from the *stalest* peer is a silent
data-loss event dressed as a successful recovery. So:

- `recover` **states the generation, the wall-clock age, and the omission set of
  what it is about to restore, and requires confirmation** before it touches
  anything.
- A read-only **survey** (`heyarr recover --survey`, or the listing form of the
  same command) asks **every reachable trusted peer what generation and age it
  holds, and restores nothing**. The operator picks the freshest deliberately
  rather than by luck.

### 4. A restored controller keeps its Ed25519 identity; the private key rides the backup, wrapped under an operator recovery secret.

This is the 🔴 question with the longest tail, and ADR-0038 makes it the whole
milestone's point: the epic's acceptance sentence is *"a peer that loses its disk
is restored from a peer that trusted it, comes back with the same identity, and
**no other peer is re-enrolled or reconfigured**."* Every other node has pinned
this node's key (ADR-0012). §82 lists "peer identity/configuration" among what a
recovery must reconstruct, which is the steer: **keep the identity**, so **no
other peer is re-enrolled** — the alternative being "the disaster continuing" as
an operator hand-re-pins this peer on every node that trusted it.

Keeping the identity requires the private key to survive the controller host. It
does so in the bundle's **wrapped-secrets blob, encrypted under a recovery secret
the operator holds out of band** — exactly invariant 6's pattern (*peers store
wrapped keys they cannot unwrap*), applied to the controller's own key. Every
Full Peer that holds a backup therefore holds the identity key as **ciphertext it
cannot open**, and a compromised peer gains nothing. At recovery the operator
supplies the recovery secret, the key is unwrapped, and the restored controller
comes up as the **same peer**.

This is a **conscious departure** from an existing documented stance, and names
it rather than quietly overriding it. `internal/persistence/catalog/catalog.go`
warns, where it records only the public half: *"the private key is never passed
to this method and never enters the database: backups stream to peers, and a
restored backup carrying a private key produces two machines able to authenticate
as one peer."* That warning stands and is respected — **the private key never
enters the database snapshot**; the snapshot still carries only the public key.
What this ADR adds is a *separate* wrapped blob beside the snapshot, and the
hazard the comment names (two machines as one peer) is exactly question 4's
two-live-controllers problem, bounded here three ways: a peer holding the bundle
holds only ciphertext and cannot clone anything; only the operator, with the
recovery secret, can materialise the key, and only during a recovery they
initiated; and the incarnation counter below lets peers detect a twin that
results from operator error. The alternative the comment implicitly prefers —
never carry the key — is question 4's fallback, and its cost is hand-re-enrolling
every peer during the disaster.

Without the recovery secret, `recover` still reconstructs all control-plane data,
but the controller comes up under a **new** identity and every peer must re-pin —
this is that fallback (a known hole, question 6), not a surprise.

**Two live peers with one identity** is *not mechanically prevented* — a clean
full restore reproduces all three identity artefacts consistently (the key file,
the `peers`-table public key, the CAS root marker), so
`internal/peer/identity`'s three-way agreement check passes on both nodes. It is
**prevented by the operator's assertion**: invoking `recover` is the assertion
that the lost peer is gone, and §51's no-election stance places that judgement
with the operator. The named mitigation that would move this from *merely
unlikely* to *detected* is a **monotonic incarnation** the restored node bumps and
re-announces, letting the peers that trust it reject a returning stale twin;
#282/#284 implement it or explicitly defer it.

### 5. A backup is structurally not an openable control plane, and not the catalog snapshot.

Invariant 5 forbids two writers on a control database. A peer holding a controller
backup must be **unable to open it as one**, as a mechanism rather than a
sentence — the precedent is `internal/peer/catalog`, in the other direction:

- A held backup is **inert bytes on the peer** — the bundle files
  (`controller-backup/<generation>/` holding the snapshot `.db`, the `.manifest`,
  its `.sig`, and the wrapped-secrets blob) under the peer's data directory.
  **No code path on a peer ever opens the snapshot with a writable handle.** When a peer must read one — to report its generation for the survey
  — it opens it `mode=ro` + `query_only`, so a write fails at the **storage
  layer** with `SQLITE_READONLY`, exactly as `catalog.OpenReadOnly` does. The
  only thing that ever opens a backup as a live writable control database is the
  `recover` path, on a *different node with a different data directory* by
  construction.
- **Told apart from the catalog snapshot on disk** by name, directory and
  content. `catalog-snapshot.db` (§52, read snapshot) has `snapshot_meta` /
  `snapshot_*` tables; a controller backup has `goose_db_version` + `peers` — the
  same control-database marker `catalog.refuseControlDatabase` already
  recognises. So one recogniser tells all three artefacts apart: the peer's read
  snapshot, a held controller backup, and (on a controller) its live `heyarr.db`.
  A peer confusing a backup for a snapshot has either lost a controller or gained
  a second writer, and the recogniser refuses both.

### 6. What a backup omits, named so a restore has known holes rather than surprises.

The omissions are mostly not a backup-time choice — they are a consequence of
where things already live, and the discipline is pre-existing:
`00015_providers.sql` and `internal/persistence/catalog/providers.go` both state
it outright — *"putting a second copy in a database that gets backed up to peers
(§50) would be adding a way to leak it in exchange for nothing... this table keeps
it out of the backup stream."* So:

- **Provider credentials AND provider configuration are absent by construction.**
  Neither is in the database — endpoints, credentials and which providers exist
  all live in the operator's config file (`internal/config`), and the database
  holds only observed provider **health**, keyed by name. A `VACUUM INTO`
  snapshot therefore cannot carry them. A restored controller recovers the health
  *history* but comes up with **no provider definitions until the config file is
  restored**, and it reports that gap — the providers it has health rows for but
  no live configuration — rather than letting it surface as a failed call.

- **The config file is a distinct recovery input**, and this is what §82's
  "peer identity/configuration" and "external provider credentials may require
  protected backup depending on deployment policy" actually point at. By
  **default it is the operator's to keep** (it is where their other secrets
  already are). The **deployment-policy opt-in** is to fold it into the bundle's
  wrapped-secrets blob (question 4's mechanism), so a deployment that wants
  zero-touch recovery of providers wraps the config under the recovery secret
  exactly as the identity key is wrapped.

What the database backup *does* carry — desired content, policy, grants, leases,
peer membership including peers' **public** keys, and provider health — is
everything the control plane owns that is not a secret and not a per-node
configuration file. The one secret in the bundle at all, the controller's own
identity key, is **ciphertext no peer can open** (question 4), which is also why a
held backup (question 5) cannot be turned into a controller impersonation.

## Consequences — the assertions this milestone now owes

`#281` produces no code; it produces the list of assertions the rest of M7 must
make provable in `make demo` (§53, #187: an assertion whose subject is absent is
not coverage):

- **RPO** → measure how much is lost: a change made after a snapshot is absent
  from a restore of that snapshot; a change before it is present.
- **Trust** → a **tampered backup is refused** — flip one byte of the file or the
  manifest and the signature check fails, at receive and at restore. Every such
  refusal is `assert_eq` on the reason, never `assert_contains` (this file's own
  history: `"not_satisfied"` contains `"satisfied"`).
- **Identity** → after `recover` with the recovery secret, **no peer re-enrols
  anyone** (the identity is kept); without it, the restored controller is a new
  identity and is **refused by every peer** — whichever path, asserted by name.
- **Read-only** → a write to a peer's **held backup** fails at the **storage
  layer** (`SQLITE_READONLY`), with a positive control that a legitimate read of
  the same file succeeds.
- **Generation** → a backup's generation **advanced** after a state change
  (#282), and a peer's **catalog-snapshot version did not move** when a backup
  crossed to it (#283) — two artefacts, two counters, told apart.

## What would make us revisit this

- **The control plane's write rate stops being low.** The 5-minute-RPO argument
  rests on control-plane writes being cheap to lose. A deployment that drives
  heavy, non-reproducible control-plane change would push toward the WAL-shipping
  RPO that this record rejects — a new ADR, because it reopens the ADR-0003 line.
- **`VACUUM INTO` becoming too coarse.** If databases grow to where a full
  snapshot every few minutes is wasteful, the answer is a longer interval or
  incremental page tracking — not WAL replication, whose objection is invariant 5,
  not cost.
- **A hardware root of trust for the identity key.** If the controller's identity
  key can live in a TPM/HSM/1Password-style store that survives the host, the
  wrapped-key-in-backup mechanism (question 4) becomes one option among several,
  and question 4's fallback ("comes up new, re-enrol") stops being the only
  no-secret path.
- **Two live peers with one identity needing to be *prevented* rather than
  *asserted away*.** The incarnation counter named in question 4 is the first
  step; a deployment that cannot rely on operator discipline would need more, and
  that is a distributed-leadership problem (ADR-0003's own "revisit if"), not a
  backup one.
