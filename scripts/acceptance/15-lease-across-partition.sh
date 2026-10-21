# shellcheck shell=bash
# A section of scripts/acceptance.sh: sourced by it, in order, never run alone.
# ---------------------------------------------------------------------------
# A LEASE IS HONOURED ACROSS A PARTITION (ADR-0048 amendment, #285)
# ---------------------------------------------------------------------------
#
# ADR-0048's acceptance sentence, as amended: with site A unreachable, a WRITE at
# site B succeeds on a lease B cached from A, and the same write without a lease
# is refused (403). Read is the floor (ADR-0067) and needs no lease, so the
# assertion is about write.
#
# The pieces were built and Go-tested in three PRs (the issuing beat, the
# sibling fetch beat, the request-path lift). This is the caller that proves
# they meet in a running binary:
#
#   TWO `heyarr controller` NODES, AUTH ON, ENROLLED BOTH WAYS. One user pinned
#   at both, two of its devices (D and E) enrolled at both. Neither device has a
#   management grant at B, so at B both sit on the read floor.
#
#   A GRANTS D WRITE; B OPTS INTO A. `heyarr admin management grant` at A, and
#   `peer.lease_issuers: [<B's id for A>]` at B. Nothing at B names D.
#
#   A ISSUES, B FETCHES — ON THE BEATS' STARTUP PASSES. Both beats run every
#   five minutes (a constant, not a setting) and once at startup. The grant is
#   a host-side act on a stopped A and the opt-in is read at start, so both
#   nodes restart anyway; their startup passes are the trigger. No interval
#   knob exists and none is added for the demo: a demo that special-cased the
#   cadence would stop proving the cadence anybody runs (the same reasoning as
#   the polled-acquisition arc).
#
#   A IS STOPPED. Then, at B: D writes (201) and the access log says it was the
#   lease (`write_via=lease`, `lease_issuer` = A's key) and the lease-check
#   metric counts a `lifted`; E, the same user with no grant at A, gets 403 on
#   the same write and 200 on a read.
#
#   EXPIRY IS NOT ASKED HERE. A cached lease stops counting after its 24h TTL,
#   and a shell demo on a wall clock cannot reach that without an injected
#   clock. It is recorded not_exercised, naming the Go tests that do assert it,
#   so this scene does not read as covering expiry (#187).
lease_partition_demo() {
  local root="$WORK/leasepartition"
  mkdir -p "$root/a" "$root/b" "$root/client"

  # Fixed peer ports: B's membership records A's endpoint, and A is restarted
  # between the two phases, so it must come back where B enrolled it.
  local port_a port_b
  read -r port_a port_b < <(python3 -c 'import socket
s=[socket.socket() for _ in range(2)]
[x.bind(("127.0.0.1",0)) for x in s]
print(*[x.getsockname()[1] for x in s])')

  lh_config() { # node port [extra peer: lines]
    cat > "$root/$1.yaml" <<YAML
data_dir: $root/$1/data
peer:
  name: lease-$1
  site: lease-$1
  listen: 127.0.0.1:$2
${3:-}
log:
  level: info
  format: json
# Auth ON: the Device scheme, and so the lease lift, is only reached when a
# credential is required (see the device-auth section).
http:
  addr: ""
  unix_socket: $root/$1/data/heyarr.sock
  auth:
    enabled: true
YAML
  }
  lh_config a "$port_a"
  lh_config b "$port_b"
  local cfg_a="$root/a.yaml" cfg_b="$root/b.yaml"
  local sock_a="$root/a/data/heyarr.sock" sock_b="$root/b/data/heyarr.sock"
  local log_a="$root/a.log" log_b="$root/b.log"
  LH_LOG_A="$log_a"; LH_LOG_B="$log_b"

  local token_a token_b
  token_a=$("$BIN" --config "$cfg_a" token create acceptance --scopes admin --json | jq -r .token)
  token_b=$("$BIN" --config "$cfg_b" token create acceptance --scopes admin --json | jq -r .token)

  lh_start() { # node → sets LH_PID
    "$BIN" --config "$root/$1.yaml" controller >>"$root/$1.log" 2>&1 &
    LH_PID=$!
    PEER_PIDS+=("$LH_PID")
    local s="$root/$1/data/heyarr.sock" waited=0
    while (( waited < 600 )); do
      curl -sf --unix-socket "$s" http://heyarr/readyz >/dev/null 2>&1 && return 0
      sleep 0.1; waited=$(( waited + 1 ))
    done
    fail "lease-partition: node $1 never became ready"; tail -20 "$root/$1.log"; return 1
  }
  local pid_a pid_b
  lh_start a || return 0; pid_a=$LH_PID
  lh_start b || return 0; pid_b=$LH_PID

  lh_a() { curl -sS --unix-socket "$sock_a" -H "Authorization: Bearer $token_a" "${@:2}" "http://heyarr$1"; }
  lh_b() { curl -sS --unix-socket "$sock_b" -H "Authorization: Bearer $token_b" "${@:2}" "http://heyarr$1"; }
  local cli_a="$BIN --config $cfg_a --token $token_a" cli_b="$BIN --config $cfg_b --token $token_b"

  # -------------------------------------------------------------------------
  note "  two auth-enabled sites, enrolled both ways; one user, two devices, pinned and enrolled at both"
  # -------------------------------------------------------------------------
  local key_a key_b
  key_a=$($cli_a peers list --json | jq -r '.[] | select(.is_self) | .public_key')
  key_b=$($cli_b peers list --json | jq -r '.[] | select(.is_self) | .public_key')
  $cli_a peers add --name lease-b --site lease-b --mode full \
    --public-key "$key_b" --endpoint "https://127.0.0.1:$port_b" --json >/dev/null 2>&1
  $cli_b peers add --name lease-a --site lease-a --mode full \
    --public-key "$key_a" --endpoint "https://127.0.0.1:$port_a" --json >/dev/null 2>&1
  $cli_a peers add --name lease-a --public-key "$key_a" --endpoint "https://127.0.0.1:$port_a" --json >/dev/null 2>&1
  $cli_b peers add --name lease-b --public-key "$key_b" --endpoint "https://127.0.0.1:$port_b" --json >/dev/null 2>&1

  # Peer ids are node-local (ADR-0012): peer.lease_issuers names A by B's id for
  # it, as `peers list` at B shows it.
  local a_at_b
  a_at_b=$($cli_b peers list --json | jq -r '.[] | select(.name == "lease-a") | .id')
  assert_eq "$(( ${#a_at_b} > 0 ))" "1" "site B has an id for site A to opt into"

  # The person's machine: one user identity, two device keys (D and E), each
  # with a user-signed cert. Client keys live under $WORK (ADR-0032).
  local idd="$root/client/identity" ddir="$root/client/dev-d" edir="$root/client/dev-e"
  local cl_d=( env "VOID_WHICH_BINDS_IDENTITY_DIR=$idd" "VOID_WHICH_BINDS_DEVICE_DIR=$ddir" "$BIN" )
  local cl_e=( env "VOID_WHICH_BINDS_IDENTITY_DIR=$idd" "VOID_WHICH_BINDS_DEVICE_DIR=$edir" "$BIN" )
  "${cl_d[@]}" identity generate --name owner >/dev/null 2>&1
  "${cl_d[@]}" device generate --name phone-d >/dev/null 2>&1
  "${cl_e[@]}" device generate --name tablet-e >/dev/null 2>&1
  "${cl_d[@]}" identity enrol >/dev/null 2>&1
  "${cl_e[@]}" identity enrol >/dev/null 2>&1
  local user_key d_key e_key d_cert e_cert
  user_key=$("${cl_d[@]}" identity show --json | jq -r .public_key)
  d_key=$("${cl_d[@]}" device show --json | jq -r .public_key)
  e_key=$("${cl_e[@]}" device show --json | jq -r .public_key)
  d_cert=$("${cl_d[@]}" identity credential | cut -d'~' -f1)
  e_cert=$("${cl_e[@]}" identity credential | cut -d'~' -f1)

  # Pinned and enrolled through the identities API, as the device-auth section
  # does. A pin is per controller (ADR-0038, ADR-0067: the user pin stays the
  # root of trust, and a lease never attests one), so the user is pinned at
  # BOTH sites by hand.
  local user_body d_body e_body code site label
  user_body="{\"public_key\":\"$user_key\",\"name\":\"owner\"}"
  d_body="{\"cert\":\"$d_cert\",\"name\":\"phone-d\"}"
  e_body="{\"cert\":\"$e_cert\",\"name\":\"tablet-e\"}"
  for site in a b; do
    label=$(printf '%s' "$site" | tr a-z A-Z)
    code=$("lh_$site" /api/v1/identities/users -X POST -H 'Content-Type: application/json' \
      -d "$user_body" -o /dev/null -w '%{http_code}')
    assert_eq "$code" "201" "the user is pinned at site $label"
    code=$("lh_$site" /api/v1/identities/devices -X POST -H 'Content-Type: application/json' \
      -d "$d_body" -o /dev/null -w '%{http_code}')
    assert_eq "$code" "201" "device D is enrolled at site $label"
    code=$("lh_$site" /api/v1/identities/devices -X POST -H 'Content-Type: application/json' \
      -d "$e_body" -o /dev/null -w '%{http_code}')
    assert_eq "$code" "201" "device E is enrolled at site $label"
  done

  # The want both devices will try to record at B names a quality profile, so
  # B's operator makes one. Its creation is the admin's write, not a device's.
  code=$(lh_b /api/v1/quality-profiles -X POST -H 'Content-Type: application/json' \
    -d '{"name":"lease-anything","description":"accepts whatever exists"}' -o /dev/null -w '%{http_code}')
  assert_eq "$code" "201" "site B's operator records a quality profile for the wants below"

  # lh_device_status METHOD PATH CLIENT-DIR [BODY] — a request to B under a
  # FRESH Device credential (a possession proof is minted per request), status
  # alone.
  lh_device_status() {
    local dev cred body=${4:-}
    dev=( env "VOID_WHICH_BINDS_IDENTITY_DIR=$idd" "VOID_WHICH_BINDS_DEVICE_DIR=$3" "$BIN" )
    cred=$("${dev[@]}" identity credential)
    if [[ -n "$body" ]]; then
      curl -sS --unix-socket "$sock_b" -H "Authorization: Device $cred" -X "$1" \
        -H 'Content-Type: application/json' -d "$body" -o /dev/null -w '%{http_code}' "http://heyarr$2"
    else
      curl -sS --unix-socket "$sock_b" -H "Authorization: Device $cred" -X "$1" \
        -o /dev/null -w '%{http_code}' "http://heyarr$2"
    fi
  }
  local want_d='{"work":{"content_type":"movie","title":"Lantern Coast","year":2017},"quality_profile":"lease-anything","reason":"asked for at B while A is down"}'
  local want_e='{"work":{"content_type":"movie","title":"Quiet Meridian","year":2015},"quality_profile":"lease-anything","reason":"asked for at B with no lease"}'

  # The baseline the lift is measured against: before any grant or opt-in, D is
  # on B's read floor. Without this, a 201 later could be write D always had.
  code=$(lh_device_status GET /api/v1/desired "$ddir")
  assert_eq "$code" "200" "before any lease, device D reads at site B (the read floor, ADR-0067)"
  code=$(lh_device_status POST /api/v1/desired "$ddir" "$want_d")
  assert_eq "$code" "403" "and its write at site B is refused: nothing at B authorises it"

  # -------------------------------------------------------------------------
  note "  site A grants D write on the host; site B opts into A as a lease issuer"
  # -------------------------------------------------------------------------
  stop_peer_node "$pid_a" "$pid_b"
  local granted
  granted=$("$BIN" --config "$cfg_a" admin management grant "$d_key" \
    --reason "the lease-partition scene: D may write at A's siblings" --json | jq -r .device_key)
  assert_eq "$granted" "$d_key" "heyarr admin management grant authorises device D for write at site A (ADR-0065)"
  lh_config b "$port_b" "  lease_issuers: [\"$a_at_b\"]"

  # A's issuing beat runs its startup pass before the node is ready; B's fetch
  # beat runs its startup pass in the background, so B is waited on below.
  lh_start a || return 0; pid_a=$LH_PID
  wait_for "site A never issued a device write lease on its issuer's startup pass" 100 lh_a_issued
  local issued
  issued=$(lh_last_log "$log_a" "device write leases converged" '.issued')
  assert_eq "$issued" "1" "site A issued device D one write lease — for its one sibling, site B (ADR-0048 amendment)"

  lh_start b || return 0; pid_b=$LH_PID
  wait_for "site B never cached a lease fetched from site A on its fetch beat's startup pass" 100 lh_b_cached
  local cached
  cached=$(lh_last_log "$log_b" "sibling lease cache updated" '.changed')
  assert_eq "$cached" "1" "site B fetched A's lease for it over the pinned peer link and cached it"

  # -------------------------------------------------------------------------
  note "  THE PARTITION: site A is stopped; site B honours the lease it cached"
  # -------------------------------------------------------------------------
  stop_peer_node "$pid_a"
  local a_up
  a_up=$(curl -sk --max-time 2 -o /dev/null -w '%{http_code}' "https://127.0.0.1:$port_a/" 2>/dev/null || true)
  assert_eq "$a_up" "000" "site A's peer surface answers nothing: the issuer is unreachable"

  code=$(lh_device_status POST /api/v1/desired "$ddir" "$want_d")
  assert_eq "$code" "201" \
    "with site A stopped, a write at site B by device D succeeds on the lease cached from A"

  # The access log says WHY it was allowed, and by whose lease. Read from B's own
  # log line for that request, not inferred from the 201.
  local line via issuer_logged
  line=$({ grep -F '"msg":"http request"' "$log_b" || true; } |
    jq -c 'select(.method == "POST" and .path == "/api/v1/desired" and .status == 201)' | tail -1)
  [[ -n "$line" ]] || line='{}'
  via=$(jq -r '.write_via // "absent"' <<<"$line")
  assert_eq "$via" "lease" "site B's access log marks that write write_via=lease"
  issuer_logged=$(jq -r '.lease_issuer // "absent"' <<<"$line")
  assert_eq "$issuer_logged" "$key_a" "and names site A's key as the lease_issuer"

  local metrics lifted
  metrics=$(lh_b /metrics)
  lifted=$(lh_metric "$metrics" 'heyarr_http_device_lease_checks_total{outcome="lifted"}')
  assert_eq "$(( lifted >= 1 ))" "1" \
    "heyarr_http_device_lease_checks_total{outcome=\"lifted\"} counts the lift at site B"

  # -------------------------------------------------------------------------
  note "  the same user's other device, with no grant at A and so no lease, keeps only the floor"
  # -------------------------------------------------------------------------
  code=$(lh_device_status POST /api/v1/desired "$edir" "$want_e")
  assert_eq "$code" "403" \
    "the same write at site B by device E, which holds no lease, is refused (403)"
  code=$(lh_device_status GET /api/v1/desired "$edir")
  assert_eq "$code" "200" "while device E still reads at site B: the read floor needs no lease"
  # E's check is not `no_lease`: B's cache DOES hold a lease, D's, and the
  # refusal names the lease that got furthest. D's lease cannot be ridden by
  # another device of the same user (ADR-0048 amendment, decision 2), and the
  # metric says so by name rather than as an error.
  local mismatch
  metrics=$(lh_b /metrics)
  mismatch=$(lh_metric "$metrics" 'heyarr_http_device_lease_checks_total{outcome="refused_principal_mismatch"}')
  assert_eq "$(( mismatch >= 1 ))" "1" \
    "and the lease check names why: the only cached lease is D's, refused for E as principal_mismatch"

  not_exercised injected_clock \
    "a cached lease refused once it expires (24h TTL; the refusal names expired) — a wall-clock shell demo cannot reach it, so it is asserted in Go with an injected clock: internal/api/http/leaselift_test.go TestEachLeaseRefusalKeepsTheFloorAndIsNamed and internal/leases/lift_test.go TestLiftDeviceWriteNamesEachRefusal (claim lease-honoured-across-partition does not cover expiry)"

  stop_peer_node "$pid_b"
}

# lh_last_log LOGFILE MSG JQ-FIELD — a field of the last JSON log line with
# this exact message, or "none".
lh_last_log() {
  local v
  v=$({ grep -F "\"msg\":\"$2\"" "$1" 2>/dev/null || true; } | tail -1 | jq -r "$3 // \"none\"" 2>/dev/null)
  printf '%s' "${v:-none}"
}

lh_a_issued() { [[ "$(lh_last_log "$LH_LOG_A" "device write leases converged" '.issued')" != "none" ]]; }
lh_b_cached() { [[ "$(lh_last_log "$LH_LOG_B" "sibling lease cache updated" '.changed')" != "none" ]]; }

# lh_metric METRICS-TEXT SERIES — the sample value of one exact series, as an
# integer (0 when absent). Matched with grep -F: the braces and quotes are
# literal.
lh_metric() {
  local row
  row=$({ grep -F "$2 " <<<"$1" || true; } | tail -1)
  awk '{ printf "%d", $NF + 0 }' <<<"${row:-x 0}"
}
