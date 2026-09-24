# shellcheck shell=bash
# A section of scripts/acceptance.sh: sourced by it, in order, never run alone.
# ENCRYPTED PERSONAL STATE: the peer stores ciphertext it cannot read (§38, §42,
# ADR-0049, #320). A device mints a space key, wraps it for two enrolled devices,
# and pushes an encrypted change; the controller stores the opaque space, the
# wrapped keys and the ciphertext, and can turn none of it into plaintext. The
# authorised devices read the item back; a device the space was NOT wrapped for
# cannot. This is the first of the two evidence lines Milestone 9 owes (epic #28).
personalstate_demo() {
  local root="$WORK/personalstate" data sock cfg A B C
  data="$root/data"; sock="$data/heyarr.sock"; cfg="$WORK/personalstate.yaml"
  A="$root/dev-a"; B="$root/dev-b"; C="$root/dev-c"
  mkdir -p "$data"

  cat > "$cfg" <<YAML
data_dir: $data
peer:
  name: acceptance-personalstate
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
  token=$("$BIN" --config "$cfg" token create acceptance --scopes admin --json | jq -r .token)
  "$BIN" --config "$cfg" controller >"$root/controller.log" 2>&1 &
  local pid=$!
  local waited=0
  while (( waited < 600 )); do
    curl -sf --unix-socket "$sock" http://heyarr/readyz >/dev/null 2>&1 && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if (( waited >= 600 )); then
    fail "the personal-state node never became ready"; cat "$root/controller.log"
    kill -KILL "$pid" 2>/dev/null || true; return 1
  fi

  # Three devices, each with its own X25519 encryption key. A creates and reads,
  # B is a second authorised reader, C is a stranger the space is never wrapped for.
  "$BIN" device generate --device-dir "$A" --name deva >/dev/null 2>&1
  "$BIN" device generate --device-dir "$B" --name devb >/dev/null 2>&1
  "$BIN" device generate --device-dir "$C" --name devc >/dev/null 2>&1

  local bpub cpub base
  bpub=$("$BIN" device show --device-dir "$B" --json | jq -r .encryption_public_key)
  cpub=$("$BIN" device show --device-dir "$C" --json | jq -r .encryption_public_key)
  base=( env "HEYARR_TOKEN=$token" "$BIN" --config "$cfg" )

  # Enrol A and B so their encryption keys are PINNED recipients (enrol-before-wrap,
  # ADR-0049): a space key may be wrapped only for an enrolled device or recovery
  # key. C is left unenrolled on purpose — the stranger, and the negative below.
  ps_enrol_device "$sock" "$token" "$A" deva
  ps_enrol_device "$sock" "$token" "$B" devb

  # Device A mints a space, wrapping its key for itself (--self, default) and B.
  # --recovery=false: these devices' local recovery keys are not pinned here, and
  # this demo is about device recipients, not recovery (that is space_recovery_demo).
  local create space_id recips
  create=$("${base[@]}" space create --device-dir "$A" --kind personal --recipient "$bpub" --recovery=false --json)
  space_id=$(jq -r .id <<<"$create")
  recips=$(jq -r '.recipients | length' <<<"$create")
  assert_eq "$recips" "2" \
    "a space key is wrapped for two enrolled devices — the creating device and one more (§41)"

  # The kind is visible to the peer (structural, §38); nothing else is.
  local kind
  kind=$("${base[@]}" space list --json | jq -r '.[0].kind')
  assert_eq "$kind" "personal" \
    "the peer sees the space's kind — structural metadata — and stores no name (§38)"

  # Device A adds an item; it is encrypted on the device and pushed as ciphertext.
  "${base[@]}" space put "$space_id" --device-dir "$A" --item "midnight-jazz" >/dev/null

  # THE INVARIANT: what the peer stores is ciphertext. Fetch the stored change and
  # decode its bytes — the plaintext item is nowhere in them. (python3 does the
  # byte-containment check so a NUL in the ciphertext cannot truncate it in bash.)
  local changes ct opaque
  changes=$("${base[@]}" space changes "$space_id" --json)
  ct=$(jq -r '.[0].ciphertext' <<<"$changes")
  opaque=$(python3 -c 'import base64,sys; b=base64.b64decode(sys.argv[1]); sys.stdout.write("OPAQUE" if b"midnight-jazz" not in b else "PLAINTEXT-LEAKED")' "$ct")
  assert_eq "$opaque" "OPAQUE" \
    "the peer stores the change as ciphertext — the plaintext item is nowhere in the bytes at rest"

  # The authorised devices decrypt it locally; the merge is client-side.
  local a_item b_item
  a_item=$("${base[@]}" space read "$space_id" --device-dir "$A" --json | jq -r '.items[0]')
  assert_eq "$a_item" "midnight-jazz" \
    "the creating device unwraps the space key and reads the item back — the server never held it"
  b_item=$("${base[@]}" space read "$space_id" --device-dir "$B" --json | jq -r '.items[0]')
  assert_eq "$b_item" "midnight-jazz" \
    "a second authorised device decrypts the same item — the key was wrapped for it too"

  # A device the space was NOT wrapped for cannot read it — decrypt without the key
  # fails, and it fails before any change is fetched (the confidentiality gate).
  assert_refuses "a device the space was not wrapped for cannot read it — a decrypt without the key fails" \
    "cannot read space" "${base[@]}" space read "$space_id" --device-dir "$C"

  # ENROL-BEFORE-WRAP (ADR-0049): a space key cannot be wrapped for an unenrolled
  # recipient. C was never enrolled, so wrapping a new space for its key is refused
  # at the create path — a key issued and immediately wrapped-for is unspellable.
  assert_refuses "a space key cannot be wrapped for an unenrolled recipient (enrol-before-wrap, ADR-0049)" \
    "not an enrolled device" "${base[@]}" space create --device-dir "$A" --kind personal --recipient "$cpub" --recovery=false --json

  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

# THE DEVICE GATEWAY: a stock Subsonic client points at the DEVICE, and the
# device serves its playlists from state it decrypts locally while proxying the
# library to a controller that holds only ciphertext (§70, §72, §73, ADR-0051,
# #372, #387).
#
# This is the two-tier scene: two processes, a real controller and a real
# gateway, driven over HTTP the way a stock app drives them. Until it existed the
# gateway was proven only by a Go integration test — a thorough one, but the
# defect this file exists to catch is precisely a mechanism whose only caller is
# a test. The app-in-the-loop half (a real Subsonic APP, not curl) cannot live in
# a 300s headless demo and is recorded separately in
# docs/acceptance/real-app-in-the-loop.md.
gateway_demo() {
  local root="$WORK/gateway" data sock cfg D pwfile
  data="$root/data"; sock="$data/heyarr.sock"; cfg="$WORK/gateway.yaml"
  D="$root/dev"; pwfile="$root/device-password"
  mkdir -p "$data" "$D"

  # The gateway proxies library and stream methods to the controller over HTTP,
  # so unlike every other section here this controller needs a TCP listener as
  # well as its socket. Port 0: the kernel picks, the node logs what it got, and
  # two runs on one machine cannot collide — the reason the rest of this file
  # stays on a unix socket in the first place.
  cat > "$cfg" <<YAML
data_dir: $data
peer:
  name: acceptance-gateway
  site: test
log:
  level: info
  format: json
http:
  addr: 127.0.0.1:0
  unix_socket: $sock
  auth:
    enabled: true
libraries:
  - name: albums
    content_type: music
    roots: ["$FULLLIB/music"]
YAML

  local token
  token=$("$BIN" --config "$cfg" token create acceptance --scopes admin --json | jq -r .token)
  "$BIN" --config "$cfg" all >"$root/controller.log" 2>&1 &
  local pid=$!
  local waited=0
  while (( waited < 600 )); do
    curl -sf --unix-socket "$sock" http://heyarr/readyz >/dev/null 2>&1 && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if (( waited >= 600 )); then
    fail "the gateway scene's controller never became ready"; cat "$root/controller.log"
    kill -KILL "$pid" 2>/dev/null || true; return 1
  fi

  # The two music fixtures have to be IN the catalogue before the proxied
  # getArtists can prove anything; an empty answer would pass a shape check and
  # prove nothing at all.
  local ingested=0
  waited=0
  while (( waited < 1800 )); do
    ingested=$(grep -c '"msg":"ingested"' "$root/controller.log" 2>/dev/null || true)
    (( ingested >= 2 )) && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if (( ingested < 2 )); then
    fail "the gateway scene ingested only $ingested of 2 music files"
    kill -KILL "$pid" 2>/dev/null || true; return 1
  fi

  local http_addr
  http_addr=$(grep -o '"http_addr":"[^"]*"' "$root/controller.log" | head -1 | cut -d'"' -f4)
  if [[ -z "$http_addr" ]]; then
    fail "the controller never reported the TCP address it bound"
    kill -KILL "$pid" 2>/dev/null || true; return 1
  fi

  # One device: it is both the personal-state reader and the thing the stock app
  # talks to. Enrolled, because a space key may be wrapped only for an enrolled
  # device (enrol-before-wrap, ADR-0049).
  "$BIN" device generate --device-dir "$D" --name gateway-device >/dev/null 2>&1
  ps_enrol_device "$sock" "$token" "$D" gateway-device

  local base create space_id
  base=( env "HEYARR_TOKEN=$token" "$BIN" --config "$cfg" )
  create=$("${base[@]}" space create --device-dir "$D" --kind personal --recovery=false --json)
  space_id=$(jq -r .id <<<"$create")
  "${base[@]}" space put "$space_id" --device-dir "$D" --item "midnight-jazz" >/dev/null

  # The password the APP uses to reach the DEVICE. It is not the controller's
  # bearer and the negative below proves the two cannot be swapped.
  printf '%s' 'stock-app-password' > "$pwfile"
  chmod 0600 "$pwfile"

  env "HEYARR_TOKEN=$token" "$BIN" --config "$cfg" device gateway \
    --device-dir "$D" --addr "http://$http_addr" --controller-url "http://$http_addr" \
    --device-user stockapp --device-password-file "$pwfile" \
    --listen 127.0.0.1:0 >"$root/gateway.log" 2>&1 &
  local gwpid=$!

  # The gateway prints the address it bound; wait for that line rather than for a
  # fixed sleep, which is the bet on machine speed this file has lost before.
  local gw_addr=""
  waited=0
  while (( waited < 600 )); do
    gw_addr=$(sed -n 's|.*serving Subsonic on http://\([^/]*\)/rest.*|\1|p' "$root/gateway.log" | head -1)
    [[ -n "$gw_addr" ]] && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if [[ -z "$gw_addr" ]]; then
    fail "the gateway never reported the address it bound"; cat "$root/gateway.log"
    kill -KILL "$gwpid" "$pid" 2>/dev/null || true; return 1
  fi

  gw() { # method [extra-query]
    curl -sS "http://$gw_addr/rest/$1?u=stockapp&p=stock-app-password&c=acceptance&v=1.16.1&f=json${2:+&$2}"
  }

  assert_eq "$(jq -r '.["subsonic-response"].status' <<<"$(gw ping)")" "ok" \
    "a stock Subsonic client's ping is answered by the DEVICE, not the controller"

  # THE SEPARATION ADR-0051 CLAIMS. The app holds a password for the device; the
  # device holds a bearer for the controller. If the controller's bearer worked
  # here the app would be holding the controller's credential after all, which is
  # the whole thing the gateway exists to avoid.
  assert_eq "$(jq -r '.["subsonic-response"].status' <<<"$(curl -sS \
    "http://$gw_addr/rest/ping?u=stockapp&p=$token&c=acceptance&v=1.16.1&f=json")")" "failed" \
    "the controller's bearer does NOT authenticate to the device — the two credentials are distinct"
  assert_eq "$(jq -r '.["subsonic-response"].status' <<<"$(curl -sS \
    "http://$gw_addr/rest/ping?u=stockapp&p=wrong&c=acceptance&v=1.16.1&f=json")")" "failed" \
    "a wrong device password is refused"

  # THE CLAIM. The playlist the app reads came out of ciphertext the controller
  # stores and cannot open.
  local playlists pl_id pl_items
  playlists=$(gw getPlaylists)
  pl_id=$(jq -r '.["subsonic-response"].playlists.playlist[0].id' <<<"$playlists")
  assert_eq "$pl_id" "$space_id" \
    "the device serves a playlist whose id is the space it decrypted"
  pl_items=$(jq -r '[.["subsonic-response"].playlist.entry[]?.title] | join(",")' <<<"$(gw getPlaylist "id=$pl_id")")
  assert_contains "$pl_items" "midnight-jazz" \
    "a stock-Subsonic request read its playlist through the device gateway while the controller held only ciphertext"

  # The other half of that sentence, measured rather than trusted: what the
  # controller stores for this space does not contain the plaintext. python3 does
  # the containment check so a NUL in the ciphertext cannot truncate it in bash.
  local ct opaque
  ct=$("${base[@]}" space changes "$space_id" --json | jq -r '.[0].ciphertext')
  opaque=$(python3 -c 'import base64,sys; b=base64.b64decode(sys.argv[1]); sys.stdout.write("OPAQUE" if b"midnight-jazz" not in b else "PLAINTEXT-LEAKED")' "$ct")
  assert_eq "$opaque" "OPAQUE" \
    "and the controller's copy of that playlist is ciphertext — the plaintext item is nowhere in the bytes at rest"

  # PROXYING, the other family of method: the library is the controller's and the
  # gateway passes it through under its own bearer.
  assert_eq "$(( $(jq -r '.["subsonic-response"].artists.index | length' <<<"$(gw getArtists)") > 0 ))" "1" \
    "the library the device serves is the controller's, proxied — the device stores no catalogue"

  # A proxied STREAM is the same bytes the ordinary blob route serves. A gateway
  # that re-encoded, truncated or invented bytes would pass every assertion above.
  local album_id song_id gw_sha direct_sha
  album_id=$(jq -r 'first(.["subsonic-response"].albumList2.album[] | select(.songCount > 0) | .id)' \
    <<<"$(gw getAlbumList2 "type=alphabeticalByName&size=500")")
  song_id=$(jq -r '.["subsonic-response"].album.song[0].id' <<<"$(gw getAlbum "id=$album_id")")
  gw_sha=$(curl -sS "http://$gw_addr/rest/stream?u=stockapp&p=stock-app-password&c=acceptance&v=1.16.1&id=$song_id" | shasum -a 256 | cut -d" " -f1)
  direct_sha=$(curl -sS -H "Authorization: Bearer $token" \
    "http://$http_addr/rest/stream?u=acceptance&p=$token&c=acceptance&v=1.16.1&id=$song_id" | shasum -a 256 | cut -d" " -f1)
  assert_eq "$gw_sha" "$direct_sha" \
    "the bytes a stock app streams through the device are byte-identical to the controller's own"

  kill -TERM "$gwpid" 2>/dev/null || true
  wait "$gwpid" 2>/dev/null || true
  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}


# REVOCATION CUTS ACCESS: a device is revoked from a space by rotating its key.
# The revoked device's wrapped copy is DELETED and the space is re-keyed for
# everyone else, then a snapshot re-encrypts the current state under the new key
# and the old change log is compacted — so the revoked device can read nothing
# encrypted from here on (§41, ADR-0022, ADR-0049, #361). It is forward-looking,
# not retroactive: the revoked device keeps whatever it already decrypted.
revocation_demo() {
  local root="$WORK/revocation" data sock cfg A B C
  data="$root/data"; sock="$data/heyarr.sock"; cfg="$WORK/revocation.yaml"
  A="$root/dev-a"; B="$root/dev-b"; C="$root/dev-c"
  mkdir -p "$data"

  cat > "$cfg" <<YAML
data_dir: $data
peer:
  name: acceptance-revocation
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
  token=$("$BIN" --config "$cfg" token create acceptance --scopes admin --json | jq -r .token)
  "$BIN" --config "$cfg" controller >"$root/controller.log" 2>&1 &
  local pid=$!
  local waited=0
  while (( waited < 600 )); do
    curl -sf --unix-socket "$sock" http://heyarr/readyz >/dev/null 2>&1 && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if (( waited >= 600 )); then
    fail "the revocation node never became ready"; cat "$root/controller.log"
    kill -KILL "$pid" 2>/dev/null || true; return 1
  fi

  # Three devices: A creates and stays, B will be revoked, C stays.
  "$BIN" device generate --device-dir "$A" --name reva >/dev/null 2>&1
  "$BIN" device generate --device-dir "$B" --name revb >/dev/null 2>&1
  "$BIN" device generate --device-dir "$C" --name revc >/dev/null 2>&1

  local bpub cpub base
  bpub=$("$BIN" device show --device-dir "$B" --json | jq -r .encryption_public_key)
  cpub=$("$BIN" device show --device-dir "$C" --json | jq -r .encryption_public_key)
  base=( env "HEYARR_TOKEN=$token" "$BIN" --config "$cfg" )

  # Enrol all three so their keys are pinned recipients (enrol-before-wrap,
  # ADR-0049): the create wraps for A, B and C, and the rotation re-wraps for the
  # remaining two — every one of them must be an enrolled device.
  ps_enrol_device "$sock" "$token" "$A" reva
  ps_enrol_device "$sock" "$token" "$B" revb
  ps_enrol_device "$sock" "$token" "$C" revc

  # A mints a space wrapped for A, B and C (no recovery key in this demo).
  local create space_id recips
  create=$("${base[@]}" space create --device-dir "$A" --kind personal \
    --recipient "$bpub" --recipient "$cpub" --recovery=false --json)
  space_id=$(jq -r .id <<<"$create")
  recips=$(jq -r '.recipients | length' <<<"$create")
  assert_eq "$recips" "3" \
    "the space is wrapped for three devices before any revocation"

  # A adds an item; B can read it — B is a recipient at this point.
  "${base[@]}" space put "$space_id" --device-dir "$A" --item "midnight-jazz" >/dev/null
  local b_before
  b_before=$("${base[@]}" space read "$space_id" --device-dir "$B" --json | jq -r '.items[0]')
  assert_eq "$b_before" "midnight-jazz" \
    "before revocation the soon-to-be-revoked device can read the space"

  # A revokes B: rotate the key, re-wrap for A and C, delete B's copy, snapshot,
  # compact the old log the snapshot subsumes.
  "${base[@]}" space rotate "$space_id" --device-dir "$A" --revoke "$bpub" >/dev/null

  # THE INVARIANT: the revoked device can no longer read the space. Its wrapped key
  # is gone, so the read fails before any change is fetched (the confidentiality
  # gate) — a rotation that left B's key in place, or re-keyed for B too, would let
  # this succeed.
  assert_refuses "a revoked device can no longer read the space — its wrapped key is gone and the space was re-keyed without it" \
    "cannot read space" "${base[@]}" space read "$space_id" --device-dir "$B"

  # A remaining device still reads the state — re-keyed for it, reachable from the
  # post-rotation snapshot without the old key.
  local c_after
  c_after=$("${base[@]}" space read "$space_id" --device-dir "$C" --json | jq -r '.items[0]')
  assert_eq "$c_after" "midnight-jazz" \
    "a remaining device still reads the space after the rotation — re-keyed, and reachable from the new snapshot"

  # And the peer holds no wrapped key for the revoked device: two remain (A, C).
  local remaining count_b
  remaining=$("${base[@]}" space keys "$space_id" --json | jq -r 'length')
  assert_eq "$remaining" "2" \
    "after revocation the peer holds a wrapped key for the two remaining devices only"
  count_b=$("${base[@]}" space keys "$space_id" --json | jq -r --arg b "$bpub" '[.[] | select(.recipient == $b)] | length')
  assert_eq "$count_b" "0" \
    "the revoked device's wrapped copy of the key is deleted from the peer"

  # THE FORWARD-ONLY BOUNDARY (§41, ADR-0022, ADR-0049, #321). Revocation protects
  # the FUTURE, not the past. A writes a change AFTER the revocation: the surviving
  # device reads it — the new key is live — while the revoked device cannot, so
  # future content is put beyond it. But the change the revoked device ALREADY
  # decrypted ("midnight-jazz", read above while it was still a recipient) stays
  # its to keep: revocation is forward-looking, not retroactive. This is the
  # honesty ADR-0022 owes and #321 asked to make visible rather than gloss — a
  # rotation that retroactively locked the past would be a promise the mechanism
  # cannot keep, and one that left the future readable would be no revocation.
  # The claim that the HELD change still decrypts under the old key after the peer
  # has dropped both the key copy and the change itself is proven end to end in
  # internal/personalstate/scenario (TestRevocationIsForwardOnly), where the
  # revoked device really decrypts it; here the boundary is made visible on the
  # real binary.
  "${base[@]}" space put "$space_id" --device-dir "$A" --item "after-revocation-track" >/dev/null
  local c_future
  c_future=$("${base[@]}" space read "$space_id" --device-dir "$C" --json | jq -r '.items | contains(["after-revocation-track"])')
  assert_eq "$c_future" "true" \
    "a surviving device reads content written AFTER the revocation — the new key is live and forward writes land under it"
  assert_refuses "the revoked device cannot read the post-revocation change either — only future content is put beyond it, under the new key" \
    "cannot read space" "${base[@]}" space read "$space_id" --device-dir "$B"
  assert_eq "$b_before" "midnight-jazz" \
    "the revoked device keeps the pre-revocation change it already decrypted — forward-only revocation, only future content is protected"

  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

# CONVERGE AFTER A PARTITION: two peers, an offline concurrent edit on each, and
# on reconnect the encrypted changes replicate and the two devices converge to
# one state — merged CLIENT-SIDE, the server never seeing plaintext (§42, §43,
# ADR-0049, #324). This is the SECOND of the two evidence lines Milestone 9 owes
# (epic #28), alongside the ciphertext-at-rest line from personalstate_demo.
converge_after_partition_demo() {
  local root="$WORK/converge"
  mkdir -p "$root"

  local cfg_a="$WORK/converge-a.yaml" cfg_b="$WORK/converge-b.yaml"
  local n
  for n in a b; do
    mkdir -p "$root/$n"
    cat > "$WORK/converge-$n.yaml" <<YAML
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
YAML
  done

  local sock_a="$root/a/data/heyarr.sock" sock_b="$root/b/data/heyarr.sock"
  local log_a="$root/a.log" log_b="$root/b.log"
  local token_a token_b
  token_a=$("$BIN" --config "$cfg_a" token create acceptance --scopes admin --json | jq -r .token)
  token_b=$("$BIN" --config "$cfg_b" token create acceptance --scopes admin --json | jq -r .token)

  local mypids=() p
  start_peer_node "$cfg_a" "$log_a" all; mypids+=("${NODE_PIDS[@]}")
  start_peer_node "$cfg_b" "$log_b" all; mypids+=("${NODE_PIDS[@]}")

  local s waited
  for s in "$sock_a" "$sock_b"; do
    waited=0
    while (( waited < 600 )); do
      curl -sf --unix-socket "$s" http://heyarr/readyz >/dev/null 2>&1 && break
      sleep 0.1; waited=$(( waited + 1 ))
    done
    if (( waited >= 600 )); then
      fail "converge: $s never became ready"; tail -20 "$log_a" "$log_b"
      for p in "${mypids[@]}"; do kill -KILL "$p" 2>/dev/null || true; done
      return 1
    fi
  done

  local addr_a addr_b
  addr_a=$(peer_listen_addr "$log_a") || { fail "converge: node A never bound a peer surface"; return 1; }
  addr_b=$(peer_listen_addr "$log_b") || { fail "converge: node B never bound a peer surface"; return 1; }

  cli_a() { "$BIN" --config "$cfg_a" --token "$token_a" "$@"; }
  cli_b() { "$BIN" --config "$cfg_b" --token "$token_b" "$@"; }
  sa_api() { curl -sS --unix-socket "$sock_a" -H "Authorization: Bearer $token_a" "${@:2}" "http://heyarr$1"; }
  sb_api() { curl -sS --unix-socket "$sock_b" -H "Authorization: Bearer $token_b" "${@:2}" "http://heyarr$1"; }
  local base_a base_b
  base_a=( env "HEYARR_TOKEN=$token_a" "$BIN" --config "$cfg_a" )
  base_b=( env "HEYARR_TOKEN=$token_b" "$BIN" --config "$cfg_b" )

  # Enrol the two peers into each other's membership (mTLS, ADR-0012), each with
  # the other's bound peer-surface endpoint, plus itself — the sequence that makes
  # a peer-to-peer request trusted in both directions.
  local key_a key_b
  key_a=$(cli_a peers list --json | jq -r '.[] | select(.is_self) | .public_key')
  key_b=$(cli_b peers list --json | jq -r '.[] | select(.is_self) | .public_key')
  cli_a peers add --name site-b --site site-b --mode full --public-key "$key_b" --endpoint "https://$addr_b" --json >/dev/null
  cli_b peers add --name site-a --site site-a --mode full --public-key "$key_a" --endpoint "https://$addr_a" --json >/dev/null
  cli_a peers add --name site-a --public-key "$key_a" --endpoint "https://$addr_a" --json >/dev/null
  cli_b peers add --name site-b --public-key "$key_b" --endpoint "https://$addr_b" --json >/dev/null

  # Two devices, one per site: X on A, Y on B, each with its own encryption key.
  local dx="$root/dev-x" dy="$root/dev-y" ypub
  "$BIN" device generate --device-dir "$dx" --name device-x >/dev/null 2>&1
  "$BIN" device generate --device-dir "$dy" --name device-y >/dev/null 2>&1
  ypub=$("$BIN" device show --device-dir "$dy" --json | jq -r .encryption_public_key)

  # Both devices are enrolled on node A, where the space is created — a space key
  # may be wrapped only for pinned recipients (enrol-before-wrap, ADR-0049).
  ps_enrol_device "$sock_a" "$token_a" "$dx" device-x
  ps_enrol_device "$sock_a" "$token_a" "$dy" device-y

  # Device X creates a space on A, wrapped for X (self) and Y, and writes one item.
  local create space_id
  create=$("${base_a[@]}" space create --device-dir "$dx" --kind shared --recipient "$ypub" --recovery=false --json)
  space_id=$(jq -r .id <<<"$create")
  "${base_a[@]}" space put "$space_id" --device-dir "$dx" --item "x-first" >/dev/null

  # Replicate A -> B. Now B holds the space, the wrapped keys and X's change — as
  # ciphertext it cannot read.
  local repl replicated
  repl=$(sa_api "/api/v1/state/replicate" -X POST)
  replicated=$(jq -r '.replicated' <<<"$repl")
  assert_eq "$replicated" "1" \
    "the space replicated from peer A to peer B — one (peer, space) pair converged"

  # Device Y, on B, decrypts what replicated: the second peer holds the space it
  # cannot itself read, and only Y's key opens it.
  local y_sees
  y_sees=$("${base_b[@]}" space read "$space_id" --device-dir "$dy" --json | jq -r '.items[0]')
  assert_eq "$y_sees" "x-first" \
    "the second peer holds the encrypted space and an authorised device reads it there — replicated as ciphertext"

  # THE PARTITION: with no replication, X writes on A and Y writes on B — a
  # concurrent, offline edit on each side.
  "${base_a[@]}" space put "$space_id" --device-dir "$dx" --item "x-second" >/dev/null
  "${base_b[@]}" space put "$space_id" --device-dir "$dy" --item "y-only" >/dev/null

  # RECONNECT: replicate both ways. A gets Y's change, B gets X's second change.
  sa_api "/api/v1/state/replicate" -X POST >/dev/null
  sb_api "/api/v1/state/replicate" -X POST >/dev/null

  # CONVERGENCE, merged CLIENT-SIDE: device X (reading from A) and device Y
  # (reading from B) materialise the SAME playlist — order-independent, from the
  # causal DAG — containing all three concurrent items.
  local x_state y_state
  x_state=$("${base_a[@]}" space read "$space_id" --device-dir "$dx" --json | jq -c '.items')
  y_state=$("${base_b[@]}" space read "$space_id" --device-dir "$dy" --json | jq -c '.items')
  assert_eq "$x_state" "$y_state" \
    "the two devices converge to the same state after an offline concurrent edit, merged client-side"
  assert_contains "$x_state" "y-only" "the converged state contains the edit made on the OTHER peer during the partition"
  assert_eq "$(jq 'length' <<<"$x_state")" "3" "and all three concurrent items survived the merge"

  # THE SERVER NEVER SAW PLAINTEXT: decode peer B's stored change bytes and confirm
  # no item appears in any of them — ciphertext throughout the exchange. (python3
  # decodes each change separately, so a NUL byte cannot truncate the check.)
  local raw opaque
  raw=$(sb_api "/api/v1/spaces/$space_id/changes" | jq -c '[.changes[].ciphertext]')
  opaque=$(python3 -c 'import base64,json,sys; arr=json.loads(sys.argv[1]); items=[b"x-first",b"x-second",b"y-only"]; leaked=any(i in base64.b64decode(c) for c in arr for i in items); sys.stdout.write("PLAINTEXT-LEAKED" if leaked else "OPAQUE")' "$raw")
  assert_eq "$opaque" "OPAQUE" \
    "peer B's stored personal-state bytes stayed ciphertext throughout — the server merged nothing and read nothing"

  for p in "${mypids[@]}"; do kill -TERM "$p" 2>/dev/null || true; done
  for p in "${mypids[@]}"; do wait "$p" 2>/dev/null || true; done
}

# SNAPSHOTS BOUND THE LOG (§44, ADR-0049, #325): a device takes an encrypted
# snapshot at a causal point, the log is compacted so the pre-snapshot changes are
# dropped, and a device still reaches the full state from the snapshot plus the
# tail — the bounded sync a long-lived space (or a phone) needs. The snapshot at
# rest is ciphertext the server cannot read.
snapshot_demo() {
  local root="$WORK/snapshot" data sock cfg dev
  data="$root/data"; sock="$data/heyarr.sock"; cfg="$WORK/snapshot.yaml"; dev="$root/dev"
  mkdir -p "$data"

  cat > "$cfg" <<YAML
data_dir: $data
peer:
  name: acceptance-snapshot
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
  token=$("$BIN" --config "$cfg" token create acceptance --scopes admin --json | jq -r .token)
  "$BIN" --config "$cfg" controller >"$root/controller.log" 2>&1 &
  local pid=$!
  local waited=0
  while (( waited < 600 )); do
    curl -sf --unix-socket "$sock" http://heyarr/readyz >/dev/null 2>&1 && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if (( waited >= 600 )); then
    fail "the snapshot node never became ready"; cat "$root/controller.log"
    kill -KILL "$pid" 2>/dev/null || true; return 1
  fi

  "$BIN" device generate --device-dir "$dev" --name snap-dev >/dev/null 2>&1
  local base space_id
  base=( env "HEYARR_TOKEN=$token" "$BIN" --config "$cfg" )
  sapi() { curl -sS --unix-socket "$sock" -H "Authorization: Bearer $token" "${@:2}" "http://heyarr$1"; }

  # The device is enrolled so its key is a pinned recipient (enrol-before-wrap, ADR-0049).
  ps_enrol_device "$sock" "$token" "$dev" snap-dev

  space_id=$("${base[@]}" space create --device-dir "$dev" --kind personal --recovery=false --json | jq -r .id)
  "${base[@]}" space put "$space_id" --device-dir "$dev" --item one >/dev/null
  "${base[@]}" space put "$space_id" --device-dir "$dev" --item two >/dev/null
  "${base[@]}" space put "$space_id" --device-dir "$dev" --item three >/dev/null

  # Take a snapshot, then add one more change (the tail).
  "${base[@]}" space snapshot "$space_id" --device-dir "$dev" >/dev/null
  "${base[@]}" space put "$space_id" --device-dir "$dev" --item four >/dev/null

  local before after dropped
  before=$("${base[@]}" space changes "$space_id" --json | jq 'length')
  assert_eq "$before" "4" "the log holds all four changes before compaction"

  dropped=$("${base[@]}" space compact "$space_id" --json | jq -r .dropped)
  assert_eq "$dropped" "3" "compaction drops the three changes the snapshot subsumes"

  after=$("${base[@]}" space changes "$space_id" --json | jq 'length')
  assert_eq "$after" "1" "the log is now bounded — only the tail change remains"

  # A device STILL reaches the full state, reconstructed from the snapshot + tail,
  # even though the pre-snapshot changes are gone.
  local items
  items=$("${base[@]}" space read "$space_id" --device-dir "$dev" --json | jq -c '.items')
  assert_eq "$(jq 'length' <<<"$items")" "4" \
    "a device reaches the full state from the snapshot plus the tail, after the pre-snapshot changes were compacted away"
  assert_contains "$items" "one" "and the state folded into the snapshot survives the compaction"

  # The snapshot at rest is ciphertext — no item appears in its bytes.
  local ct opaque
  ct=$(sapi "/api/v1/spaces/$space_id/snapshot" | jq -r .ciphertext)
  opaque=$(python3 -c 'import base64,sys; b=base64.b64decode(sys.argv[1]); items=[b"one",b"two",b"three",b"four"]; sys.stdout.write("PLAINTEXT-LEAKED" if any(i in b for i in items) else "OPAQUE")' "$ct")
  assert_eq "$opaque" "OPAQUE" \
    "the snapshot at rest is ciphertext — the server materialises nothing and reads nothing"

  kill -TERM "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

