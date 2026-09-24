# shellcheck shell=bash
# A section of scripts/acceptance.sh: sourced by it, in order, never run alone.
# THE DEVICE AUTHENTICATES AS ITS USER (§40, ADR-0048, ADR-0032, #303).
#
# M8-02 built the primitives — a user identity, an enrolment cert, a control
# plane that pins them — and left them one step short of a caller. This is that
# step, proved through the real binary: a person self-enrols a device
# client-side, an operator pins the user and enrols the device, and the device
# then authenticates a request with ONLY its key and a user-signed cert. No
# token is issued at any point (the acceptance sentence's first half): the
# identity is the Device Authorization scheme, verified offline against a pinned
# key. The negatives carry as much weight as the positive — no credential is a
# 401, and a revoked device is a 401 — because that is what makes "authenticated"
# mean something rather than "the endpoint answered".
#
# It is a node of its own with auth ENABLED, and that is load-bearing: the
# device scheme is only reached when a bearer token is required, so the
# auth-disabled nodes elsewhere in this file could never exercise it. The client
# keys live under $WORK through the two env overrides that exist precisely so a
# private key never lands in a server data directory (ADR-0032).
device_auth_demo() {
  local root="$WORK/deviceauth" data sock cfg iddir ddir
  data="$root/data"; sock="$data/heyarr.sock"
  cfg="$root/client"; iddir="$cfg/identity"; ddir="$cfg/device"
  mkdir -p "$data" "$cfg"

  cat > "$WORK/deviceauth.yaml" <<YAML
data_dir: $data
peer:
  name: acceptance-deviceauth
  site: test
log:
  level: info
  format: json
# Auth ON, unlike the other single-node sections: the Device scheme is only
# tried when a credential is required, so proving it needs a node that requires
# one. The socket is the transport, so no fixed port collides across runs.
http:
  addr: ""
  unix_socket: $sock
  auth:
    enabled: true
YAML

  # The admin token migrates the database and pins identities. Minted before the
  # server starts, the only order an operator can use (ADR-0011).
  local token
  token=$("$BIN" --config "$WORK/deviceauth.yaml" token create acceptance --scopes admin --json | jq -r .token)

  "$BIN" --config "$WORK/deviceauth.yaml" controller >"$root/controller.log" 2>&1 &
  local pid=$!
  local waited=0
  while (( waited < 600 )); do
    curl -sf --unix-socket "$sock" http://heyarr/readyz >/dev/null 2>&1 && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if (( waited >= 600 )); then
    fail "the device-auth node never became ready"; cat "$root/controller.log"
    kill -KILL "$pid" 2>/dev/null || true; return 1
  fi

  da_api() { curl -sS --unix-socket "$sock" -H "Authorization: Bearer $token" "${@:2}" "http://heyarr$1"; }
  # A device presents its own scheme, no bearer token. Returns just the status.
  da_device() { # credential
    curl -sS --unix-socket "$sock" -H "Authorization: Device $1" \
      -o /dev/null -w '%{http_code}' "http://heyarr/api/v1/libraries"
  }
  local client=( env "VOIDBIND_IDENTITY_DIR=$iddir" "VOIDBIND_DEVICE_DIR=$ddir" "$BIN" )

  # Client-side: a user identity and a device key, and the cert that binds them.
  # Nothing has reached the server yet — this is all on the person's machine.
  "${client[@]}" identity generate --name owner >/dev/null 2>&1
  "${client[@]}" device generate --name phone >/dev/null 2>&1

  # Everything below assigns each value to a plain variable BEFORE asserting on
  # it. macOS ships bash 3.2, whose parser mangles a multiline command
  # substitution that carries nested double quotes when it sits inside another
  # quoted argument — it silently shifts the arguments, and an assert then reads
  # a passing status against the wrong expectation. Assigning first sidesteps
  # that parser entirely, and is easier to read besides.

  # BEFORE enrol: the label is on, and honest.
  local before status unproven
  before=$("${client[@]}" device show --json)
  status=$(jq -r .enrolment_status <<<"$before")
  assert_eq "$status" "not_enrolled" "a fresh device reports not_enrolled before it is enrolled"
  unproven=$(jq -r .unproven <<<"$before")
  assert_eq "$unproven" "true" "and it reports unproven — the ADR-0032 caveat, still true"

  "${client[@]}" identity enrol >/dev/null 2>&1

  # AFTER enrol: the label came off, in the same change that made it untrue —
  # the ADR-0032 revisit, observed at the edge a person actually uses.
  local after user_key device_key
  after=$("${client[@]}" device show --json)
  status=$(jq -r .enrolment_status <<<"$after")
  assert_eq "$status" "enrolled" \
    "the device reports enrolled once it holds a valid cert (ADR-0032 revisit: the label comes off)"
  unproven=$(jq -r .unproven <<<"$after")
  assert_eq "$unproven" "false" \
    "and it is no longer unproven — the caveat came off in the same change, not a milestone later"
  user_key=$("${client[@]}" identity show --json | jq -r .public_key)
  device_key=$(jq -r .public_key <<<"$after")

  # Operator-mediated pinning (ADR-0032's gate): pin the user, then enrol the
  # device by its cert. Nothing the user signed is honoured until this pin — a
  # human act, out of band, not something the device can assert about itself.
  local user_body cert cert_body code cred
  user_body="{\"public_key\":\"$user_key\",\"name\":\"owner\"}"
  code=$(da_api /api/v1/identities/users -X POST -H 'Content-Type: application/json' \
    -d "$user_body" -o /dev/null -w '%{http_code}')
  assert_eq "$code" "201" "an operator pins the user identity"

  cert=$("${client[@]}" identity credential | cut -d'~' -f1)
  cert_body="{\"cert\":\"$cert\",\"name\":\"phone\"}"
  code=$(da_api /api/v1/identities/devices -X POST -H 'Content-Type: application/json' \
    -d "$cert_body" -o /dev/null -w '%{http_code}')
  assert_eq "$code" "201" "and enrols the device by its user-signed cert"

  # THE POSITIVE. The device authenticates as its user over the real middleware
  # chain, presenting only its key and the cert. This is the claims.list
  # evidence, and it is an assertion, not a line of the epilogue.
  cred=$("${client[@]}" identity credential)
  code=$(da_device "$cred")
  assert_eq "$code" "200" \
    "a device authenticates as its user with only a cert and a possession proof — no token issued"

  # NEGATIVE 1: no credential is a 401. Without this the positive proves only
  # that the endpoint answers, not that it authenticated anyone.
  code=$(curl -sS --unix-socket "$sock" -o /dev/null -w '%{http_code}' "http://heyarr/api/v1/libraries")
  assert_eq "$code" "401" \
    "no credential is refused — an unauthenticated request is a 401, not a silent read"

  # NEGATIVE 2: a revoked device is a 401, even holding a once-valid cert. The
  # cert still verifies; the membership no longer does, and the membership is
  # the gate (ADR-0012's revocation shape, per device).
  code=$(da_api "/api/v1/identities/devices/$device_key" -X DELETE -o /dev/null -w '%{http_code}')
  assert_eq "$code" "200" "the operator revokes the device"
  cred=$("${client[@]}" identity credential)
  code=$(da_device "$cred")
  assert_eq "$code" "401" \
    "a revoked device is refused — its once-valid cert authenticates nobody now"

  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

# DEVICE PAIRING: an old device authorises a new one over a dumb relay (§40,
# ADR-0022, ADR-0038, #305).
#
# M8-03 proved a device authenticates from a cert. This is where that cert comes
# from when there is no operator at a keyboard: an already-enrolled OLD device
# authorises a NEW one directly, the server acting only as a dumb store-and-
# forward (ADR-0038). The two exchange public keys and a salt through the relay,
# each derives a short authentication string over BOTH keys, the humans compare
# the two codes, and on a match the old device signs an enrolment cert the new
# device stores. A man-in-the-middle that substitutes a key changes the code, so
# the code is the whole gate — and the demo proves both halves: the honest
# pairing enrols, and a mismatched code enrols nobody.
#
# The relay is on the UNAUTHENTICATED router (ADR-0040): a device being paired
# holds no credential, so this node needs no auth for the relay to work. Client
# keys live under $WORK through the two env overrides that keep a private key out
# of a server data directory (ADR-0032).
pairing_demo() {
  local root="$WORK/pairing" data sock
  data="$root/data"; sock="$data/heyarr.sock"
  mkdir -p "$data"

  cat > "$WORK/pairing.yaml" <<YAML
data_dir: $data
peer:
  name: acceptance-pairing
  site: test
log:
  level: info
  format: json
# The relay grants no authority and carries only public values, so it is served
# without a credential (ADR-0038, ADR-0040); auth off changes nothing about it.
http:
  addr: ""
  unix_socket: $sock
YAML

  "$BIN" --config "$WORK/pairing.yaml" controller >"$root/controller.log" 2>&1 &
  local pid=$!
  local waited=0
  while (( waited < 600 )); do
    curl -sf --unix-socket "$sock" http://heyarr/readyz >/dev/null 2>&1 && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if (( waited >= 600 )); then
    fail "the pairing node never became ready"; cat "$root/controller.log"
    kill -KILL "$pid" 2>/dev/null || true; return 1
  fi

  # The OLD device holds the user identity; the NEW device has only its own key.
  # A SECOND device key stands in for a MITM's substituted key.
  local oldc newc subc
  oldc=( env "VOIDBIND_IDENTITY_DIR=$root/old-id" "VOIDBIND_DEVICE_DIR=$root/old-dev" "$BIN" )
  newc=( env "VOIDBIND_IDENTITY_DIR=$root/new-id" "VOIDBIND_DEVICE_DIR=$root/new-dev" "$BIN" )
  subc=( env "VOIDBIND_IDENTITY_DIR=$root/sub-id" "VOIDBIND_DEVICE_DIR=$root/sub-dev" "$BIN" )
  "${oldc[@]}" identity generate --name owner >/dev/null 2>&1
  "${newc[@]}" device generate --name new-phone >/dev/null 2>&1
  "${subc[@]}" device generate --name attacker >/dev/null 2>&1

  # Each value into a plain variable BEFORE asserting on it — macOS bash 3.2
  # mangles a multiline command substitution carrying nested quotes inside a
  # quoted assert argument, so the assignments stay single-line and simple.
  local user_key new_key sub_key
  user_key=$("${oldc[@]}" identity show --json | jq -r .public_key)
  new_key=$("${newc[@]}" device show --json | jq -r .public_key)
  sub_key=$("${subc[@]}" device show --json | jq -r .public_key)

  # SUBSTITUTION CHANGES THE CODE. The SAS binds BOTH keys, so swapping the
  # responder key for a MITM's yields a different code — the reason the humans
  # compare it at all. Shown with the `pair sas` utility over a fixed salt, no
  # relay needed: two derivations, one honest key and one substitute.
  local salt honest_sas sub_sas sas_cmp
  salt="5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a5a"
  honest_sas=$("${oldc[@]}" pair sas --initiator "$user_key" --responder "$new_key" --salt "$salt")
  sub_sas=$("${oldc[@]}" pair sas --initiator "$user_key" --responder "$sub_key" --salt "$salt")
  sas_cmp="same"; [[ "$honest_sas" != "$sub_sas" ]] && sas_cmp="differ"
  assert_eq "$sas_cmp" "differ" \
    "a substituted responder key yields a DIFFERENT short code — the MITM the humans catch"

  # THE ENCRYPTION KEY IS BOUND TOO (§41, ADR-0049). The v2 SAS binds each
  # device's X25519 encryption key — the wrap target — alongside its signing key,
  # so a relay that keeps the honest SIGNING key but swaps the ENCRYPTION key
  # (to get itself enrolled as a wrap target) still yields a different code. Same
  # signing key throughout; only the responder-enc differs.
  local new_enc sub_enc enc_honest_sas enc_sub_sas enc_cmp
  new_enc=$("${newc[@]}" device show --json | jq -r .encryption_public_key)
  sub_enc=$("${subc[@]}" device show --json | jq -r .encryption_public_key)
  enc_honest_sas=$("${oldc[@]}" pair sas --initiator "$user_key" --responder "$new_key" --responder-enc "$new_enc" --salt "$salt")
  enc_sub_sas=$("${oldc[@]}" pair sas --initiator "$user_key" --responder "$new_key" --responder-enc "$sub_enc" --salt "$salt")
  enc_cmp="same"; [[ "$enc_honest_sas" != "$enc_sub_sas" ]] && enc_cmp="differ"
  assert_eq "$enc_cmp" "differ" \
    "a substituted responder ENCRYPTION key yields a DIFFERENT short code — the wrap-target swap the humans catch too"

  # THE HONEST PAIRING, over the real relay: both sides derive the SAME code and
  # the new device ends up enrolled under the user. Run concurrently, as the two
  # devices are; --yes stands in for the human who saw the codes match.
  local sess="acceptance-pair-ok" aout eout apid epid arc erc
  aout="$root/authorise.out"; eout="$root/enrol.out"
  ( "${oldc[@]}" pair authorise --relay "$sock" --session "$sess" --yes --poll 10ms >"$aout" 2>&1 ) &
  apid=$!
  ( "${newc[@]}" pair enrol --relay "$sock" --session "$sess" --yes --poll 10ms >"$eout" 2>&1 ) &
  epid=$!
  arc=0; wait "$apid" || arc=$?
  erc=0; wait "$epid" || erc=$?
  assert_eq "$arc" "0" "pair authorise completed"
  assert_eq "$erc" "0" "pair enrol completed"

  local asas esas sas_match
  asas=$(grep 'short authentication code:' "$aout" | sed 's/.*code: *//' | tr -d ' ')
  esas=$(grep 'short authentication code:' "$eout" | sed 's/.*code: *//' | tr -d ' ')
  sas_match="differ"; [[ -n "$asas" && "$asas" == "$esas" ]] && sas_match="match"
  assert_eq "$sas_match" "match" \
    "both devices derived the SAME short code over the relay — the SAS the humans compare"

  local after new_status enrolled_user
  after=$("${newc[@]}" device show --json)
  new_status=$(jq -r .enrolment_status <<<"$after")
  assert_eq "$new_status" "enrolled" \
    "the new device is enrolled by the old one over the relay — paired, no server trusted"
  enrolled_user=$(jq -r .enrolled_user <<<"$after")
  assert_eq "$enrolled_user" "$user_key" \
    "and the paired device authenticates as the SAME user the old device vouched for"

  # THE REFUSAL: told the codes did NOT match (a wrong --confirm-sas), the old
  # device refuses to sign and NO device is enrolled. The refusal is the
  # deliverable as much as the success.
  local rsess="acceptance-pair-refuse" refc rapid repid rarc auth_verdict ref_status
  refc=( env "VOIDBIND_IDENTITY_DIR=$root/ref-id" "VOIDBIND_DEVICE_DIR=$root/ref-dev" "$BIN" )
  "${refc[@]}" device generate --name reject-phone >/dev/null 2>&1
  ( "${oldc[@]}" pair authorise --relay "$sock" --session "$rsess" --confirm-sas 0000000 --poll 10ms >"$root/refuse-auth.out" 2>&1 ) &
  rapid=$!
  ( "${refc[@]}" pair enrol --relay "$sock" --session "$rsess" --yes --poll 10ms >"$root/refuse-enrol.out" 2>&1 ) &
  repid=$!
  rarc=0; wait "$rapid" || rarc=$?
  wait "$repid" 2>/dev/null || true
  auth_verdict="signed"; [[ "$rarc" != "0" ]] && auth_verdict="refused"
  assert_eq "$auth_verdict" "refused" \
    "a mismatched code makes the old device refuse to sign, so a substituted key enrols nobody"
  ref_status=$("${refc[@]}" device show --json | jq -r .enrolment_status)
  assert_eq "$ref_status" "not_enrolled" \
    "and the device left the refused pairing not_enrolled — the short code is the whole gate"

  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

# IDENTITY RECOVERY: the secret reconstructs the identity offline, and the
# recovered device authenticates (§79, ADR-0022, #306).
#
# ADR-0021 makes key loss total data loss, so recovery is load-bearing, not a
# convenience. A recovery secret is minted once at `identity generate`, and the
# identity is derived deterministically FROM it (recovery.DeriveUserSeed), so the
# secret alone reconstructs the SAME identity — same public key peers already
# pinned — on a machine with no surviving device and NO server. This proves the
# whole arc: the secret reconstructs the identity offline, enrols this machine's
# device under it, that device then authenticates as the same user, and a
# mistyped secret is refused loudly rather than reconstructing a wrong identity.
#
# Auth is ON, because proving the recovered device AUTHENTICATES needs a node
# that requires a credential — the recovery itself is offline and touches it not.
recovery_demo() {
  local root="$WORK/recovery" data sock
  data="$root/data"; sock="$data/heyarr.sock"
  mkdir -p "$data"

  cat > "$WORK/recovery.yaml" <<YAML
data_dir: $data
peer:
  name: acceptance-recovery
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

  local token
  token=$("$BIN" --config "$WORK/recovery.yaml" token create acceptance --scopes admin --json | jq -r .token)
  "$BIN" --config "$WORK/recovery.yaml" controller >"$root/controller.log" 2>&1 &
  local pid=$!
  local waited=0
  while (( waited < 600 )); do
    curl -sf --unix-socket "$sock" http://heyarr/readyz >/dev/null 2>&1 && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if (( waited >= 600 )); then
    fail "the recovery node never became ready"; cat "$root/controller.log"
    kill -KILL "$pid" 2>/dev/null || true; return 1
  fi

  # The ORIGINAL machine mints an identity and, once, its recovery secret.
  local origc gen orig_key secret secret_shown
  origc=( env "VOIDBIND_IDENTITY_DIR=$root/orig-id" "VOIDBIND_DEVICE_DIR=$root/orig-dev" "$BIN" )
  gen=$("${origc[@]}" identity generate --name owner --json)
  orig_key=$(jq -r .identity.public_key <<<"$gen")
  secret=$(jq -r .recovery_secret <<<"$gen")
  secret_shown="absent"; [[ -n "$secret" && "$secret" != "null" ]] && secret_shown="shown"
  assert_eq "$secret_shown" "shown" \
    "identity generate displays a recovery secret, once — the only way back if every device is lost"

  # A FRESH machine — no surviving identity, no surviving device — recovers from
  # the secret ALONE, offline (piped on stdin, out of argv). It reconstructs the
  # SAME identity and enrols this machine's device under it in one step.
  local newc rec rec_key rec_dev_status
  newc=( env "VOIDBIND_IDENTITY_DIR=$root/rec-id" "VOIDBIND_DEVICE_DIR=$root/rec-dev" "$BIN" )
  rec=$(printf '%s' "$secret" | "${newc[@]}" identity recover --json)
  rec_key=$(jq -r .identity.public_key <<<"$rec")
  assert_eq "$rec_key" "$orig_key" \
    "recovery reconstructs the SAME identity from the secret alone, offline — the public key peers already pinned"
  rec_dev_status=$(jq -r .device.enrolment_status <<<"$rec")
  assert_eq "$rec_dev_status" "enrolled" \
    "and it enrols this machine's device under the recovered identity in the same offline step"

  # THE RECOVERED DEVICE AUTHENTICATES. Pin the recovered user and enrol its
  # device on the node; then it authenticates with only its cert — no token.
  rec_api() { curl -sS --unix-socket "$sock" -H "Authorization: Bearer $token" "${@:2}" "http://heyarr$1"; }
  local user_body cert cert_body code cred
  user_body="{\"public_key\":\"$rec_key\",\"name\":\"owner\"}"
  code=$(rec_api /api/v1/identities/users -X POST -H 'Content-Type: application/json' -d "$user_body" -o /dev/null -w '%{http_code}')
  assert_eq "$code" "201" "an operator pins the recovered user identity"
  cert=$("${newc[@]}" identity credential | cut -d'~' -f1)
  cert_body="{\"cert\":\"$cert\",\"name\":\"recovered-phone\"}"
  code=$(rec_api /api/v1/identities/devices -X POST -H 'Content-Type: application/json' -d "$cert_body" -o /dev/null -w '%{http_code}')
  assert_eq "$code" "201" "and enrols the recovered device by its cert"
  cred=$("${newc[@]}" identity credential)
  code=$(curl -sS --unix-socket "$sock" -H "Authorization: Device $cred" -o /dev/null -w '%{http_code}' "http://heyarr/api/v1/libraries")
  assert_eq "$code" "200" \
    "the recovered device authenticates as the same user — offline recovery, then a live request, no token issued"

  # A WRONG SECRET FAILS LOUD: a mistyped secret is refused by its checksum, not
  # quietly reconstructed into a different, wrong identity. One character flipped
  # in the checksum tail; the parse rejects it before any key is derived.
  local bad
  bad="${secret%?}q"; [[ "$secret" == *q ]] && bad="${secret%?}p"
  assert_refuses "a corrupted recovery secret is refused loudly, never turned into a wrong identity" \
    "not accepted" "${newc[@]}" identity recover --secret "$bad"

  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

