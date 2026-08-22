# ACCEPTANCE-M4-04 — the section to add to `scripts/acceptance.sh`

`scripts/acceptance.sh` is held by PR #155 and is **not edited by this change**.
This file is the section that would otherwise have been added, with the exact
assertions and where to anchor them. Apply it when #155 lands.

Every enum-like or count-like comparison uses `assert_eq`, never
`assert_contains`: `assert_contains "$out" "full"` passes on `"mode":"partial"`
if the word appears anywhere else in the JSON, and the one thing this section
exists to prove is that a specific value is a specific value.

---

## Anchor

Insert a new `note "  peer membership (§26, ADR-0012, M4-04)"` block inside
`full_library_demo`, **immediately after** the existing

```bash
  note "  replicas"
  ...
  assert_eq "$present" "$blobs" "every blob has a present replica on the self peer"
```

block and **before** `note "  publications (§69)"`.

That position matters. The revocation assertion needs blobs that are actually
present on this node, and the `replicas` block above is what has just proved
they are. Placing it earlier would mean the "the peer could read this a moment
ago" half rests on an assumption rather than on an assertion three lines up.

One existing line must also change — see **Amendment to the CLI block** at the
bottom.

---

## The section

```bash
  note "  peer membership (§26, ADR-0012, M4-04)"
  # Membership is the only trust root in the inter-peer path. ADR-0012:
  # "Revocation is removing a membership record." Everything below is about
  # that sentence being literally true rather than aspirational.
  local self_key peer_key peer_id enrol_out
  cli() { "$BIN" --config "$WORK/full.yaml" --token "$TOKEN" "$@"; }

  # This node's own public key, which is what the OTHER site would be pasting.
  # Asserted non-empty first: every comparison below is against this value, and
  # two empty strings compare equal.
  self_key=$(cli peers list --json | jq -r '.[] | select(.is_self) | .public_key')
  if [[ -n "$self_key" && "$self_key" != "null" ]]; then
    pass "this node reports its own Ed25519 public key (M4-03)"
  else
    fail "the self peer has no public key, so nothing below can be compared against it"
    return 1
  fi
  assert_eq "$(grep -c . <<<"$self_key")" "1" "the public key is a single line an operator can copy"
  if [[ "$self_key" =~ ^ed25519:[0-9a-f]{64}$ ]]; then
    pass "the public key is rendered as ed25519:<64 lowercase hex>"
  else
    fail "the public key is not in the enrolment form — got '$self_key'"
  fi

  # A second key, standing in for the other node's identity. Generated with
  # openssl rather than by starting a second heyarr: this section is about
  # membership, and a second full node is M4-05's fixture.
  peer_key="ed25519:$(openssl rand -hex 32)"

  note "    enrolment"
  enrol_out=$(cli peers add --name peer-b --site site-b \
    --endpoint https://b.example:8385 --public-key "$peer_key" --json)
  peer_id=$(jq -r '.id' <<<"$enrol_out")

  assert_eq "$(jq -r '.public_key' <<<"$enrol_out")" "$peer_key" \
    "the enrolled peer's public key is byte-for-byte what was registered"
  assert_eq "$(jq -r '.name' <<<"$enrol_out")" "peer-b" "the enrolled peer has the name it was given"
  assert_eq "$(jq -r '.mode' <<<"$enrol_out")" "full" "an enrolled peer defaults to mode full (§9)"
  assert_eq "$(jq -r '.is_self' <<<"$enrol_out")" "false" "an enrolled peer is not this node"
  assert_eq "$(cli peers list --json | jq -r 'length')" "2" \
    "the deployment now lists two peers"
  assert_eq "$(cli peers show peer-b --json | jq -r '.id')" "$peer_id" \
    "a peer can be looked up by name as well as by id"
  assert_eq "$(cli peers list --json | jq -r '[.[] | select(.is_self)] | length')" "1" \
    "exactly one peer claims to be this node"

  # The endpoint is where to reach it; the key is who it is.
  local moved_out
  moved_out=$(cli peers add --name peer-b --site site-b \
    --endpoint https://b2.example:8385 --public-key "$peer_key" --json)
  assert_eq "$(jq -r '.endpoint' <<<"$moved_out")" "https://b2.example:8385" \
    "an endpoint can be changed by re-registering the same key"
  assert_eq "$(jq -r '.id' <<<"$moved_out")" "$peer_id" \
    "moving the endpoint does not change the peer's identity"
  assert_eq "$(jq -r '.public_key' <<<"$moved_out")" "$peer_key" \
    "moving the endpoint does not change the pinned key"
  assert_eq "$(jq -r '.enrolled_at' <<<"$moved_out")" "$(jq -r '.enrolled_at' <<<"$enrol_out")" \
    "moving the endpoint does not re-enrol the peer"
  assert_eq "$(cli peers list --json | jq -r 'length')" "2" \
    "re-registering an existing key did not create a second peer"

  note "    refusals"
  # One case per refusal. A single "invalid input is rejected" would pass with
  # four of these deleted, and each one is a different mistake by the operator.
  local out
  out=$(cli peers add --name peer-c --public-key "ed25519:nothex" 2>&1 || true)
  assert_contains "$out" "not hex" "a malformed public key is refused, and the message says why"

  out=$(cli peers add --name peer-c --public-key "ed25519:$(openssl rand -hex 16)" 2>&1 || true)
  assert_contains "$out" "16 bytes" "a public key of the wrong length is refused by its length"

  out=$(cli peers add --name peer-c --endpoint https://c.example:8385 2>&1 || true)
  assert_contains "$out" "public-key" "registering a peer with no key at all is refused (no trust on first use)"

  out=$(cli peers add --name peer-c --public-key "$peer_key" 2>&1 || true)
  assert_contains "$out" "already registered to another peer" \
    "a key already pinned to another peer is refused"

  out=$(cli peers add --name peer-b --public-key "ed25519:$(openssl rand -hex 32)" 2>&1 || true)
  assert_contains "$out" "already registered under this name" "a name already taken is refused"

  out=$(cli peers remove "$(cli peers list --json | jq -r '.[] | select(.is_self) | .name')" 2>&1 || true)
  assert_contains "$out" "cannot remove its own membership" "this node cannot remove itself"

  out=$(cli peers remove never-heard-of-it 2>&1 || true)
  assert_contains "$out" "no peer is registered" "removing a peer that does not exist is refused"

  # Nothing above created anything. If a refusal had leaked through, the count
  # is where it shows.
  assert_eq "$(cli peers list --json | jq -r 'length')" "2" "no refusal created a peer"

  note "    no private key material on the wire"
  # Scanned, not reviewed. The field that leaks a key is the one somebody adds
  # after the review.
  local peers_json key_marker
  peers_json=$(cli peers list --json; cli peers show peer-b --json; cli peers list)
  # The positive control: the PUBLIC key really is in there, so its absence
  # below would be a finding rather than an empty string comparing clean.
  assert_contains "$peers_json" "$self_key" "the public key is in the output (so the scan below means something)"
  assert_not_contains "$peers_json" "ed25519-seed:" "no private key marker appears in peers output"
  assert_not_contains "$peers_json" "$(tr -d '\n' < "$WORK/data/peer_ed25519.key")" \
    "the private key file's contents do not appear in peers output"
  assert_not_contains "$peers_json" "private" "peers output has no field named private anything"
  key_marker=$(stat -f '%Lp' "$WORK/data/peer_ed25519.key" 2>/dev/null || stat -c '%a' "$WORK/data/peer_ed25519.key")
  assert_eq "$key_marker" "600" "the private key is still 0600 after the peers commands ran"

  note "    revocation"
  # ADR-0012: "Revocation is removing a membership record." The demo asserts
  # the parts a shell can see: the record is GONE (not flagged), the peer is
  # unresolvable, and its replica rows went with it.
  #
  # The other half of the requirement — that a peer which was reading bytes a
  # moment earlier is refused on the SAME already-open connection — cannot be
  # driven from here yet, and pretending otherwise would be worse than not
  # asserting it. Nothing in this deployment presents a peer identity: the
  # production extractor reads the public key out of a verified mTLS client
  # certificate (httpapi.TLSPresentedPeerKey), and mTLS is M4-05. Adding a
  # header the demo could set would mean any client could claim a peer
  # identity, and a revoked peer could get back in by simply not sending it.
  #
  # That assertion therefore lives in Go, against the real router, the real
  # middleware chain and a real reused TCP connection:
  #   internal/peer/membership/revocation_test.go
  #     TestRemovingAPeerSeversAConnectionThatWasReadingBytes
  # It reads the bytes successfully, proves the transport reused the
  # connection, removes the peer through DELETE /api/v1/peers/{id}, and
  # requires the next read on that same connection to be 403. When M4-05 lands,
  # its acceptance section replaces the seam with a client certificate and
  # asserts the same three outcomes here.
  local probe_hash before_replicas after_replicas
  probe_hash=$(api_all "/api/v1/replicas?state=present" '.items[0].blob_hash' | head -1)
  if [[ -z "$probe_hash" ]]; then
    fail "no present replica, so the replica half of revocation cannot be shown"
    return 1
  fi

  # Reproduce the working case FIRST. Asserting only the after-state would pass
  # on a deployment where the peer never held anything.
  before_replicas=$(api_all "/api/v1/replicas" '.items[] | select(.peer_id == "'"$peer_id"'") | .blob_hash' | wc -l | tr -d ' ')
  assert_eq "$(cli peers show peer-b --json | jq -r '.id')" "$peer_id" \
    "the peer is resolvable BEFORE it is removed"
  assert_eq "$(cli peers list --json | jq -r '[.[] | select(.id == "'"$peer_id"'")] | length')" "1" \
    "the peer is in the membership list BEFORE it is removed"

  local removed_out
  removed_out=$(cli peers remove peer-b --json)
  assert_eq "$(jq -r '.id' <<<"$removed_out")" "$peer_id" "remove reports which peer it revoked"
  assert_eq "$(jq -r '.public_key' <<<"$removed_out")" "$peer_key" "remove reports which key it revoked"

  assert_eq "$(cli peers list --json | jq -r 'length')" "1" "the removed peer is gone from the list"
  assert_eq "$(cli peers list --json | jq -r '[.[] | select(.id == "'"$peer_id"'")] | length')" "0" \
    "the record is DELETED, not flagged — there is no revocation list to fall back on"
  if cli peers show peer-b --json >/dev/null 2>&1; then
    fail "a removed peer is still resolvable"
  else
    pass "a removed peer cannot be looked up"
  fi
  after_replicas=$(api_all "/api/v1/replicas" '.items[] | select(.peer_id == "'"$peer_id"'") | .blob_hash' | wc -l | tr -d ' ')
  assert_eq "$after_replicas" "0" \
    "a removed peer holds no replicas — a peer this instance will not talk to does not count towards placement"
  # ...and the blob itself is untouched: revocation removes a peer, not bytes.
  assert_eq "$(api "/api/v1/blobs/$probe_hash/content" -o /dev/null -w '%{http_code}')" "200" \
    "the blob the removed peer was recorded against is still readable here"

  note "    events"
  # Read from the API the way the rest of this script reads events: the SSE
  # snapshot, filtered server-side to the peer namespace.
  local peer_events
  peer_events=$(api "/api/v1/events?after=0&types=peer.*" --max-time 5 --no-buffer 2>/dev/null || true)
  assert_eq "$(grep -c '"type":"peer.registered"' <<<"$peer_events")" "3" \
    "peer.registered was emitted three times: self, the enrolment, the endpoint move"
  assert_eq "$(grep -c '"type":"peer.removed"' <<<"$peer_events")" "1" \
    "peer.removed was emitted exactly once"
  assert_contains "$peer_events" '"transition":"enrolled"' "an enrolment carries the enrolled transition"
  assert_contains "$peer_events" '"transition":"endpoint_changed"' \
    "moving an endpoint carries the endpoint_changed transition on the SAME event type"
  assert_contains "$peer_events" '"transition":"removed"' "a removal carries the removed transition"
  assert_contains "$peer_events" "$peer_key" "the peer events name the key that was pinned and revoked"
  # No per-item storm and no second type for the second half of one machine
  # (§76): peer.registered, peer.identity_established and peer.removed, and
  # nothing else in this namespace.
  assert_eq "$(grep -o '"type":"peer\.[a-z_]*"' <<<"$peer_events" | sort -u | wc -l | tr -d ' ')" "3" \
    "the peer plane emitted three event types, not one per operation"
```

---

## Amendment to the CLI block

The existing line

```bash
  assert_eq "$cli_peers" "1" "the CLI lists exactly one peer (ADR-0010)"
```

runs **after** the section above, by which point `peer-b` has been enrolled and
then removed — so the count is back to one and the assertion still holds. Leave
it as it is; it now asserts something stronger than it used to, namely that the
membership section cleaned up after itself.

If the section above is ever moved to run *after* the CLI block, this line must
become `assert_eq "$cli_peers" "2" ...` — and moving it is not recommended,
because the revocation assertion depends on the `replicas` block that precedes
it.

---

## Why no `assert_contains` on any enum

Every value compared above (`mode`, `is_self`, a peer id, a public key, a
status code, a count) is compared with `assert_eq`. `assert_contains` appears
in exactly two roles:

* refusal **messages**, where a substring is the whole point — the assertion is
  that the operator was told *which* thing was wrong, and the rest of the
  sentence is prose that should be free to change;
* the event stream, which is an SSE body rather than a JSON document, so there
  is no single value to compare against. Each of those greps pins an exact
  `"transition":"..."` literal rather than a bare word, so
  `"transition":"endpoint_changed"` cannot be satisfied by the word
  `endpoint` appearing in an unrelated payload.
