# shellcheck shell=bash
# A section of scripts/acceptance.sh: sourced by it, in order, never run alone.
# ---------------------------------------------------------------------------
# THE VAULT BLOB THAT REPLICATES BY A PIN, NOT BY AN ASSET (ADR-0096, #540)
# ---------------------------------------------------------------------------
#
# A vault blob is bytes a client encrypted and uploaded (internal/api/vaultblob):
# it has NO `assets` row, so the canonical blob set — every blob a live asset
# accounts for — never contains it, and the peer-convergence diff alone would
# never replicate it or retain it. A placement pin is the other thing that makes
# a blob belong on a peer, and this scene proves both halves of what the pin buys:
#
#   REPLICATION. A blob is pinned to node B through the device placement route
#   while NO asset on B names it, and it crosses the wire to B when B reconciles —
#   because the convergence union adds the pin's (blob, peer) to the plan even
#   though the canonical-set diff does not. It is per-(blob, peer): the blob is
#   pinned to B and reaches B, and this scene sends it nowhere else.
#
#   RETENTION. The same blob on node A has no asset either — its asset is deleted,
#   which is a vault blob's real condition — so by every measure the first
#   milestones had it is garbage, and `gc --apply` with a one-nanosecond grace
#   window SPARES it, because a placement pin counts as a reference. Remove the
#   pin and the very same sweep reclaims it: the pin was the whole of its retention.
#
# It is its own two-peer fabric, isolated under $WORK like the two-peer and swarm
# arcs, so it shifts no count any other section asserts. Both nodes ingest the one
# fixture — a peer can only be told a source holds a blob it has a row for, which
# is content addressing's own rule (a replica of an unknown blob cannot be
# recorded) — and then node B's asset AND bytes for it are deleted, so that when B
# pulls it back the ONLY thing marking it desired on B is the pin.
vault_placement_demo() {
  local root="$WORK/vaultplacement" lib
  lib="$root/library"
  mkdir -p "$lib/movies/Vault Reel (2023)"

  # A small blob — this scene is about a pin, not a transfer's size — and random,
  # so its digest is not something the fixture generator already knows.
  local secret="$lib/movies/Vault Reel (2023)/Vault.Reel.2023.1080p.mkv"
  head -c 262144 /dev/urandom > "$secret"

  local cfg_a="$WORK/vaultplacement-a.yaml" cfg_b="$WORK/vaultplacement-b.yaml"
  local n
  for n in a b; do
    mkdir -p "$root/$n"
    cat > "$WORK/vaultplacement-$n.yaml" <<YAML
data_dir: $root/$n/data
peer:
  name: site-$n
  site: site-$n
  listen: 127.0.0.1:0
log:
  level: info
  format: json
http:
  addr: ""
libraries:
  - name: films
    content_type: movie
    roots: ["$lib/movies"]
YAML
  done

  VP_ROOT="$root"
  local sock_a="$root/a/data/heyarr.sock" sock_b="$root/b/data/heyarr.sock"
  local log_a="$root/a.log" log_b="$root/b.log"
  local token_a token_b
  token_a=$("$BIN" --config "$cfg_a" token create acceptance --scopes admin --json | jq -r .token)
  token_b=$("$BIN" --config "$cfg_b" token create acceptance --scopes admin --json | jq -r .token)
  VP_SOCK_B="$sock_b"; VP_TOKEN_B="$token_b"

  start_peer_node "$cfg_a" "$log_a" all
  start_peer_node "$cfg_b" "$log_b" all

  vp_a() { curl -sS --unix-socket "$sock_a" -H "Authorization: Bearer $token_a" "${@:2}" "http://heyarr$1"; }
  vp_b() { curl -sS --unix-socket "$sock_b" -H "Authorization: Bearer $token_b" "${@:2}" "http://heyarr$1"; }
  local vcli_a="$BIN --config $cfg_a --token $token_a"
  local vcli_b="$BIN --config $cfg_b --token $token_b"

  local s waited
  for s in "$sock_a" "$sock_b"; do
    waited=0
    while (( waited < 600 )); do
      curl -sf --unix-socket "$s" http://heyarr/readyz >/dev/null 2>&1 && break
      sleep 0.1; waited=$(( waited + 1 ))
    done
    if (( waited >= 600 )); then
      fail "vault-placement: $s never became ready"; tail -20 "$log_a" "$log_b"; return 1
    fi
  done

  local l
  for l in "$log_a" "$log_b"; do
    waited=0
    while (( waited < 900 )); do
      (( $(grep -c '"msg":"ingested"' "$l" 2>/dev/null || true) >= 1 )) && break
      sleep 0.1; waited=$(( waited + 1 ))
    done
    if (( waited >= 900 )); then
      fail "vault-placement: $l never ingested the fixture"; tail -20 "$l"; return 1
    fi
  done

  local addr_a addr_b
  addr_a=$(peer_listen_addr "$log_a") || { fail "vault-placement: node A never bound a peer surface"; return 1; }
  addr_b=$(peer_listen_addr "$log_b") || { fail "vault-placement: node B never bound a peer surface"; return 1; }

  # -------------------------------------------------------------------------
  note "  a vault blob is uploaded to node A and pins itself there (ADR-0021, ADR-0096)"
  # -------------------------------------------------------------------------
  #
  # The digest is learned from the asset node A made of the fixture — content
  # addressing means the bytes on disk hash to the id the vault route verifies
  # against — and the SAME bytes are uploaded through the vault ingest route, which
  # stores them (idempotently) and pins them to node A itself.
  VP_BLOB=$(vp_a /api/v1/assets | jq -r '.items[0].blob_hash')
  assert_contains "$VP_BLOB" "blake3:" "node A hashed the fixture to a blake3 id"

  local up
  up=$(vp_a "/api/v1/vault/blobs/$VP_BLOB" -X PUT \
    -H 'Content-Type: application/octet-stream' --data-binary @"$secret")
  assert_eq "$(jq -r '.hash' <<<"$up")" "$VP_BLOB" \
    "the vault ingest route stored the ciphertext under the id the client declared"

  # -------------------------------------------------------------------------
  note "  two peers, enrolled by public key in both directions (§26, ADR-0012)"
  # -------------------------------------------------------------------------
  local key_a key_b pid_b self_a
  key_a=$($vcli_a peers list --json | jq -r '.[] | select(.is_self) | .public_key')
  key_b=$($vcli_b peers list --json | jq -r '.[] | select(.is_self) | .public_key')
  self_a=$($vcli_a peers list --json | jq -r '.[] | select(.is_self) | .id')
  $vcli_a peers add --name site-b --site site-b --mode full \
    --public-key "$key_b" --endpoint "https://$addr_b" --json >/dev/null 2>&1
  $vcli_b peers add --name site-a --site site-a --mode full \
    --public-key "$key_a" --endpoint "https://$addr_a" --json >/dev/null 2>&1
  $vcli_a peers add --name site-a --public-key "$key_a" --endpoint "https://$addr_a" --json >/dev/null 2>&1
  $vcli_b peers add --name site-b --public-key "$key_b" --endpoint "https://$addr_b" --json >/dev/null 2>&1

  # The peer id node B knows itself by, which is what the pin must name: the
  # convergence union matches a pin's peer_id against the Full Peer ids, not a name.
  pid_b=$($vcli_b peers list --json | jq -r '.[] | select(.is_self) | .id')
  assert_eq "$(( ${#pid_b} > 0 ))" "1" "node B knows the peer id it converges as"

  # -------------------------------------------------------------------------
  note "  the device pins the vault blob to node B (POST /api/v1/vault/placements)"
  # -------------------------------------------------------------------------
  #
  # The pin is recorded on the node that will act on it — replication is a
  # destination pull (ADR-0030), so node B's convergence is what turns the pin
  # into a transfer. The route is the generic placement route, which (unlike the
  # self-only pin the vault upload records) takes any peer id the device names.
  local pin
  pin=$(vp_b "/api/v1/vault/placements" -X POST -H 'Content-Type: application/json' \
    -d "{\"blob_hash\":\"$VP_BLOB\",\"peer_id\":\"$pid_b\"}")
  assert_eq "$(jq -r '.blob_hash' <<<"$pin")" "$VP_BLOB" \
    "the placement pin was recorded for the pinned blob"
  assert_eq "$(jq -r '.peer_id' <<<"$pin")" "$pid_b" \
    "and names the peer it should live on"

  # Now make node B a peer that does NOT hold the blob and has no asset for it:
  # delete its asset (so the canonical diff cannot desire it) and its bytes (so a
  # real transfer is needed), and report the loss so the fabric knows. From here,
  # the ONLY thing marking the blob desired on B is the pin.
  local bid
  for bid in $(vp_b /api/v1/assets | jq -r --arg h "$VP_BLOB" '.items[] | select(.blob_hash == $h) | .id'); do
    vp_b "/api/v1/assets/$bid" -X DELETE -o /dev/null
  done
  find "$root/b/data/cas/blobs" -name "${VP_BLOB#blake3:}" -type f -delete
  $vcli_b peers report-inventory site-b --json >/dev/null
  $vcli_b peers report-inventory site-a --json >/dev/null
  # Node A tells node B's controller that A holds the bytes, so B has a source.
  $vcli_a peers report-inventory site-b --json >/dev/null
  assert_eq "$(peer_holds "$root/b/data/cas" "$VP_BLOB")" "0" \
    "node B holds none of the blob, and no asset names it — only the pin marks it desired there"

  # -------------------------------------------------------------------------
  note "  the pin becomes a transfer: node B pulls the vault blob (convergence union)"
  # -------------------------------------------------------------------------
  vp_b "/api/v1/peers/site-b/reconcile" -X POST -o /dev/null
  wait_for "the pinned vault blob never reached node B — the convergence union did not turn the pin into a transfer" \
    900 vp_b_replicated
  assert_eq "$(peer_holds "$root/b/data/cas" "$VP_BLOB")" "1" \
    "the blob crossed the wire to node B because a pin, not an asset, marked it desired there"

  # Node B tells node A it now holds the bytes, so A has a durable second copy on
  # record — which the reverse-retention step below relies on: ADR-0018 will not
  # let A delete a last copy it cannot see elsewhere, and here it can.
  $vcli_b peers report-inventory site-a --json >/dev/null

  # -------------------------------------------------------------------------
  note "  and node A's garbage collector SPARES the pinned, asset-less vault blob (retention)"
  # -------------------------------------------------------------------------
  #
  # Node A's asset for the fixture is deleted, so nothing in the catalogue
  # references the bytes any more — a vault blob's real condition, since it never
  # had an asset. Node A's self-pin from the vault upload is all that keeps it.
  local aid
  for aid in $(vp_a /api/v1/assets | jq -r --arg h "$VP_BLOB" '.items[] | select(.blob_hash == $h) | .id'); do
    vp_a "/api/v1/assets/$aid" -X DELETE -o /dev/null
  done
  assert_eq "$(vp_a /api/v1/assets | jq -r --arg h "$VP_BLOB" '[.items[] | select(.blob_hash == $h)] | length')" "0" \
    "no asset references the vault blob on node A — by the first milestones' measure it is garbage"
  assert_eq "$(peer_holds "$root/a/data/cas" "$VP_BLOB")" "1" "and node A still holds the bytes"

  # Two passes at a one-nanosecond grace: a pinned blob is counted referenced, so
  # it is never even marked, let alone reclaimed.
  local gc1 gc2
  gc1=$("$BIN" --config "$cfg_a" gc --apply --grace 1ns --json 2>/dev/null)
  assert_eq "$(jq -r --arg h "$VP_BLOB" '[.marked[].hash] | index($h) != null' <<<"$gc1")" "false" \
    "the pinned vault blob is not even marked — a placement pin is a reference (ADR-0096)"
  gc2=$("$BIN" --config "$cfg_a" gc --apply --grace 1ns --json 2>/dev/null)
  assert_eq "$(jq -r --arg h "$VP_BLOB" '[.reclaimed[].hash] | index($h) != null' <<<"$gc2")" "false" \
    "and never reclaimed by the sweep that would reclaim any unpinned, unreferenced blob"
  assert_eq "$(peer_holds "$root/a/data/cas" "$VP_BLOB")" "1" \
    "node A kept every byte of the pinned vault blob"

  # Remove node A's self-pin — the device saying the blob may go — and the very
  # same sweep reclaims it: the pin was the whole of its retention (ADR-0096).
  vp_a /api/v1/vault/placements -X DELETE -H 'Content-Type: application/json' \
    -d "{\"blob_hash\":\"$VP_BLOB\",\"peer_id\":\"$self_a\"}" -o /dev/null
  "$BIN" --config "$cfg_a" gc --apply --grace 1ns --json >/dev/null 2>&1
  "$BIN" --config "$cfg_a" gc --apply --grace 1ns --json >/dev/null 2>&1
  assert_eq "$(peer_holds "$root/a/data/cas" "$VP_BLOB")" "0" \
    "with its last pin gone the blob is reclaimable again — the pin was the whole of its retention"

  local p
  for p in "${PEER_PIDS[@]:-}"; do kill -TERM "$p" 2>/dev/null || true; done
  for p in "${PEER_PIDS[@]:-}"; do wait "$p" 2>/dev/null || true; done
  PEER_PIDS=()
}

# vp_b_replicated — 0 once node B has a replicate_blob job in `succeeded`, the last
# of the things the transfer produces (#207). It reads globals the demo set so
# wait_for can call it by name.
vp_b_replicated() {
  [[ "$(curl -sS --unix-socket "$VP_SOCK_B" -H "Authorization: Bearer $VP_TOKEN_B" \
    "http://heyarr/api/v1/jobs?type=replicate_blob" | jq -r '[.items[] | select(.state == "succeeded")] | length > 0')" == "true" ]]
}

