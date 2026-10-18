# shellcheck shell=bash
# A section of scripts/acceptance.sh: sourced by it, in order, never run alone.
# ---------------------------------------------------------------------------
# A VAULT BLOB UPLOADED AT ONE SITE IS KEPT AT THE OTHER (ADR-0114, #662)
# ---------------------------------------------------------------------------
#
# The vault-placement scene before this one proves a pin pulls a blob — but it
# records the pin on the node that pulls, and both of its nodes ingest the same
# library, so the destination already knew the blob. Neither holds in a real
# two-site vault: a device uploads to the site it talks to, and the other site
# has never heard of the bytes. This scene has none of that help:
#
#   TWO `mnemosyne all` NODES, NO SHARED INGEST. A ciphertext blob is uploaded to
#   site A only. Site B has no row for it, no pin naming it, no library.
#
#   THE DESTINATION ASKS. Each lists the other in placement.siblings. B asks A
#   which blobs A wants kept at B (POST /peer/v1/placements), takes the upload's
#   placement, and pulls it — in B's own queue. A plans no cross-site transfer.
#
#   THE CLAIM COMES BACK. B's next question carries what it now holds, so A can
#   say the blob is at two sites (GET /vault/blobs/{hash}/placement).
#
#   AND GOES AWAY. Unpinned at A, the blob is withdrawn from B at B's next
#   question, and B's GC may then reclaim it.
sibling_placement_demo() {
  local root="$WORK/siblingplacement" mbin=${MNEMOSYNE_BIN:-./bin/mnemosyne}
  if [[ ! -x "$mbin" ]]; then
    fail "sibling-placement: no mnemosyne binary at $mbin — run 'make build', or set MNEMOSYNE_BIN"
    return 0
  fi

  # Fixed peer ports, so a node restarted with its siblings configured is still
  # where the other one enrolled it.
  local port_a port_b
  read -r port_a port_b < <(python3 -c 'import socket
s=[socket.socket() for _ in range(2)]
[x.bind(("127.0.0.1",0)) for x in s]
print(*[x.getsockname()[1] for x in s])')

  sp_config() { # node port siblings-yaml
    cat > "$root/$1.yaml" <<YAML
data_dir: $root/$1/data
peer:
  name: site-$1
  site: site-$1
  listen: 127.0.0.1:$2
log:
  level: info
  format: json
http:
  addr: ""
placement:
  interval: 1s
$3
YAML
  }
  mkdir -p "$root/a" "$root/b"
  sp_config a "$port_a" ""
  sp_config b "$port_b" ""
  local cfg_a="$root/a.yaml" cfg_b="$root/b.yaml"
  local sock_a="$root/a/data/mnemosyne.sock" sock_b="$root/b/data/mnemosyne.sock"
  local log_a="$root/a.log" log_b="$root/b.log"
  local token_a token_b
  token_a=$("$mbin" --config "$cfg_a" token create acceptance --scopes admin --json | jq -r .token)
  token_b=$("$mbin" --config "$cfg_b" token create acceptance --scopes admin --json | jq -r .token)
  SP_SOCK_A="$sock_a"; SP_TOKEN_A="$token_a"; SP_SOCK_B="$sock_b"; SP_TOKEN_B="$token_b"; SP_CAS_B="$root/b/data/cas"

  sp_a() { curl -sS --unix-socket "$sock_a" -H "Authorization: Bearer $token_a" "${@:2}" "http://mnemosyne$1"; }
  sp_b() { curl -sS --unix-socket "$sock_b" -H "Authorization: Bearer $token_b" "${@:2}" "http://mnemosyne$1"; }
  local cli_a="$mbin --config $cfg_a --token $token_a" cli_b="$mbin --config $cfg_b --token $token_b"

  local pids_a pids_b
  sp_start() { # node → sets SP_PIDS
    "$mbin" --config "$root/$1.yaml" all >>"$root/$1.log" 2>&1 &
    SP_PIDS=$!
    PEER_PIDS+=("$SP_PIDS")
    local s="$root/$1/data/mnemosyne.sock" waited=0
    while (( waited < 600 )); do
      curl -sf --unix-socket "$s" http://mnemosyne/readyz >/dev/null 2>&1 && return 0
      sleep 0.1; waited=$(( waited + 1 ))
    done
    fail "sibling-placement: node $1 never became ready"; tail -20 "$root/$1.log"; return 1
  }
  sp_start a || return 0; pids_a=$SP_PIDS
  sp_start b || return 0; pids_b=$SP_PIDS

  # -------------------------------------------------------------------------
  note "  two Mnemosyne sites, enrolled both ways, each naming the other a sibling"
  # -------------------------------------------------------------------------
  local key_a key_b
  key_a=$($cli_a peers list --json | jq -r '.[] | select(.is_self) | .public_key')
  key_b=$($cli_b peers list --json | jq -r '.[] | select(.is_self) | .public_key')
  $cli_a peers add --name site-b --site site-b --mode full \
    --public-key "$key_b" --endpoint "https://127.0.0.1:$port_b" --json >/dev/null 2>&1
  $cli_b peers add --name site-a --site site-a --mode full \
    --public-key "$key_a" --endpoint "https://127.0.0.1:$port_a" --json >/dev/null 2>&1
  $cli_a peers add --name site-a --public-key "$key_a" --endpoint "https://127.0.0.1:$port_a" --json >/dev/null 2>&1
  $cli_b peers add --name site-b --public-key "$key_b" --endpoint "https://127.0.0.1:$port_b" --json >/dev/null 2>&1

  # Peer ids are node-local (ADR-0012): each site names the other by ITS OWN id
  # for it, and the two are not the same string.
  local b_at_a a_at_b self_a
  b_at_a=$($cli_a peers list --json | jq -r '.[] | select(.name == "site-b") | .id')
  a_at_b=$($cli_b peers list --json | jq -r '.[] | select(.name == "site-a") | .id')
  self_a=$($cli_a peers list --json | jq -r '.[] | select(.is_self) | .id')
  assert_eq "$(( ${#b_at_a} > 0 && ${#a_at_b} > 0 ))" "1" "each site has an id for the other"

  stop_peer_node "$pids_a" "$pids_b"
  sp_config a "$port_a" "  siblings: [\"$b_at_a\"]"
  sp_config b "$port_b" "  siblings: [\"$a_at_b\"]"
  sp_start a || return 0; pids_a=$SP_PIDS
  sp_start b || return 0; pids_b=$SP_PIDS

  # -------------------------------------------------------------------------
  note "  a vault blob is uploaded to site A only; site B has never heard of it"
  # -------------------------------------------------------------------------
  local blobfile="$root/blob.ct" blob up
  head -c 262144 /dev/urandom > "$blobfile"
  blob=$("$GEN" -hash "$blobfile")
  SP_BLOB="$blob"
  up=$(sp_a "/api/v1/vault/blobs/$blob" -X PUT -H 'Content-Type: application/octet-stream' --data-binary @"$blobfile")
  assert_eq "$(jq -r '.hash' <<<"$up")" "$blob" "site A stored the ciphertext and pinned it to itself"
  assert_eq "$(peer_holds "$root/b/data/cas" "$blob")" "0" "site B holds none of it"

  # -------------------------------------------------------------------------
  note "  site B asks site A, takes the placement, and pulls it in its own queue"
  # -------------------------------------------------------------------------
  wait_for "site B never pulled the blob uploaded at site A — it did not ask, or did not plan the pull" \
    300 sp_b_pulled
  assert_eq "$(peer_holds "$root/b/data/cas" "$blob")" "1" \
    "the blob crossed to site B with no shared ingest and no pin recorded at B"
  assert_eq "$(sp_a "/api/v1/jobs?type=replicate_blob" | jq -r '.items | length')" "0" \
    "site A planned no cross-site transfer: the destination's queue ran it (ADR-0030, ADR-0114)"

  # -------------------------------------------------------------------------
  note "  site B's next question says it holds the blob; site A reports two sites"
  # -------------------------------------------------------------------------
  wait_for "site A never learned the blob is at site B" 100 sp_a_durable_two
  local st
  st=$(sp_a "/api/v1/vault/blobs/$blob/placement")
  assert_eq "$(jq -r '.sites_durable' <<<"$st")" "2" "the blob is durable at both sites"
  assert_eq "$(jq -r --arg b "$b_at_a" '.pinned_to | index($b) != null' <<<"$st")" "true" \
    "and site A says it is wanted at its sibling"

  # -------------------------------------------------------------------------
  note "  unpinned at site A, the placement is withdrawn from site B, and B's GC may take it"
  # -------------------------------------------------------------------------
  sp_a /api/v1/vault/placements -X DELETE -H 'Content-Type: application/json' \
    -d "{\"blob_hash\":\"$blob\",\"peer_id\":\"$self_a\"}" -o /dev/null
  wait_for "site B kept the placement after site A withdrew it" 100 sp_b_withdrawn
  "$mbin" --config "$cfg_b" gc --apply --grace 1ns --json >/dev/null 2>&1
  "$mbin" --config "$cfg_b" gc --apply --grace 1ns --json >/dev/null 2>&1
  assert_eq "$(peer_holds "$root/b/data/cas" "$blob")" "0" \
    "with the placement withdrawn, site B's GC reclaimed its copy (grace window, ADR-0018)"

  stop_peer_node "$pids_a" "$pids_b"
}

# sp_b_pulled — 0 once site B holds the blob. Nudges B's reconcile each poll, so
# the scene waits on the question (a one-second beat), not on the five-minute
# reconcile cadence.
sp_b_pulled() {
  curl -sS --unix-socket "$SP_SOCK_B" -H "Authorization: Bearer $SP_TOKEN_B" \
    -X POST "http://mnemosyne/api/v1/peers/site-b/reconcile" -o /dev/null 2>/dev/null || true
  [[ "$(peer_holds "$SP_CAS_B" "$SP_BLOB")" == "1" ]]
}

sp_a_durable_two() {
  [[ "$(curl -sS --unix-socket "$SP_SOCK_A" -H "Authorization: Bearer $SP_TOKEN_A" \
    "http://mnemosyne/api/v1/vault/blobs/$SP_BLOB/placement" | jq -r '.sites_durable')" == "2" ]]
}

sp_b_withdrawn() {
  [[ "$(curl -sS --unix-socket "$SP_SOCK_B" -H "Authorization: Bearer $SP_TOKEN_B" \
    "http://mnemosyne/api/v1/vault/blobs/$SP_BLOB/placement" | jq -r '.taken_from | length')" == "0" ]]
}
