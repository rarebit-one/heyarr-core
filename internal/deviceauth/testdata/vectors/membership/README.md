# Membership op-set golden vectors

Cross-implementation vectors for the Voidbind **membership op-set** (ADR-0007):
the v3 `enrolment` op (`add` / `remove`) and the deterministic `Evaluate` over a
set of ops. voidbind-go generates them (`go test ./enrolment -run TestVectors
-update`), and every other implementation — voidbind-kmp's `Membership.evaluate`,
heyarr-core's `deviceauth`, All Thing's `internal/membership` — replays them
byte-for-byte. A vector that passes here and fails there is a divergence in the
port, never a "flaky key".

## Layout

One file per case, `<case>.json`, all keys **test-only** (deterministic seeds
appear in the file so a consumer can re-sign and reproduce every `hash`):

```jsonc
{
  "name":        "genesis-a-b",                 // == file stem
  "description": "genesis admits A; A admits B; …",
  "usr":         "ed25519:<hex>",               // the identity = genesis public key
  "now":         1788267600,                    // unix seconds Evaluate is run at
  "keys": {                                     // test seeds (hex), by label
    "genesis": { "sign_seed": "<hex>", "id": "ed25519:<hex>" },
    "A":       { "sign_seed": "<hex>", "enc_seed": "<hex>", "id": "ed25519:<hex>" }
  },
  "ops": [                                      // op TOKENS, in REVERSE build order
    { "label": "add-A", "token": "<b64url>.<b64url>", "hash": "sha256:<hex>" }
  ],
  "expect": {
    "members":     { "ed25519:<A>": { "admitted_by": "sha256:<hex>", "denc": "x25519:<hex>", "admitted_at": 1788264000, "expires": 1796040000 } },
    "removed":     [ "ed25519:<B>" ],           // sorted
    "heads":       [ "sha256:<hex>" ],          // sorted
    "rejected":    { "sha256:<hex>": "bad_prev" },      // structurally invalid: uncitable
    "ineffective": { "sha256:<hex>": "unauthorised" }   // valid but changes nothing right now
  }
}
```

- `ops` is deliberately **not** in causal order: a consumer must resolve `prev`
  itself. Feeding the same list in any permutation must give the same `expect`
  (the CRDT property the Go property tests assert).
- `hash` is `"sha256:" + hex(sha256(token))` and is authoritative for `prev`.
- `denc` for label X is `"x25519:" + enc_seed` — a stand-in string; `Evaluate`
  treats it as opaque.
- `rejected` reasons: `malformed`, `bad_signature`, `foreign_usr`, `bad_prev`.
  A rejected op is keyed by the hash of its raw token bytes.
- `ineffective` reasons: `unauthorised`, `outranked`, `removed`, `superseded`,
  `expired`, `not_yet_valid`. An ineffective op is still part of the state (citable as
  `prev`, counts toward `heads`).

## Cases

| case | what it pins |
|------|--------------|
| `genesis-a-b` | genesis → A → B, all members; heads = [add-B] |
| `a-removes-b` | A removes B: B in `removed`, re-add by A is `removed` |
| `concurrent-mutual-remove` | A and B remove each other concurrently: **seniority wins** — B's remove is `outranked`, A stays, B removed |
| `junior-remove-acknowledged` | B removes A and A's later remove *cites* it: not a fork — A's remove is `unauthorised` |
| `readd-refused-unless-genesis` | a removed dev re-added by a member, or by genesis without citing the remove, stays removed; by genesis citing the remove is a member |
| `expired-add` | an add past its `exp` is not a member; an op it signed after expiry is `unauthorised`; one signed while valid stands |
| `bad-prev` | an op citing an unknown hash is `bad_prev`, and so is anything built on it; a member op with `prev: []` is `malformed` |
| `foreign-usr` | an op whose `usr` is another identity is `foreign_usr` |
| `junk` | unparseable / tampered tokens are rejected, never fatal |
| `v2-cert-as-genesis-add` | v1 and v2 certs reinterpreted as `{op:add, by:usr, prev:[]}` |
| `concurrent-add-and-remove` | remove concurrent with add wins; a member's earlier admissions survive its removal (no cascade through history) |
| `stale-heads-after-removal` | a removed device keeps signing with stale heads: every op is void (`outranked`), and ops by the devices it "admitted" are `unauthorised` |
| `senior-concurrent-add-survives` | a senior's add concurrent with a junior's remove of it stands; a junior's add concurrent with a senior's remove of it is void |
| `cosig-reserved` | a remove carrying `cosig` entries is honoured as if without them |

Regenerate with `go test ./enrolment -run TestVectors -update`.
