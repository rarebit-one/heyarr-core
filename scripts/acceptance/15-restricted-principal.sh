# shellcheck shell=bash
# A section of scripts/acceptance.sh: sourced by it, in order, never run alone.
# RESTRICTED PRINCIPALS (ADR-0104): an executor token reaches the vault surface
# and the spaces an owner's device granted it, and nothing else. The owner's
# device grants and revokes; a bearer token — even an admin one — cannot. The
# ordinary admin token keeps every route it had, which is the regression line.
restricted_principal_demo() {
  local root="$WORK/restricted" data sock cfg dev
  data="$root/data"; sock="$data/heyarr.sock"; cfg="$WORK/restricted.yaml"
  dev="$root/dev"
  mkdir -p "$data"

  cat > "$cfg" <<YAML
data_dir: $data
peer:
  name: acceptance-restricted
  site: test
log:
  level: info
  format: json
http:
  addr: ""
  unix_socket: $sock
  auth:
    enabled: true
YAML

  local admin
  admin=$("$BIN" --config "$cfg" token create acceptance --scopes admin --json | jq -r .token)
  "$BIN" --config "$cfg" controller >"$root/controller.log" 2>&1 &
  local pid=$!
  local waited=0
  while (( waited < 600 )); do
    curl -sf --unix-socket "$sock" http://heyarr/readyz >/dev/null 2>&1 && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if (( waited >= 600 )); then
    fail "the restricted-principal node never became ready"; cat "$root/controller.log"
    kill -KILL "$pid" 2>/dev/null || true; return 1
  fi

  # The owner's device: a user identity and a device key, the user pinned and
  # the device enrolled, then authorised for write by the operator (ADR-0065) —
  # the only credential that may consent to a grant.
  local cl user_key device_key cert
  cl=( env "VOID_WHICH_BINDS_IDENTITY_DIR=$dev/id" "VOID_WHICH_BINDS_DEVICE_DIR=$dev/dev" "$BIN" )
  "${cl[@]}" identity generate --name owner >/dev/null 2>&1
  "${cl[@]}" device generate --name laptop >/dev/null 2>&1
  "${cl[@]}" identity enrol >/dev/null 2>&1
  user_key=$("${cl[@]}" identity show --json | jq -r .public_key)
  device_key=$("${cl[@]}" device show --json | jq -r .public_key)
  cert=$("${cl[@]}" identity credential | cut -d'~' -f1)
  rp_admin() { curl -sS --unix-socket "$sock" -H "Authorization: Bearer $admin" -X POST \
    -H 'Content-Type: application/json' -d "$2" -o /dev/null "http://heyarr$1"; }
  rp_admin /api/v1/identities/users "{\"public_key\":\"$user_key\",\"name\":\"owner\"}"
  rp_admin /api/v1/identities/devices "{\"cert\":\"$cert\",\"name\":\"laptop\"}"
  rp_admin /api/v1/session/management-grants "{\"device_key\":\"$device_key\"}"

  # rp_status METHOD PATH AUTH [BODY] prints the HTTP status alone.
  rp_status() {
    local body=${4:-}
    if [[ -n "$body" ]]; then
      curl -sS --unix-socket "$sock" -H "Authorization: $3" -X "$1" -H 'Content-Type: application/json' \
        -d "$body" -o /dev/null -w '%{http_code}' "http://heyarr$2"
    else
      curl -sS --unix-socket "$sock" -H "Authorization: $3" -X "$1" -o /dev/null -w '%{http_code}' "http://heyarr$2"
    fi
  }
  # rp_body AUTH PATH prints the response body of a GET.
  rp_body() { curl -sS --unix-socket "$sock" -H "Authorization: $1" "http://heyarr$2"; }
  rp_device() { printf 'Device %s' "$("${cl[@]}" identity credential)"; }

  # Every value is assigned BEFORE it is asserted: macOS's bash 3.2 mangles a
  # command substitution with nested quotes inside another quoted argument
  # (see 13-m8-device-identity.sh).
  local space code got devcred
  space=$(python3 -c 'import uuid; print(uuid.uuid4())')
  devcred=$(rp_device)
  code=$(rp_status POST /api/v1/spaces "$devcred" "{\"id\":\"$space\",\"kind\":\"family\"}")
  assert_eq "$code" "201" "the owner's device records a space"

  local exa exb restricted
  exa=$("$BIN" --config "$cfg" token create executor-a --executor --json)
  restricted=$(jq -r .restricted <<<"$exa")
  assert_eq "$restricted" "true" "token create --executor mints a restricted token"
  exa="Bearer $(jq -r .token <<<"$exa")"
  exb=$("$BIN" --config "$cfg" token create executor-b --executor --json | jq -r .token)
  exb="Bearer $exb"

  # Before any grant the executor sees nothing, and reaches nothing else.
  code=$(rp_status GET "/api/v1/spaces/$space/changes" "$exa")
  assert_eq "$code" "404" "an executor without a grant gets 404 on the space — as if it did not exist"
  got=$(rp_body "$exa" /api/v1/spaces | jq '.spaces | length')
  assert_eq "$got" "0" "and enumerates no spaces"
  local route
  for route in /api/v1/libraries /api/v1/works /api/v1/system /api/v1/tokens /metrics; do
    code=$(rp_status GET "$route" "$exa")
    assert_eq "$code" "403" "an executor token is refused $route (restricted to the vault surface)"
  done
  code=$(rp_status POST /api/v1/mcp "$exa" '{"jsonrpc":"2.0","id":1,"method":"tools/list"}')
  assert_eq "$code" "403" "an executor token is refused MCP"

  # Consent comes from the device, never a bearer.
  local grant='{"principal":"executor-a","caps":"read,write"}'
  code=$(rp_status POST "/api/v1/spaces/$space/grants" "Bearer $admin" "$grant")
  assert_eq "$code" "403" "an admin bearer token cannot grant an executor access to a space"
  devcred=$(rp_device)
  code=$(rp_status POST "/api/v1/spaces/$space/grants" "$devcred" "$grant")
  assert_eq "$code" "201" "the owner's management-authorised device grants executor-a read,write"

  code=$(rp_status GET "/api/v1/spaces/$space/changes" "$exa")
  assert_eq "$code" "200" "the granted executor fetches the space's ciphertext"
  got=$(rp_body "$exa" /api/v1/spaces | jq -r '.spaces[0].id')
  assert_eq "$got" "$space" "and lists exactly the space it was granted"
  code=$(rp_status GET "/api/v1/spaces/$space/changes" "$exb")
  assert_eq "$code" "404" "a second executor, never granted, still sees nothing"
  code=$(rp_status POST "/api/v1/spaces/$space/rotate" "$exa" '{}')
  assert_eq "$code" "403" "even granted, an executor can never rotate a space key"
  code=$(rp_status DELETE "/api/v1/spaces/$space/keys/x25519:00" "$exa")
  assert_eq "$code" "403" "nor delete a wrapped key"
  code=$(rp_status GET /api/v1/libraries "$exa")
  assert_eq "$code" "403" "and a grant opens nothing outside the vault surface"

  # The regression line: the ordinary admin token is exactly what it was.
  code=$(rp_status GET /api/v1/system "Bearer $admin")
  assert_eq "$code" "200" "the ordinary admin token still reaches /system"
  code=$(rp_status GET "/api/v1/spaces/$space/changes" "Bearer $admin")
  assert_eq "$code" "200" "and every space, with no grant involved"

  # Revocation closes the gate on the next request.
  devcred=$(rp_device)
  code=$(rp_status DELETE "/api/v1/spaces/$space/grants/executor-a" "$devcred")
  assert_eq "$code" "204" "the owner's device revokes the grant"
  code=$(rp_status GET "/api/v1/spaces/$space/changes" "$exa")
  assert_eq "$code" "404" "and the executor's next fetch is refused"

  # The event stream is SSE: read the catch-up and let the one-second bound end it.
  local evs
  evs=$(curl -sS --unix-socket "$sock" -H "Authorization: Bearer $admin" -m 1 \
    "http://heyarr/api/v1/events?after=0&types=personalstate.space.*" 2>/dev/null | grep '^event: ' || true)
  assert_contains "$evs" "personalstate.space.access_granted" "the grant is an event (Invariant 7)"
  assert_contains "$evs" "personalstate.space.access_revoked" "and so is the revocation"

  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}
