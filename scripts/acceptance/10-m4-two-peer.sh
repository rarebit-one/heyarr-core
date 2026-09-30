# shellcheck shell=bash
# A section of scripts/acceptance.sh: sourced by it, in order, never run alone.
two_peer_demo() { # mode
  local mode=$1
  local root="$WORK/twopeer-$mode" lib
  lib="$root/library"
  mkdir -p "$lib/movies/Signal Fire (2021)" "$lib/movies/Static Field (2019)" \
    "$lib/movies/Cold Harbour (2018)"

  # Three small files, ingested by BOTH nodes from the same directory. That is
  # what gives the two catalogues the same blob digests without either node
  # having to be told about the other's contents — content addressing doing the
  # job it exists for (invariant 1).
  #
  # Three rather than two because the garbage-collection arc needs two blobs it
  # can make unreferenced: one to unlink while this node is still a fabric of
  # one, and one to spare once a second peer exists and has gone away. A refusal
  # with no matching permission beside it is indistinguishable from a collector
  # that refuses everything, so the permission has to be shown too.
  # Signal Fire is the file the transfer arc moves, and it is deliberately the
  # only one ABOVE §16's chunking threshold (manifests.ThresholdBytes, 4 MiB).
  # The lazy-chunking section at the end of this arc needs a blob on each side
  # of that number, and making the one that already crosses the wire the large
  # one costs a loopback transfer and no new fixture.
  #
  # A HUNDRED megabytes rather than five, and the size is load-bearing rather
  # than generous.
  #
  # The repair arc below asserts this milestone's thesis — that a repair costs
  # the DAMAGE and not the blob — and how convincing that number looks is
  # entirely a property of how many chunks the blob has. At the shipped chunker
  # parameters (256 KiB minimum, 1 MiB average, 4 MiB maximum) five megabytes
  # is THREE chunks, so one chunk of it is a third, and the measured figure
  # swung between 8% and 23% run to run purely with where a content-defined
  # boundary fell in random bytes. A hundred megabytes is ~89 chunks and one of
  # them is ~1%.
  #
  # The alternative was shrinking the chunker for the fixture, and it is not
  # available and should not be: two peers that chunk with different parameters
  # share no chunks at all, which is why #198 pins boundaries with golden
  # fixtures across three platforms. A per-node chunking knob would be a
  # footgun aimed at the whole feature.
  #
  # Measured cost of the change, same machine, equipped, verdict line both
  # times: 187s at five megabytes, 194s at a hundred. Seven seconds against a
  # 240-second budget, for an assertion that reads as the thing it claims.
  head -c 104857600 /dev/urandom > "$lib/movies/Signal Fire (2021)/Signal.Fire.2021.1080p.mkv"
  head -c 131072 /dev/urandom > "$lib/movies/Static Field (2019)/Static.Field.2019.1080p.mkv"
  head -c 196608 /dev/urandom > "$lib/movies/Cold Harbour (2018)/Cold.Harbour.2018.1080p.mkv"

  local cfg_a="$WORK/twopeer-$mode-a.yaml" cfg_b="$WORK/twopeer-$mode-b.yaml"
  local n
  for n in a b; do
    mkdir -p "$root/$n"
    cat > "$WORK/twopeer-$mode-$n.yaml" <<YAML
data_dir: $root/$n/data
peer:
  name: site-$n
  site: site-$n
  # The mTLS peer surface (§26, ADR-0012). Port 0: the kernel chooses, the
  # node logs what it got, and two concurrent runs cannot collide.
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

  local sock_a="$root/a/data/heyarr.sock" sock_b="$root/b/data/heyarr.sock"
  local log_a="$root/a.log" log_b="$root/b.log"
  local token_a token_b
  token_a=$("$BIN" --config "$cfg_a" token create acceptance --scopes admin --json | jq -r .token)
  token_b=$("$BIN" --config "$cfg_b" token create acceptance --scopes admin --json | jq -r .token)

  # Only node B's PIDs are kept: it is the one this section stops on purpose,
  # to make a peer go away. Node A runs to the end and is stopped with
  # everything else through PEER_PIDS.
  local pids_b=()
  start_peer_node "$cfg_a" "$log_a" "$mode"
  start_peer_node "$cfg_b" "$log_b" "$mode"; pids_b=("${NODE_PIDS[@]}")

  # api_a / api_b are the two nodes' client APIs; cli_a / cli_b are the two
  # operators. Both are needed: enrolment and inventory reporting are CLI verbs
  # over the peer surface, and satisfaction is an HTTP question.
  api_a() { curl -sS --unix-socket "$sock_a" -H "Authorization: Bearer $token_a" "${@:2}" "http://heyarr$1"; }
  api_b() { curl -sS --unix-socket "$sock_b" -H "Authorization: Bearer $token_b" "${@:2}" "http://heyarr$1"; }
  cli_a() { "$BIN" --config "$cfg_a" --token "$token_a" "$@"; }
  cli_b() { "$BIN" --config "$cfg_b" --token "$token_b" "$@"; }

  local s waited
  for s in "$sock_a" "$sock_b"; do
    waited=0
    while (( waited < 600 )); do
      curl -sf --unix-socket "$s" http://heyarr/readyz >/dev/null 2>&1 && break
      sleep 0.1; waited=$(( waited + 1 ))
    done
    if (( waited >= 600 )); then
      fail "two-peer: $s never became ready"; tail -20 "$log_a" "$log_b"; return 1
    fi
  done

  local l
  for l in "$log_a" "$log_b"; do
    waited=0
    while (( waited < 900 )); do
      (( $(grep -c '"msg":"ingested"' "$l" 2>/dev/null || true) >= 3 )) && break
      sleep 0.1; waited=$(( waited + 1 ))
    done
    if (( waited >= 900 )); then
      fail "two-peer: $l never ingested all three fixture files"; tail -20 "$l"; return 1
    fi
  done

  local addr_a addr_b
  addr_a=$(peer_listen_addr "$log_a") || { fail "two-peer: node A never bound a peer surface"; return 1; }
  addr_b=$(peer_listen_addr "$log_b") || { fail "two-peer: node B never bound a peer surface"; return 1; }
  assert_contains "$addr_a" "127.0.0.1:" "the mTLS peer surface binds an address of its own (§26, ADR-0012)"

  # -------------------------------------------------------------------------
  note "  one peer: placement is satisfied, and says why that proves nothing"
  # -------------------------------------------------------------------------
  #
  # Node A is a fabric of one, which is what most Heyarr deployments will be
  # forever. Asserted FIRST, before the second peer is enrolled, because it is
  # the half of the `unproven` contract that a hard-coded `false` would break —
  # and a suite that only checked the two-peer answer would never notice.
  local tp_profile tp_work tp_want tp_json
  tp_profile=$(api_a /api/v1/quality-profiles -X POST -H 'Content-Type: application/json' \
    -d '{"name":"twopeer-anything","description":"accepts whatever exists"}' | jq -r '.id')
  tp_work=$(api_a /api/v1/works | jq -r '.items[] | select(.title == "Signal Fire") | .id')
  tp_want=$(api_a /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d "{\"work_id\":\"$tp_work\",\"quality_profile_id\":\"$tp_profile\"}" | jq -r '.id')

  tp_json=$(api_a "/api/v1/desired/$tp_want/satisfaction")
  assert_eq "$(jq -r '.content.satisfaction' <<<"$tp_json")" "satisfied" \
    "node A holds bytes a profile that accepts anything accepts"
  assert_eq "$(jq -r '.placement.satisfaction' <<<"$tp_json")" "satisfied" \
    "with one peer, placement is satisfied the moment content is"
  assert_eq "$(jq -r '.placement.unproven' <<<"$tp_json")" "true" \
    "and it says so: unproven is TRUE when the target set is this node alone"
  assert_eq "$(jq -r '.state' <<<"$tp_json")" "FULLY_SATISFIED" \
    "so the §64 name goes straight to FULLY_SATISFIED — PLACEMENT_CONVERGING is unreachable here"

  # -------------------------------------------------------------------------
  note "  and garbage collection lets go of bytes, while this is still a fabric of one"
  # -------------------------------------------------------------------------
  #
  # THE POSITIVE CONTROL for the refusal at the end of this section, and it has
  # to come first, on this node, while it still has no other peer.
  #
  # The refusal below is the milestone's other headline. It is also the
  # assertion that a collector which refuses EVERYTHING passes perfectly, and a
  # precondition that never lets go is a different way of losing a library —
  # slowly, to a full disk. So the same binary, on the same node, with the same
  # collector, is made to unlink a blob HERE. Between this assertion and the
  # refusal a hundred lines below, nothing about the code changes; a second peer
  # is enrolled.
  #
  # It uses the third fixture, whose bytes node B also holds and which nothing
  # after this point asserts on.
  local sole_blob sole_assets sole_pass1 sole_pass2 aid
  sole_blob=$(api_a /api/v1/assets |
    jq -r '.items[] | select(.source_path != null and (.source_path | contains("Cold.Harbour"))) | .blob_hash' |
    head -1)
  assert_contains "$sole_blob" "blake3:" "the third fixture names bytes this node ingested"
  assert_eq "$(peer_holds "$root/a/data/cas" "$sole_blob")" "1" "and node A holds them"

  # Unreference them: DELETE /assets removes the catalog row and never touches a
  # byte, which is precisely the garbage a sweep exists to find (ADR-0018).
  sole_assets=$(api_a /api/v1/assets | jq -r --arg h "$sole_blob" '.items[] | select(.blob_hash == $h) | .id')
  for aid in $sole_assets; do api_a "/api/v1/assets/$aid" -X DELETE -o /dev/null; done
  assert_eq "$(api_a /api/v1/assets | jq -r --arg h "$sole_blob" '[.items[] | select(.blob_hash == $h)] | length')" "0" \
    "nothing in the catalogue references those bytes any more"

  # Two passes, because a blob is never reclaimed by the pass that marks it. The
  # grace window is a window and not a flag, and a single-pass reclaim would
  # mean a bad scan and a `gc --apply` in the same minute took the library.
  sole_pass1=$("$BIN" --config "$cfg_a" gc --apply --grace 1ns --json 2>/dev/null)
  assert_eq "$(jq -r --arg h "$sole_blob" '[.marked[].hash] | index($h) != null' <<<"$sole_pass1")" "true" \
    "the first pass starts the grace window"
  assert_eq "$(jq -r '.reclaimed | length' <<<"$sole_pass1")" "0" \
    "and reclaims nothing at all on the pass that marked it"
  assert_eq "$(peer_holds "$root/a/data/cas" "$sole_blob")" "1" "so the bytes are still there afterwards"

  sole_pass2=$("$BIN" --config "$cfg_a" gc --apply --grace 1ns --json 2>/dev/null)
  assert_eq "$(jq -r --arg h "$sole_blob" '[.reclaimed[].hash] | index($h) != null' <<<"$sole_pass2")" "true" \
    "the second pass reclaims them: one peer means no elsewhere to satisfy, so the window is the whole gate"
  assert_eq "$(jq -r '.spared | length' <<<"$sole_pass2")" "0" \
    "nothing was spared — this collector is a gate, not a refusal to ever delete"
  assert_eq "$(jq -r '.refusals | length' <<<"$sole_pass2")" "0" \
    "and nothing stopped the sweep as a whole"
  assert_eq "$(peer_holds "$root/a/data/cas" "$sole_blob")" "0" "node A let the bytes go"

  # -------------------------------------------------------------------------
  note "  two peers, enrolled by public key in both directions (§26, ADR-0012)"
  # -------------------------------------------------------------------------
  #
  # Operator-mediated and explicit: no discovery, no join token, no trust on
  # first use. Two nodes, two commands, each carrying the other's key.
  local key_a key_b peer_b_id
  key_a=$(cli_a peers list --json | jq -r '.[] | select(.is_self) | .public_key')
  key_b=$(cli_b peers list --json | jq -r '.[] | select(.is_self) | .public_key')
  assert_contains "$key_a" "ed25519:" "each node has an Ed25519 identity of its own (M4-03)"

  peer_b_id=$(cli_a peers add --name site-b --site site-b --mode full \
    --public-key "$key_b" --endpoint "https://$addr_b" --json | jq -r '.id')

  # Peer health, before node B has said anything to node A (#184).
  #
  # `unknown` is the column's default and it is deliberately NOT a synonym for
  # reachable: a peer that has never been heard from has not been shown to be
  # up. Asserting it HERE is what makes the assertions after it mean something —
  # without it, "reachable" below would pass on a build where the column started
  # reachable and nothing ever moved it, which is precisely how #184 survived M4
  # with every test green.
  #
  # It has to sit between the two enrolments rather than after both, and the
  # reason is the mechanism itself. Node B's own `peers add` opens an
  # AUTHENTICATED mTLS request to node A's peer surface to ask about the return
  # path, and node A records liveness on exactly that — so by the line after the
  # next, the value has legitimately moved. (Node A's enrolment a moment ago did
  # not move node B's view for node A the same way, because at that point node B
  # had not enrolled node A and the handshake was refused. That refusal is the
  # "not verified in both directions" note on stderr, and it is correct.)
  #
  # assert_eq, not assert_contains: "unreachable" CONTAINS "reachable".
  local tp_health
  tp_health=$(cli_a peers list --json | jq -r '.[] | select(.name == "site-b") | .health')
  assert_eq "$tp_health" "unknown" \
    "a peer that has not been heard FROM is unknown — a dial OUT is not an observation of the far end (#184)"

  cli_b peers add --name site-a --site site-a --mode full \
    --public-key "$key_a" --endpoint "https://$addr_a" --json >/dev/null
  # Each node also records where IT can be reached, so it can report its own
  # inventory through the same door every other peer uses. A self peer reports
  # through the same mechanism as anyone else (M4-07); there is no privileged
  # in-process shortcut, because a call that could not survive being a network
  # hop is not allowed (invariant 4).
  cli_a peers add --name site-a --public-key "$key_a" --endpoint "https://$addr_a" --json >/dev/null
  cli_b peers add --name site-b --public-key "$key_b" --endpoint "https://$addr_b" --json >/dev/null

  # The ping answers with what the OTHER node made of this one's certificate,
  # which is the fact worth asserting: node B derived "site-a" from the key it
  # was handed, so the enrolment on both sides describes the same two machines.
  # Asserting on "site-b" here would only prove that the string this node typed
  # into the command came back.
  local tp_ping
  tp_ping=$(cli_a peers ping site-b --json 2>&1)
  assert_eq "$(jq -r '.name // "none"' <<<"$tp_ping")" "site-a" \
    "node B recognises node A's certificate as the peer it enrolled (ADR-0012, ADR-0033)"
  assert_contains "$tp_ping" "served_by" \
    "over pinned mTLS, with no second credential anywhere (ADR-0033)"
  # The ping went A -> B, so it is NODE B that just observed node A. Asserting
  # it from the dialling side would be asserting about the wrong node: liveness
  # is recorded by the end that was TALKED TO, which is the whole reason the
  # peer surface had to become one of its writers (#184).
  assert_eq "$(cli_b peers list --json | jq -r '.[] | select(.name == "site-a") | .health')" \
    "reachable" "node B observed node A on the connection A opened to its peer surface (#184)"


  # The field the milestone changed, answering differently on the SAME node it
  # answered `true` on ninety seconds ago. Nothing about the code moved between
  # these two assertions — a peer was enrolled.
  tp_json=$(api_a "/api/v1/desired/$tp_want/satisfaction")
  assert_eq "$(jq -r '.placement.unproven' <<<"$tp_json")" "false" \
    "unproven is FALSE with two required peers: the axis is answering a real question"

  # -------------------------------------------------------------------------
  note "  the inventories, exchanged (§19, §20, M4-07)"
  # -------------------------------------------------------------------------
  #
  # `replicas` on a controller is what the CONTROLLER believes; a peer's
  # inventory is what is on its DISK. Until B says what it holds, A knows only
  # that B is required to hold things — which is `converging`, correctly, and
  # for a reason that is about missing information rather than missing bytes.
  cli_a peers report-inventory site-b --json >/dev/null
  cli_b peers report-inventory site-a --json >/dev/null

  tp_json=$(api_a "/api/v1/desired/$tp_want/satisfaction")
  assert_eq "$(jq -r '.placement.satisfaction' <<<"$tp_json")" "satisfied" \
    "with both peers reporting the bytes, placement is satisfied on evidence"
  assert_eq "$(jq -r '.state' <<<"$tp_json")" "FULLY_SATISFIED" \
    "and the §64 name says so"
  assert_eq "$(jq -r '.placement.unproven' <<<"$tp_json")" "false" \
    "unproven does not come back when the answer is satisfied — it is about the target set"
  # THE TRANSITION. Node B reported its inventory to node A over the peer
  # surface — pinned mTLS, no bearer token anywhere near it — and that is the
  # interaction that actually happens between two peers. Before #184 nothing
  # observed it, and this value stayed "unknown" for the rest of the run.
  #
  # assert_eq, not assert_contains: "unreachable" CONTAINS "reachable".
  tp_health=$(cli_a peers list --json | jq -r '.[] | select(.name == "site-b") | .health')
  assert_eq "$tp_health" "reachable" \
    "THE TRANSITION: a peer that has spoken only to the peer surface is reachable (#184)"
  # last_seen_at is the actionable half. "reachable" with no timestamp is a
  # status nobody can act on, and a column that is set but never advanced would
  # pass the assertion above forever.
  assert_eq "$(cli_a peers list --json | jq -r '.[] | select(.name == "site-b") | (.last_seen_at != null)')" \
    "true" "and it records WHEN, which is the half an operator acts on"
  # -------------------------------------------------------------------------
  note "  a one-way pairing is REPORTED, not refused (#186, ADR-0037, ADR-0038)"
  # -------------------------------------------------------------------------
  #
  # Replication needs two flows and they run in OPPOSITE directions: a peer
  # PUSHES its inventory to the controller, and a destination PULLS bytes from
  # the source (ADR-0030). A link that carries one direction only cannot
  # converge in the other, and it fails SILENTLY — the controller is never told
  # the far node holds a blob, so reconciliation correctly emits nothing and
  # nothing is reported as wrong.
  #
  # 🔴 It is REPORTED and never refused, and that is ADR-0038 rather than a
  # softened ADR-0037. Each peer is authoritative for its own site: a node that
  # can be reached but cannot reach back still fetches what it lacks from the
  # peer it CAN reach, and both sites keep serving everything already on their
  # own disks. Refusing to enrol that would block a working configuration for
  # being unusual. What is lost is convergence in one direction, and an operator
  # who is told at the terminal can act on it.
  #
  # The first assertion is that a healthy pairing says NOTHING. A check that
  # always spoke would be a check nobody reads.
  local ow_quiet ow_broken ow_status
  ow_quiet=$(cli_a peers add --name site-b --site site-b --mode full \
    --public-key "$key_b" --endpoint "https://$addr_b" --json 2>&1 >/dev/null)
  assert_eq "$ow_quiet" "" \
    "a pairing verified in both directions enrols with nothing printed (#186)"

  # Now break ONE direction, for real. Node B's record of node A is moved to a
  # port that refuses connections, so when node A asks node B to reach back,
  # node B genuinely cannot. Node A can still reach node B throughout: that is
  # the observed asymmetry, reproduced on one host.
  #
  # Port 9 is discard: reserved, and refusing connections everywhere.
  cli_b peers add --name site-a --site site-a --mode full \
    --public-key "$key_a" --endpoint "https://127.0.0.1:9" --json >/dev/null 2>&1

  ow_status=0
  ow_broken=$(cli_a peers add --name site-b --site site-b --mode full \
    --public-key "$key_b" --endpoint "https://$addr_b" --json 2>&1 >/dev/null) || ow_status=$?
  assert_eq "$ow_status" "0" \
    "a one-way pairing is ENROLLED, not refused — each peer is authoritative for its own site (ADR-0038)"
  assert_contains "$ow_broken" "the return path did not answer" \
    "and the operator is TOLD, naming the direction that failed"
  assert_contains "$ow_broken" "127.0.0.1:9" \
    "naming the address the far node actually tried, so a stale record is distinguishable from a firewall"
  assert_contains "$ow_broken" "not a fault" \
    "stated as information rather than as a fault, because under ADR-0038 it is not one"
  assert_contains "$ow_broken" "ADR-0038" \
    "and it cites the record that says why, so an operator need not infer the stance"

  # The peer really was enrolled, and with the endpoint the operator typed. A
  # report that half-applied would leave a working endpoint replaced while the
  # peer looked healthy.
  assert_eq "$(cli_a peers show site-b --json 2>/dev/null | jq -r '.endpoint')" "https://$addr_b" \
    "the enrolment landed intact: node A's record of node B is exactly what was asked for"

  # Repair, so the rest of the phase runs against a fabric working both ways.
  # Load-bearing: without it every later assertion here runs against a node B
  # that cannot reach node A.
  cli_b peers add --name site-a --site site-a --mode full \
    --public-key "$key_a" --endpoint "https://$addr_a" --json >/dev/null 2>&1



  # -------------------------------------------------------------------------
  note "  PLACEMENT_CONVERGING, reached by a real gap in real bytes"
  # -------------------------------------------------------------------------
  #
  # THE MILESTONE'S HEADLINE ASSERTION. The blob is deleted from node B's
  # content store — really deleted, from a real disk — B reports what it now
  # holds, and node A's API is asked the §56 question again.
  local tp_blob tp_asset
  tp_asset=$(jq -r '.content.satisfied_by' <<<"$tp_json")
  tp_blob=$(api_a "/api/v1/assets/$tp_asset" | jq -r '.blob_hash')
  assert_contains "$tp_blob" "blake3:" "the satisfying asset names the bytes placement is about"

  # -------------------------------------------------------------------------
  note "  arming the observation: WHICH SURFACE serves the bytes (§21, §32, ADR-0030)"
  # -------------------------------------------------------------------------
  #
  # ADR-0030 puts the byte-carrying hop on the PEER SURFACE: the destination
  # pulls from the source's mTLS listener, and the client API — the controller's
  # door, the one that authenticates a bearer token — is not in the data path at
  # all. The claim after the transfer is that the bytes went peer to peer.
  #
  # It is asserted POSITIVELY, from the node that sent them. A negative
  # assertion — "this counter did not move" — is the weakest evidence in this
  # file: it passes against a metric that was never registered, a label that was
  # renamed, and, as this section learned in CI, against a perfectly live
  # counter that simply had a second, unrelated writer. See peer_blob_reads.
  #
  # Two controls arm it, and they point in opposite directions on purpose.
  local ctrl_a_before
  assert_eq "$(peer_blob_read_count "$log_a" GET "$tp_blob")" "0" \
    "node A has served these bytes to no peer yet: the record starts empty"

  # A genuine blob read on the CLIENT API, for the same blob, on a bearer token.
  # It must be visible to the client API's counter — that instrument is live —
  # and invisible to the peer surface's record. If a bearer-token read showed up
  # as a peer read, the two surfaces would not be distinguishable and every
  # assertion below would be measuring one number twice.
  ctrl_a_before=$(ctrl_blob_bytes "$(api_a /metrics)")
  assert_eq "$(api_a "/api/v1/blobs/$tp_blob/content" -H 'Range: bytes=0-1023' -o /dev/null -w '%{http_code}')" \
    "206" "node A's client API serves blob bytes to a bearer token, as it always has"
  assert_eq "$(( $(ctrl_blob_bytes "$(api_a /metrics)") - ctrl_a_before ))" "1" \
    "the client API's counter saw that read: it is a live instrument, not one that never existed"
  assert_eq "$(peer_blob_read_count "$log_a" GET "$tp_blob")" "0" \
    "and the peer surface recorded NOTHING for it — a bearer-token read is not a peer read"

  assert_eq "$(peer_holds "$root/b/data/cas" "$tp_blob")" "1" "node B holds the bytes before the gap is made"
  find "$root/b/data/cas/blobs" -name "${tp_blob#blake3:}" -type f -delete
  assert_eq "$(peer_holds "$root/b/data/cas" "$tp_blob")" "0" "and does not hold them after"

  cli_b peers report-inventory site-b --json >/dev/null
  cli_b peers report-inventory site-a --json >/dev/null

  tp_json=$(api_a "/api/v1/desired/$tp_want/satisfaction")
  # assert_eq, not assert_contains. "not_satisfied" contains "satisfied" and
  # this is the assertion the whole milestone is for.
  assert_eq "$(jq -r '.placement.satisfaction' <<<"$tp_json")" "converging" \
    "PLACEMENT_CONVERGING: one of two required peers holds the bytes (§56)"
  assert_eq "$(jq -r '.placement.missing | join(",")' <<<"$tp_json")" "$peer_b_id" \
    "and it names WHICH peer is missing them — converging with no list is unactionable"
  assert_eq "$(jq -r '.state' <<<"$tp_json")" "PLACEMENT_CONVERGING" \
    "the §64 state §56 draws this distinction for, reached by a running system"
  assert_eq "$(jq -r '.content.satisfaction' <<<"$tp_json")" "satisfied" \
    "content is still satisfied — the two axes are separate answers, and only one moved"

  # -------------------------------------------------------------------------
  note "  the transfer: the destination pulls, verifies, and then claims (M4-09)"
  # -------------------------------------------------------------------------
  api_b "/api/v1/peers/site-b/reconcile" -X POST -o /dev/null
  # WAIT FOR THE LAST OF THE THREE THINGS ASSERTED BELOW, not the first (#207).
  # internal/worker/replicateblob.go pulls the bytes, THEN records the transfer,
  # THEN logs "replicated a blob", and only then does the queue mark the job
  # succeeded. Waiting on the bytes landing left the job row and the log line —
  # both asserted below — still in flight, and a slow runner would have reported
  # a replication job "retried into silence" that had simply not finished.
  wait_for "the replicate_blob job never reached succeeded on node B — the bytes, the transfer record and the log line all land before it does" \
    900 replicate_job_succeeded
  assert_eq "$(peer_holds "$root/b/data/cas" "$tp_blob")" "1" \
    "the bytes crossed the wire and landed in node B's content store"
  assert_eq "$(api_b "/api/v1/jobs?type=replicate_blob" | jq -r '[.items[] | select(.state == "succeeded")] | length > 0')" "true" \
    "and a replicate_blob job succeeded rather than being retried into silence"
  # The destination's account of the transfer, by field rather than by the
  # presence of a log line: which blob, how many bytes, and that they were
  # actually pulled rather than found already present.
  local tp_rep tp_size
  tp_size=$(api_a "/api/v1/blobs/$tp_blob" | jq -r '.size')
  tp_rep=$(grep -F '"msg":"replicated a blob"' "$log_b" 2>/dev/null |
    jq -s --arg h "$tp_blob" '[.[] | select(.blob_hash == $h)] | last // {}')
  # `| tostring`, never `// "absent"`. jq's alternative operator fires on false
  # as well as on null, so `.deduplicated // "absent"` reports a successful
  # non-deduplicated transfer as a missing field — which is exactly what it did
  # the first time this ran. An absent field comes back as "null" and fails
  # loudly instead.
  assert_eq "$(jq -r '.blob_hash | tostring' <<<"$tp_rep")" "$tp_blob" \
    "node B recorded the transfer of exactly these bytes"
  assert_eq "$(jq -r '.bytes | tostring' <<<"$tp_rep")" "$tp_size" \
    "and the whole blob crossed — the byte count matches what the catalogue says it is"
  assert_eq "$(jq -r '.deduplicated | tostring' <<<"$tp_rep")" "false" \
    "pulled over the wire, not found already present: a dedupe would prove nothing about a transfer"

  # -------------------------------------------------------------------------
  # And now the surface, from the node that SENT the bytes.
  # -------------------------------------------------------------------------
  #
  # THE CONTROLLER CARRIED NO BYTES, asserted as the positive it actually is:
  # node A served this blob on its PEER SURFACE, by GET, to the peer whose
  # pinned certificate opened the connection. The whole blob is accounted for by
  # that read — node B pulled tp_size bytes and re-hashed them itself — so there
  # is nothing left over for the client API to have carried.
  #
  # This is the assertion CI corrected. It used to count the client API's route
  # label and require it not to move, which is a fact about ffprobe's absence on
  # a laptop rather than a fact about the data path. See peer_blob_reads.
  local tp_served
  tp_served=$(peer_blob_reads "$log_a" GET "$tp_blob" | sort -u | tr '\n' ',' | sed 's/,$//')
  assert_eq "$(( $(peer_blob_read_count "$log_a" GET "$tp_blob") >= 1 ))" "1" \
    "node A served these bytes on its peer surface — the record that was empty a moment ago is not now"
  assert_eq "$tp_served" "site-b" \
    "and to site-b alone: the only listener in the data path is the one a bearer token cannot open"
  # -------------------------------------------------------------------------
  note "  the byte saving, as a NUMBER (M5-06, M5-07, M5-08, M5-09)"
  # -------------------------------------------------------------------------
  #
  # 🔴 Milestone 5's thesis is a saving, and a milestone whose thesis is a
  # saving must assert the saving rather than describe it. This section is where
  # that arithmetic is done, out loud, in the demo.
  #
  # THE CONTROL FIRST, because a saving assertion with no control passes on a
  # transfer that fetched nothing at all. The transfer above moved a blob to a
  # peer holding NONE of it, and the number is stated as a percentage rather
  # than left as two byte counts a reader has to divide.
  local sv_moved sv_pct
  sv_moved=$(jq -r '.bytes | tostring' <<<"$tp_rep")
  sv_pct=$(( sv_moved * 100 / tp_size ))
  assert_eq "$sv_pct" "100" \
    "THE CONTROL: replicating to a peer that holds nothing moves 100% of the blob — $sv_moved of $tp_size bytes"
  assert_eq "$(cli_b blobs verify "$tp_blob" --json 2>/dev/null | jq -r '.verified | tostring')" "true" \
    "and every one of those bytes was re-hashed by the destination against the blob's own digest (invariant 1)"

  # 🔴 AND THE SAVING ITSELF IS ASSERTED, further down this same arc.
  #
  # It was not, until this milestone closed: resumption and chunk reuse were
  # not on `main`, and the peer-backed chunk source was not wired into
  # `fsck --repair`, so this file could assert the expensive case and nothing
  # else. #196's acceptance is that a milestone whose thesis is a saving must
  # assert the saving, and a control on its own is a milestone that measured
  # the wrong number.
  #
  # It is asserted where the fabric can be made to produce it deterministically
  # — the repair arc, after lazy chunking has given node A a manifest — and the
  # measurement is the same instrument as the control above: bytes the SOURCE
  # recorded serving, not bytes a destination claims it fetched.
  #
  # THREE of the four savings this milestone measured are still asserted in Go
  # rather than here, and that is a limitation rather than a preference:
  #
  #   - a resumed transfer moving 74.5% of a 512 KiB blob after a real SIGKILL,
  #     29 of 110 chunks kept;
  #   - a modified file moving 1.1% of its size to a peer holding the original;
  #   - 3 KiB prepended moving 1.2%.
  #
  # Each needs an interruption at a chosen point, or a second blob built to
  # share chunks with the first. From a shell the first means a test hook in
  # production code and the second moves catalogue counts this file asserts
  # elsewhere. They live in internal/peer/transfer, where the byte counts are
  # measured on the source's serving side exactly as they are here.


  cli_b peers report-inventory site-a --json >/dev/null
  tp_json=$(api_a "/api/v1/desired/$tp_want/satisfaction")
  assert_eq "$(jq -r '.placement.satisfaction' <<<"$tp_json")" "satisfied" \
    "FULLY_SATISFIED after the transfer: placement closed the gap it reported"
  assert_eq "$(jq -r '.state' <<<"$tp_json")" "FULLY_SATISFIED" \
    "and the §64 name completes the walk CONTENT_SATISFIED → PLACEMENT_CONVERGING → FULLY_SATISFIED"
  assert_eq "$(jq -r '.placement.unproven' <<<"$tp_json")" "false" \
    "on evidence, with unproven false throughout"
  # -------------------------------------------------------------------------
  note "  the manifest on the peer surface: a description, not a negotiation (M5-05, ADR-0034)"
  # -------------------------------------------------------------------------
  #
  # A destination may ask a source what its chunks are. The source answers with
  # what it stored and decides NOTHING — it is never told what the destination
  # holds, never asked what to send, and never computes a difference (ADR-0030).
  #
  # 🔴 And it never GENERATES. A GET that chunked a 20 GB blob to answer would
  # be a remote denial of service with a polite name, so §16's third state is
  # the ANSWER: a destination that is told "no manifest" pulls whole, which is
  # exactly what the transfer above just did.
  #
  # The route lives on the PEER listener and nowhere else. A member that may
  # read the bytes may read their description; a bearer token is not a peer
  # credential (ADR-0011, ADR-0012, ADR-0015).
  assert_contains "$(<api/openapi.yaml)" "/peer/v1/blobs/{hash}/manifest" \
    "the manifest route is documented on the PEER surface (ADR-0015)"
  assert_not_contains "$(<api/openapi.yaml)" "/api/v1/blobs/{hash}/manifest" \
    "and nowhere on the client API: a bearer token is not a peer credential"

  # The two manifest-less answers are DIFFERENT answers and a destination acts
  # differently on each — one says "pull these bytes whole from this same
  # source", the other says "there is nothing here at all, try another source".
  # Asserted on the `type` URI, which is the contract, and asserted to be
  # non-overlapping, because a contains-check on one must not match the other.
  assert_contains "$(<api/openapi.yaml)" "no-chunk-manifest" \
    "a blob the source HOLDS with no manifest has a problem type of its own"
  assert_not_contains "no-chunk-manifest" "not-found" \
    "and it is not a substring of the not-found type: the two do not overlap"

  # This route is CALLED in this run, and by a running fabric rather than by a
  # curl. It is not called here: at this point in the arc no blob on either
  # node has a manifest, which is why the transfer above took the whole path
  # and is the assertion directly below. The call happens further down, once
  # lazy chunking has given node A a manifest and node B has a gap — and the
  # source's record of what it served, and to whom, is asserted there.
  #
  # The 404 that names WHICH manifest-less state a source was in is still a Go
  # assertion (internal/api/peerapi): reaching it from here would mean asking a
  # source for a manifest of a blob it holds and has not chunked, which is a
  # request this fabric never makes on its own.


  # -------------------------------------------------------------------------
  note "  a blob on NEITHER peer is not_satisfied, not converging"
  # -------------------------------------------------------------------------
  #
  # The distinction EvaluatePlacement draws, against real rows: converging
  # means replication is closing a gap, and bytes on nobody are not converging
  # on anything. Made by deleting the OTHER fixture's bytes from both stores.
  local tp_work2 tp_want2 tp_asset2 tp_blob2 tp_json2
  tp_work2=$(api_a /api/v1/works | jq -r '.items[] | select(.title == "Static Field") | .id')
  tp_want2=$(api_a /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d "{\"work_id\":\"$tp_work2\",\"quality_profile_id\":\"$tp_profile\"}" | jq -r '.id')
  tp_asset2=$(api_a "/api/v1/desired/$tp_want2/satisfaction" | jq -r '.content.satisfied_by')
  tp_blob2=$(api_a "/api/v1/assets/$tp_asset2" | jq -r '.blob_hash')

  find "$root/a/data/cas/blobs" -name "${tp_blob2#blake3:}" -type f -delete
  find "$root/b/data/cas/blobs" -name "${tp_blob2#blake3:}" -type f -delete
  cli_a peers report-inventory site-a --json >/dev/null
  cli_b peers report-inventory site-b --json >/dev/null
  cli_b peers report-inventory site-a --json >/dev/null

  tp_json2=$(api_a "/api/v1/desired/$tp_want2/satisfaction")
  assert_eq "$(jq -r '.placement.satisfaction' <<<"$tp_json2")" "not_satisfied" \
    "bytes on no peer at all are not_satisfied — nowhere is not converging on anything"
  assert_eq "$(jq -r '.placement.missing | length' <<<"$tp_json2")" "2" \
    "and both required peers are named as missing them"

  # -------------------------------------------------------------------------
  # -------------------------------------------------------------------------
  note "  lazy chunking: a manifest is a job's product, never a lookup's (§16, §75, M5-04)"
  # -------------------------------------------------------------------------
  #
  # §16 makes chunking lazy and §75 has listed `chunk_blob` since Milestone 1
  # with nothing behind it. This is the handler, on a running fabric.
  #
  # # Placement
  #
  # Inside the two-peer arc, at the end of it, and ABOVE the garbage-collection
  # refusal only because that section stops node B. It ingests one small file,
  # so it DOES create a Work, an asset and a blob — but in this arc's own
  # isolated library, below every count this arc asserts, and nowhere near the
  # demo catalogue the CLI section counts. That is the trap recorded at
  # `note "  the CLI (M1-17)"`, and this is the side of it that is safe.
  #
  # # Why the assertions are all on node A
  #
  # Node A holds every byte and its convergence cycle is triggered here
  # explicitly. Node B's own cycle also enqueues chunking, for blobs that are
  # arriving as it runs, and asserting the result of that race would be
  # asserting a timing rather than a rule.
  # A SMALL blob is needed and there is not one left. The section above
  # deliberately deleted the second fixture's bytes from BOTH stores, and the
  # fabric-of-one collector reclaimed the third, so the only blob node A still
  # holds is the five-megabyte one that just crossed the wire. One is therefore
  # ingested here, through a real scan, which also means the ingest assertions
  # below are made against a file that arrived after this node started rather
  # than against fixtures the run has been carrying since the beginning.
  local lc_small lc_large
  lc_large=$tp_blob
  mkdir -p "$lib/movies/Short Cut (2020)"
  head -c 32768 /dev/urandom > "$lib/movies/Short Cut (2020)/Short.Cut.2020.1080p.mkv"
  #
  # `scan --wait` waits for the SCAN jobs, which enqueue the ingests; the asset
  # appears when the ingest lands, so the wait is on the asset rather than on
  # the command returning. That distinction is the same one wait_for_ingest
  # makes in the single-node demo, and getting it wrong here cost a run.
  cli_a scan films --wait --json >/dev/null 2>&1 || true
  waited=0
  while (( waited < 900 )); do
    lc_small=$(api_a /api/v1/assets |
      jq -r '.items[] | select(.source_path != null and (.source_path | contains("Short.Cut"))) | .blob_hash' |
      head -1)
    [[ -n "$lc_small" ]] && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  assert_contains "$lc_small" "blake3:" "the small fixture names bytes node A ingested"
  assert_eq "$(api_a "/api/v1/blobs/$lc_small" | jq -r '.size')" "32768" \
    "and it is a small blob: well under the 4 MiB threshold, unlike the one that just crossed the wire"
  assert_eq "$(( $(api_a "/api/v1/blobs/$lc_large" | jq -r '.size') > 4194304 ))" "1" \
    "while the transferred blob is above it — the two sides of the threshold, on one node"

  # THE ABSENCE FIRST, and it is measured before anything is asked to produce
  # one. Ingest is `file → BLAKE3 → blob available` and nothing else: no
  # manifest, and not even a job to make one.
  #
  # assert_eq on the state, never assert_contains. The three §16 answers were
  # chosen so that none is a substring of another, and a containment match here
  # would accept the opposite meaning.
  assert_eq "$(cli_a blobs stat "$lc_large" --json | jq -r '.chunk_manifest')" "undecided" \
    "ingest left the large blob UNDECIDED — chunking it eagerly would be a second full read of "\
"every byte ingested, for a manifest that may never be used (§16)"
  assert_eq "$(cli_a blobs stat "$lc_small" --json | jq -r '.chunk_manifest')" "undecided" \
    "and the small one too: 'never needs one' is a decision, not something ingest guesses"
  assert_eq "$(api_a "/api/v1/jobs?type=chunk_blob" | jq -r '.items | length')" "0" \
    "and no chunk_blob job exists at all — not even one waiting to run"

  # Asking must never generate (ADR-0034). Five reads of the state, and the
  # answer is still the one that means nobody has decided.
  local lc_ask
  for lc_ask in 1 2 3 4 5; do
    api_a "/api/v1/blobs/$lc_large" >/dev/null
    cli_a blobs stat "$lc_large" --json >/dev/null
  done
  assert_eq "$(cli_a blobs stat "$lc_large" --json | jq -r '.chunk_manifest')" "undecided" \
    "ten reads of 'does this blob have a manifest' produced none: asking is a READ (ADR-0034)"
  assert_eq "$(api_a "/api/v1/jobs?type=chunk_blob" | jq -r '.items | length')" "0" \
    "and enqueued nothing — a question that produces the answer it was asked cannot be asked"

  # Now something decides it wants one: a convergence cycle that has just
  # worked out these bytes must cross a network. That is §16's own trigger —
  # "when replication or deduplication requires it" — and it is the only thing
  # that enqueues this job. There is no sweep.
  #
  # The same cycle also queues the TRANSFERS that close those gaps, with node B
  # as the destination, and node A refuses to run those — a destination pulls
  # its own bytes (ADR-0030). That is not something this section introduces:
  # every node's own five-minute beat computes the same gaps for every peer,
  # and it is why the chunking half is keyed on the blob alone rather than on
  # the transfer.
  # Only the large blob has to be taken away: node B has never seen the small
  # one — it was ingested into this library a moment ago and node A is the only
  # node that has scanned it — so it is already a gap, and one this section did
  # not have to manufacture.
  find "$root/b/data/cas/blobs" -name "${lc_large#blake3:}" -type f -delete
  # Node B walks its own store first and only then tells node A, which is the
  # order the gap arc above uses: a peer's inventory is what is on its DISK,
  # and reporting to A without re-reading B's own would forward a belief B has
  # not itself re-checked.
  cli_b peers report-inventory site-b --json >/dev/null
  cli_b peers report-inventory site-a --json >/dev/null
  api_a "/api/v1/peers/site-b/reconcile" -X POST -o /dev/null

  # Waited on the CONDITION rather than on a job count: the claim is that both
  # blobs stop being undecided, and a count of succeeded jobs can be satisfied
  # by jobs about other blobs entirely.
  waited=0
  while (( waited < 900 )); do
    [[ "$(cli_a blobs stat "$lc_large" --json | jq -r '.chunk_manifest')" != "undecided" &&
       "$(cli_a blobs stat "$lc_small" --json | jq -r '.chunk_manifest')" != "undecided" ]] && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  assert_eq "$(( $(api_a "/api/v1/jobs?type=chunk_blob" | jq -r '[.items[] | select(.state == "succeeded")] | length') >= 2 ))" "1" \
    "the cycle that decided the bytes must move enqueued the chunking, and it ran"
  assert_eq "$(api_a "/api/v1/jobs?type=chunk_blob" | jq -r '[.items[] | select(.state == "failed")] | length')" "0" \
    "and no chunking failed — a job that gives up is a manifest nobody will make"

  # The two sides of the threshold, on the same node, in the same pass.
  assert_eq "$(cli_a blobs stat "$lc_large" --json | jq -r '.chunk_manifest')" "present" \
    "the large blob has a manifest, generated on demand by a job (§16)"
  assert_eq "$(cli_a blobs stat "$lc_small" --json | jq -r '.chunk_manifest')" "not_required" \
    "and the small one is recorded as NEVER NEEDING one, rather than left undecided — "\
"below the threshold the manifest costs the same full read as the transfer it would optimise"
  assert_eq "$(api_a "/api/v1/blobs/$lc_small" | jq -r '.chunked')" "false" \
    "the compatibility boolean says false for it, which is exactly the conflation §16 needed a third state for"
  assert_eq "$(api_a "/api/v1/blobs/$lc_large" | jq -r '.chunked')" "true" \
    "and true for the blob that actually has one"

  # Idempotent (invariant 9). A second cycle over the same fabric enqueues
  # nothing new, and nothing about the recorded answers moves.
  api_a "/api/v1/peers/site-b/reconcile" -X POST -o /dev/null
  sleep 0.5
  assert_eq "$(cli_a blobs stat "$lc_large" --json | jq -r '.chunk_manifest')" "present" \
    "a second cycle left the manifest alone"
  assert_eq "$(cli_a blobs stat "$lc_small" --json | jq -r '.chunk_manifest')" "not_required" \
    "and left the recorded exemption alone — a re-run that changes nothing must also SAY nothing"
  # THE ASSERTION M5-03 EXISTS FOR: the two states a boolean collapsed together
  # are not the same string. Written as an explicit inequality rather than left
  # to be inferred from the two assertions above, because those two would both
  # pass if the names had been made the same value.
  if [[ "$(cli_a blobs stat "$lc_small" --json | jq -r '.chunk_manifest')" \
     == "$(cli_a blobs stat "$lc_large" --json | jq -r '.chunk_manifest')" ]]; then
    fail "'never needs a manifest' and 'has one' report the same state — that is the boolean again"
  else
    pass "the states blobs.chunked collapsed together are distinguishable in one read (M5-03)"
  fi


  # Put the fabric back the way this section found it. The gap above was made
  # by deleting bytes from node B, and the garbage-collection refusal below
  # turns on node A believing node B holds them — a peer that is UNREACHABLE is
  # a different refusal from a peer that has reported not having the bytes, and
  # leaving this section's gap in place would quietly change which one is being
  # asserted. It is closed by the real mechanism rather than by copying files
  # about: node B reconciles, pulls, and reports what it now holds.
  #
  # It is also the first transfer in this run that the SOURCE can describe:
  # node A has a manifest for these bytes now (asserted directly above) and
  # node B holds none of them, so this pull takes the CHUNKED path, and what it
  # costs is measured on node A's serving side either side of it.
  local ctl_before ctl_moved ctl_size ctl_manifests_before
  ctl_size=$(api_a "/api/v1/blobs/$lc_large" | jq -r '.size')
  ctl_before=$(peer_served_bytes "$log_a" "$lc_large")
  ctl_manifests_before=$(grep -cF '"msg":"served a chunk manifest to a peer"' "$log_a" 2>/dev/null || true)
  api_b "/api/v1/peers/site-b/reconcile" -X POST -o /dev/null
  waited=0
  while (( waited < 900 )); do
    (( $(peer_holds "$root/b/data/cas" "$lc_large") == 1 )) && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  cli_b peers report-inventory site-b --json >/dev/null
  cli_b peers report-inventory site-a --json >/dev/null
  assert_eq "$(peer_holds "$root/b/data/cas" "$lc_large")" "1" \
    "and the fabric is converged again, on the same transfer path, before the refusal below is asked for"
  ctl_moved=$(( $(peer_served_bytes "$log_a" "$lc_large") - ctl_before ))

  # THE MANIFEST, OVER THE WIRE. Until this pull, every transfer in this run
  # was of a blob nothing had chunked, so the route existed and nothing had
  # ever called it. This one is a destination asking a source to describe bytes
  # it is about to fetch, over mTLS, and the source recording what it served
  # and to whom.
  assert_eq "$(( $(grep -cF '"msg":"served a chunk manifest to a peer"' "$log_a" 2>/dev/null || true) \
      > ctl_manifests_before ))" "1" \
    "a destination read the source's chunk manifest over the peer surface before pulling — the \
route is no longer one nothing calls (M5-05, ADR-0034)"

  # 🔴 THE CONTROL, as a number, on the chunked path.
  #
  # Milestone 5's thesis is a saving, and a saving assertion with no control
  # passes on a transfer that fetched NOTHING at all. This is that control: the
  # destination held none of these bytes, so the chunked path had nothing to
  # reuse and had to move all of them. If this ever reads materially under 100%
  # the saving asserted below is measuring an empty transfer.
  assert_eq "$(pct "$ctl_moved" "$ctl_size")" "100" \
    "THE CONTROL: a chunked pull to a peer holding NONE of the blob moves 100% of it — \
$ctl_moved of $ctl_size bytes, measured on the SOURCE's serving side"
  assert_eq "$(cli_b blobs verify "$lc_large" --json 2>/dev/null | jq -r '.verified | tostring')" "true" \
    "and every one of those bytes was re-hashed by the destination against the blob's own digest (invariant 1)"

  # -------------------------------------------------------------------------
  note "  🔴 THE SAVING, as a NUMBER: a repair fetches the damage, not the blob (M5-08, M5-09)"
  # -------------------------------------------------------------------------
  #
  # #196's acceptance in its own words: a milestone whose thesis is a saving
  # must ASSERT the saving. Everything above this line is the control, and a
  # control on its own is a milestone that measured the expensive case.
  #
  # This is the cheap case, driven end to end on the running fabric: node A's
  # copy of a five-megabyte blob is damaged in ONE chunk, and `fsck --repair`
  # rebuilds it from node B. Node A already holds every chunk but that one, so
  # the only bytes that may cross the wire are the damaged chunk's — and what
  # crosses is counted on NODE B, the machine that sent them.
  #
  # Repair rather than a resumed or reusing transfer, and that choice is worth
  # stating. All three demonstrate the same saving. Only this one is
  # DETERMINISTIC from a shell: a resumed transfer needs an interruption at a
  # chosen point, and interrupting a transfer from outside means either a test
  # hook in production code or a race with a five-megabyte loopback copy. A
  # flaky assertion about a saving is worse than an honest paragraph saying the
  # saving is asserted in Go — so the other three stay in Go, and the epilogue
  # says which one this run drove.
  local rp_size rp_file rp_off rp_before rp_moved rp_json rp_entry
  rp_size=$ctl_size
  # -print -quit rather than `| head -1`: this file runs under `pipefail`, and
  # a find killed by the SIGPIPE `head` sends it makes the whole pipeline
  # non-zero, which under `set -e` ends the run with no output at all.
  rp_file=$(find "$root/a/data/cas/blobs" -name "${lc_large#blake3:}" -type f -print -quit)
  if [[ ! -f "$rp_file" ]]; then
    fail "the blob to damage is not in node A's store, so the repair below would measure nothing"
  fi
  # Damaged in the MIDDLE, not at the start: a chunker's first boundary is the
  # one most likely to be shared by accident, and damage at offset 0 would let
  # a repair that re-fetched a fixed prefix look chunk-scoped.
  # chmod first: a published blob is read-only, which is the store protecting
  # its own bytes from exactly this. Damaging it means stepping around that
  # deliberately, the way the single-node repair section already does.
  #
  # ONE BYTE, and the width is the point rather than a minimum.
  #
  # This wrote a 4096-byte block until it hit CI. A write wider than one byte
  # can STRADDLE a content-defined chunk boundary, and then two chunks are
  # damaged and two are fetched. At the old five-megabyte fixture that was
  # three chunks and effectively unreachable; at a hundred megabytes it is
  # eighty-odd chunks, boundaries are close enough together to land on, and it
  # failed on `main` with `chunks_fetched — got '2', want '1'`. A single byte
  # cannot span a boundary, so the damage is exactly one chunk every time.
  #
  # And the byte written is the ORIGINAL byte INVERTED, not a random one.
  # `dd if=/dev/urandom … count=1` writes one random byte, which has a 1-in-256
  # chance of being the byte already there — leaving the file unchanged, the
  # blob still verifying, and this section failing with
  # `the blob is damaged … got 'true', want 'false'`. That is a 0.4% flake per
  # run, which is rare enough to look like something else and frequent enough
  # to hit: it did, on CI, once. Inverting the byte that is there cannot
  # produce the byte that is there.
  rp_off=$(( rp_size / 2 ))
  chmod u+w "$rp_file"
  rp_orig=$(dd if="$rp_file" bs=1 skip="$rp_off" count=1 2>/dev/null | od -An -tu1 | tr -d ' ')
  printf "$(printf '\\%03o' "$(( rp_orig ^ 255 ))")" \
    | dd of="$rp_file" bs=1 seek="$rp_off" count=1 conv=notrunc 2>/dev/null
  # And CHECK the damage landed, before asserting anything about repair.
  #
  # The write's stderr is discarded — it is noisy — so a write that did not
  # happen at all would look exactly like a write that changed nothing, and the
  # section would fail three assertions later complaining about repair. This
  # reads the byte back and fails HERE, naming the step that did not work.
  rp_now=$(dd if="$rp_file" bs=1 skip="$rp_off" count=1 2>/dev/null | od -An -tu1 | tr -d ' ')
  if [[ "$rp_now" == "$rp_orig" ]]; then
    fail "the damage did not land: byte $rp_off of the blob is still $rp_orig, so nothing below measures a repair"
  fi
  assert_eq "$(cli_a blobs verify "$lc_large" --json 2>/dev/null | jq -r '.verified | tostring')" "false" \
    "the blob is damaged on node A: it no longer hashes to its own name"

  rp_before=$(peer_served_bytes "$log_b" "$lc_large")
  # ONE repair pass, reported as JSON. The enum is the diagnosis: every refusal
  # this can end in — no_manifest, unreachable, source_corrupt — is a different
  # sentence about a different thing, and asserting "the blob verifies" alone
  # would report all of them as the same silence.
  # "$BIN" directly rather than cli_a: fsck talks to the database and the store,
  # not to the API, so it takes no --token (ADR-0002 — it has to work when the
  # controller will not start, which is precisely when someone reaches for it).
  rp_json=$("$BIN" --config "$cfg_a" fsck --deep --repair --json 2>/dev/null || true)
  rp_moved=$(( $(peer_served_bytes "$log_b" "$lc_large") - rp_before ))
  rp_entry=$(jq -c --arg h "$lc_large" '[.repairs[] | select(.hash == $h)][0] // {}' <<<"$rp_json")

  assert_eq "$(jq -r '.outcome | tostring' <<<"$rp_entry")" "repaired" \
    "the damaged blob was REPAIRED from a peer — the outcome enum, so a refusal names ITSELF \
rather than arriving as a blob that still does not verify (ADR-0036)"
  assert_eq "$(cli_a blobs verify "$lc_large" --json 2>/dev/null | jq -r '.verified | tostring')" "true" \
    "and it hashes to its own name again: the replacement was verified WHOLE before publication, \
never written in place (invariant 1, ADR-0036)"

  # 🔴 THE NUMBER, from TWO instruments that do not share a code path.
  #
  # `bytes_fetched` is the repairer's own account of what it pulled. The delta
  # is what node B recorded SERVING. A repairer that under-reported, and a
  # source that over-reported, would each be invisible on its own; neither
  # survives having to agree with the other.
  local rp_reported rp_total
  rp_reported=$(jq -r '.bytes_fetched | tostring' <<<"$rp_entry")
  rp_total=$(jq -r '.chunks_total | tostring' <<<"$rp_entry")

  # THE SAVING IS ASSERTED IN CHUNKS, and the bytes are reported beside it.
  #
  # That is not a softer claim, it is the fixture-independent one. At the
  # chunker's shipped parameters — 256 KiB minimum, 1 MiB average, 4 MiB
  # maximum — a five-megabyte blob is about FIVE chunks, so one chunk of it is
  # a fifth, and the measured figure swings between roughly 8% and 23% run to
  # run purely with where a content-defined boundary happens to fall in random
  # bytes. A percentage threshold here would be asserting the fixture's size,
  # and it would go red on a chunker re-tuning that is not a regression.
  #
  # What the feature actually claims is that a repair costs ONE CHUNK rather
  # than one blob, and that is exact: fetched over total. The dramatic ratios
  # need a blob with many chunks, and a blob with many chunks does not fit a
  # 240-second acceptance budget — which the epilogue says out loud rather than
  # letting this section imply otherwise.
  # THE CLAIM IS fetched == damaged, not fetched == 1.
  #
  # "Exactly one chunk" was a statement about the fixture's damage, and it went
  # red on `main` the moment the fixture was big enough for a wide write to
  # straddle a boundary. What the feature actually claims is that a repair
  # fetches THE DAMAGE and nothing else, and that is what is asserted — so a
  # repair that quietly pulled a spare chunk still fails, and a future change
  # to how the damage is made cannot silently weaken it.
  local rp_damaged
  rp_damaged=$(jq -r '.chunks_damaged | tostring' <<<"$rp_entry")
  assert_eq "$(jq -r '.chunks_fetched | tostring' <<<"$rp_entry")" "$rp_damaged" \
    "🔴 THE SAVING: the repair fetched exactly the $rp_damaged damaged chunk(s) of the $rp_total \
this blob has, and nothing else — $rp_moved bytes of $rp_size, $(pct "$rp_moved" "$rp_size")% — \
against the 100% control above, both measured on the SOURCE's serving side (#196, ADR-0036)"
  # And the damage really is a SMALL part of the blob, without which
  # "fetched == damaged" is satisfied by a blob that was damaged end to end.
  assert_eq "$(( rp_damaged >= 1 && rp_damaged * 10 < rp_total ))" "1" \
    "and the damage is a small fraction of the blob — $rp_damaged chunk(s) of $rp_total — so \
this is a saving rather than an arithmetic identity"
  assert_eq "$(( rp_total > 1 ))" "1" \
    "and the blob has MORE than one chunk ($rp_total), without which fetching one of them is not \
a saving and this section would pass on a single-chunk blob"
  assert_eq "$(( rp_moved > 0 ))" "1" \
    "it crossed the WIRE: node B served $rp_moved bytes. A repair that fetched nothing repaired \
nothing, and would report the best saving in this file"
  assert_eq "$rp_reported" "$rp_moved" \
    "and the two ends agree — the repairer says it fetched $rp_reported bytes and the source says \
it served $rp_moved"
  assert_eq "$(( rp_moved < rp_size ))" "1" \
    "and it is materially less than the blob: $rp_moved of $rp_size bytes"

  # The repair settled, asserted about THIS blob rather than about the whole
  # store. A second pass may legitimately report other blobs — this arc leaves
  # unreferenced and deleted bytes behind on purpose — and a global exit code
  # would make this section fail for somebody else's fixture.
  local rp_second
  rp_second=$("$BIN" --config "$cfg_a" fsck --deep --repair --json 2>/dev/null || true)
  assert_eq "$(jq -r --arg h "$lc_large" '[.repairs[]? | select(.hash == $h)] | length' <<<"$rp_second")" "0" \
    "a second pass finds nothing to repair for these bytes — which is what tells a real repair \
apart from one that republished the damage"

  # The damaged original is preserved, never deleted (ADR-0018). It is then
  # removed HERE, by this script, so the sections below count the store they
  # expect: quarantine is evidence for an operator, and leaving it would make
  # this section's cost show up in somebody else's assertion.
  local rp_q
  rp_q=$(find "$root/a/data/cas/quarantine" -type f 2>/dev/null | wc -l | tr -d ' ')
  assert_eq "$(( rp_q >= 1 ))" "1" \
    "the damaged original was QUARANTINED rather than deleted — on a hardlink-ingested library \
the corruption may be the operator's own file (ADR-0018)"
  find "$root/a/data/cas/quarantine" -type f -delete 2>/dev/null || true
  # ADR-0034's falsification, asserted about transfers that have ALREADY
  # happened rather than by deleting rows out of a database. Every blob in this
  # arc was `undecided` when the five-megabyte blob first crossed the wire a
  # hundred lines above — no manifest on either node, and no chunk_blob job at
  # all, both asserted at the time. The bytes crossed anyway, node B re-hashed
  # them itself, and no replication job failed.
  #
  # That is ADR-0034's condition in its own words: if deleting every manifest in
  # the store broke anything other than efficiency, the line would have been
  # crossed. A store with no manifests is the state this run spent its whole
  # transfer arc in.
  assert_eq "$(api_b "/api/v1/jobs?type=replicate_blob" | jq -r '[.items[] | select(.state == "failed")] | length')" "0" \
    "no replication has failed for want of a manifest: a manifest is an optimisation, never a precondition (ADR-0034)"
  assert_eq "$(cli_b blobs verify "$tp_blob" --json 2>/dev/null | jq -r '.verified | tostring')" "true" \
    "and the blob that crossed with no manifest anywhere verifies to its own whole-object digest on the destination"

  # -------------------------------------------------------------------------
  note "  garbage collection catches a LYING replica row (Refusal 3, #184)"
  # -------------------------------------------------------------------------
  #
  # The refusal that needs a peer to ANSWER, and the one that lived only in Go
  # until #184 made a remote peer's health able to move.
  #
  # The refusal below this one is the easy half: node B is stopped, nothing
  # answers, and nothing about where the bytes are can be established. This is
  # the other one. Node B is UP and reachable. Its `replicas` row for the blob
  # says `present` and was confirmed moments ago, so the catalog's belief is
  # fresh and positive. The bytes are then deleted from node B's disk WITHOUT B
  # reporting its inventory again — which is the scenario ADR-0018 is about: a
  # peer that lost a disk, restored an older CAS, or quarantined a blob, leaving
  # a row that still says `present`. Node A is then asked to reclaim its last
  # copy.
  #
  # Before #184 this path was unreachable from a shell. Reachable() is consulted
  # before the dial, and a remote peer's health was pinned at `unknown`, so the
  # sweep refused with `peer_unreachable` before it ever asked.
  #
  # It uses the SMALL blob, not the transferred one, and that is deliberate:
  # this section leaves its blob unreferenced with a `missing` row on B, and the
  # refusal below turns on node A believing node B still holds ITS blob. Sharing
  # a blob between the two would quietly change which refusal the section below
  # is asserting — from peer_unreachable (rank 3) to replica_not_present
  # (rank 1) — and it would pass either way.
  local r3_json r3_before r3_after r3_assets
  # Node B has to genuinely HOLD these bytes for the lie to be a lie, and until
  # now it has not: the small fixture was written into the shared library
  # directory a moment ago and only node A has scanned it. Node B ingests it
  # from the same file, by its own scan — content addressing gives the two
  # catalogues the same digest without either being told about the other.
  #
  # This is deliberately BELOW the chunking assertions above rather than beside
  # the scan that made the file. Those assertions turn on the small blob being a
  # gap for node B; ingesting it here would close that gap and quietly change
  # what they prove.
  cli_b scan films --wait --json >/dev/null 2>&1 || true
  waited=0
  while (( waited < 900 )); do
    (( $(peer_holds "$root/b/data/cas" "$lc_small") == 1 )) && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  cli_b peers report-inventory site-b --json >/dev/null
  cli_b peers report-inventory site-a --json >/dev/null

  # Node B is reachable, and it is asserted rather than assumed: this refusal is
  # only distinguishable from peer_unreachable if the peer is actually up.
  # assert_eq, never assert_contains — "unreachable" CONTAINS "reachable".
  assert_eq "$(cli_a peers list --json | jq -r '.[] | select(.name == "site-b") | .health')" \
    "reachable" "node B is reachable, so a refusal here cannot be silence wearing a lie's clothes"

  # The catalog's BELIEF: present, and fresh, because B reported it moments ago.
  assert_eq "$(api_a "/api/v1/replicas?state=present" |
    jq -r --arg h "$lc_small" --arg p "$peer_b_id" \
      '[.items[] | select(.blob_hash == $h and .peer_id == $p)] | length')" "1" \
    "the catalog believes node B holds these bytes, and says so in a fresh row"

  # And now the divergence. Deleted from B's disk, and B is NOT asked to report
  # again — the row keeps saying present, which is exactly the lie.
  assert_eq "$(peer_holds "$root/b/data/cas" "$lc_small")" "1" "node B holds the bytes before the lie is made"
  find "$root/b/data/cas/blobs" -name "${lc_small#blake3:}" -type f -delete
  assert_eq "$(peer_holds "$root/b/data/cas" "$lc_small")" "0" "and does not hold them after"

  # Unreferenced on node A, so it is garbage by every LOCAL measure.
  r3_assets=$(api_a /api/v1/assets | jq -r --arg h "$lc_small" '.items[] | select(.blob_hash == $h) | .id')
  for aid in $r3_assets; do api_a "/api/v1/assets/$aid" -X DELETE -o /dev/null; done
  assert_eq "$(api_a /api/v1/assets | jq -r --arg h "$lc_small" '[.items[] | select(.blob_hash == $h)] | length')" "0" \
    "the blob is unreferenced on node A, and eligible by every LOCAL measure"

  r3_before=$(find "$root/a/data/cas" -type f | wc -l | tr -d ' ')
  # Marking pass, then the pass that would reclaim.
  "$BIN" --config "$cfg_a" gc --apply --grace 1ns --json >/dev/null 2>&1

  r3_json=$("$BIN" --config "$cfg_a" gc --apply --grace 1ns --json 2>/dev/null)
  r3_after=$(find "$root/a/data/cas" -type f | wc -l | tr -d ' ')

  assert_eq "$(jq -r '.reclaimed | length' <<<"$r3_json")" "0" "nothing was reclaimed"
  assert_eq "$r3_after" "$r3_before" "and nothing left the store: the file count is unchanged"
  assert_eq "$(peer_holds "$root/a/data/cas" "$lc_small")" "1" \
    "THE LAST COPY IS STILL THERE — a lying row did not cost the fabric its only copy"
  assert_eq "$(jq -r '[.spared[] | select(.hash == "'"$lc_small"'")] | length' <<<"$r3_json")" "1" \
    "the blob whose last local copy was at stake was spared"
  # THE ASSERTION THIS SECTION EXISTS FOR. assert_eq, never assert_contains:
  # every reason in this enum shares words with its neighbours, and
  # "peer_unreachable" is the answer this must NOT be — that is the easy half,
  # and it is the one the demo could already prove.
  assert_eq "$(jq -r '[.spared[] | select(.hash == "'"$lc_small"'")][0].reason' <<<"$r3_json")" "remote_lacks_blob" \
    "REFUSAL 3: the row said present, the peer was ASKED, and it answered that it does not hold them"
  assert_eq "$(jq -r '[.spared[] | select(.hash == "'"$lc_small"'")][0].peer_name' <<<"$r3_json")" "site-b" \
    "and it names WHICH peer's claim did not survive being checked"

  # The correction, which is the half that stops the next sweep rediscovering
  # the same lie. The row is not deleted — a peer that lost bytes must stay
  # VISIBLE — it is moved to `missing`.
  assert_eq "$(api_a "/api/v1/replicas?state=missing" |
    jq -r --arg h "$lc_small" --arg p "$peer_b_id" \
      '[.items[] | select(.blob_hash == $h and .peer_id == $p)] | length')" "1" \
    "and the lying row was CORRECTED to missing, so the next sweep does not have to discover it again"
  assert_eq "$(api_a "/api/v1/replicas?state=present" |
    jq -r --arg h "$lc_small" --arg p "$peer_b_id" \
      '[.items[] | select(.blob_hash == $h and .peer_id == $p)] | length')" "0" \
    "the present claim is gone: correcting it means the belief changed, not that a second row appeared"

  # The operator at a terminal, on the NEXT sweep — and the reason has changed,
  # which is the correction doing its job rather than a weaker assertion.
  #
  # This refusal corrects the row it caught, so the lie survives exactly one
  # reclaiming pass. The next pass reads a row that now says `missing` — a peer
  # claiming NOT to hold the bytes — and spares them on that alone, without
  # dialling anybody. Asserting `remote_lacks_blob` here would be asserting that
  # the correction did NOT happen, and it would pass while the feature was
  # broken. `replica_not_present` is the assertion that the correction landed
  # somewhere the collector reads.
  assert_eq "$(grep -cF "spared        $lc_small  replica_not_present" \
    <<<"$("$BIN" --config "$cfg_a" gc --apply --grace 1ns 2>/dev/null)" | tr -d ' ')" "1" \
    "the next sweep spares the same blob on the CORRECTED row, without asking the peer again"


  # -------------------------------------------------------------------------
  note "  a peer pushes its control plane to the one that trusts it (§50, ADR-0046, M7-03)"
  # -------------------------------------------------------------------------
  #
  # §50: a control-plane backup is not kept only where it was taken. Node B takes
  # a signed snapshot of its own control plane and pushes it to every Full Peer
  # that trusts it — here, node A. This is the half that makes the recovery at the
  # end of this arc possible: a peer can only be rebuilt from a backup some OTHER
  # peer already holds, and nothing PULLS a control plane (ADR-0046), so it has to
  # have been pushed across before the disk was lost.
  #
  # The facts node B must come back with are captured now, while it still has
  # them: its own peer id, and its catalogue of works. Works are control-DATABASE
  # rows, not content, so a restore is the only thing that returns them and a
  # convergence cycle never could — which is what makes them the right witness
  # that a control plane came back rather than a disk being re-filled.
  local b_self_id b_works_before tp_push tp_push_gen
  b_self_id=$(cli_b peers list --json | jq -r '.[] | select(.is_self) | .id')
  b_works_before=$(api_b /api/v1/works | jq -r '[.items[].title] | sort | join(",")')

  tp_push=$("$BIN" --config "$cfg_b" backup push --json 2>>"$log_b")
  assert_eq "$(jq -r '[.peers[] | select(.name == "site-a") | .ok] | first' <<<"$tp_push")" "true" \
    "the control-plane backup crossed to the peer that trusts this one"
  tp_push_gen=$(jq -r '.peers[] | select(.name == "site-a") | .held_generation' <<<"$tp_push")
  if [[ "$tp_push_gen" =~ ^[0-9]+$ ]] && (( tp_push_gen > 0 )); then
    pass "and node A reports the generation it now holds ($tp_push_gen), answering from its own side of the push"
  else
    fail "node A did not report a generation held after the push: $tp_push"
  fi

  # -------------------------------------------------------------------------
  note "  garbage collection REFUSES to delete the last copy (ADR-0018, §53, M4-12)"
  # -------------------------------------------------------------------------
  #
  # THE MILESTONE'S OTHER HEADLINE, and the one an operator most needs to have
  # seen work.
  #
  # Node B is STOPPED first, and waited for, so that "the peer is gone" is a
  # fact rather than a signal that has been sent. Then the blob that just
  # crossed the wire is unreferenced on node A, which makes it garbage by every
  # LOCAL measure the first three milestones had: nothing points at it, and the
  # grace window is set to a nanosecond so age cannot be what saves it.
  #
  # Before this milestone that combination unlinked the bytes. The fabric's last
  # copy of a blob and an orphan from a rolled-back ingest look identical from
  # inside one node's catalog, and ADR-0018 deferred the difference to here.
  #
  # The contrast that makes this mean something is a hundred lines above: the
  # SAME collector, on the SAME node, unlinked a blob when this was a fabric of
  # one. Nothing in between changed the code. A peer was enrolled, and then it
  # went away.
  local gc_before gc_after gc_assets gc_refuse gc_text
  stop_peer_node "${pids_b[@]}"
  pids_b=()

  gc_assets=$(api_a /api/v1/assets | jq -r --arg h "$tp_blob" '.items[] | select(.blob_hash == $h) | .id')
  for aid in $gc_assets; do api_a "/api/v1/assets/$aid" -X DELETE -o /dev/null; done
  assert_eq "$(api_a /api/v1/assets | jq -r --arg h "$tp_blob" '[.items[] | select(.blob_hash == $h)] | length')" "0" \
    "the transferred blob is unreferenced on node A, and eligible by every LOCAL measure"

  gc_before=$(find "$root/a/data/cas" -type f | wc -l | tr -d ' ')
  # Marking pass, then the pass that would reclaim.
  "$BIN" --config "$cfg_a" gc --apply --grace 1ns --json >/dev/null 2>&1
  gc_refuse=$("$BIN" --config "$cfg_a" gc --apply --grace 1ns --json 2>/dev/null)
  gc_after=$(find "$root/a/data/cas" -type f | wc -l | tr -d ' ')

  assert_eq "$(jq -r '.reclaimed | length' <<<"$gc_refuse")" "0" "nothing was reclaimed"
  assert_eq "$gc_after" "$gc_before" "and nothing left the store: the file count is unchanged"
  assert_eq "$(peer_holds "$root/a/data/cas" "$tp_blob")" "1" \
    "THE LAST COPY IS STILL THERE — this is the assertion the milestone is for"
  # Named rather than counted. Until Refusal 3 was folded in above, this could
  # assert "exactly one blob was spared" and mean "the sweep did something"; that
  # section now leaves a second unreferenced blob behind, whose own row it
  # corrected to `missing`. Counting would fail on a change that made this
  # section MORE thorough, so the blob at stake is named and the count is only
  # asked to be non-zero.
  assert_eq "$(( $(jq -r '.spared | length' <<<"$gc_refuse") >= 1 ))" "1" \
    "at least one blob was spared, so this is a refusal and not a sweep that did nothing"
  assert_eq "$(jq -r '[.spared[] | select(.hash == "'"$tp_blob"'")] | length' <<<"$gc_refuse")" "1" \
    "and the blob whose last local copy was at stake is one of them"
  # assert_eq, never assert_contains: "no_other_peer" CONTAINS "other_peer",
  # and a substring match on an enum-like value has shipped here once already.
  assert_eq "$(jq -r '[.spared[] | select(.hash == "'"$tp_blob"'")][0].reason' <<<"$gc_refuse")" "peer_unreachable" \
    "the reason is that nothing about where these bytes are could be established"
  assert_eq "$(jq -r '[.spared[] | select(.hash == "'"$tp_blob"'")][0].peer_name' <<<"$gc_refuse")" "site-b" \
    "and it names WHICH peer could not be established against — a refusal nobody can diagnose is an outage"

  # The operator at a terminal must not be the one left guessing either. The
  # --json shape is for scripts; a person reading the output gets the hash and
  # the reason on one line, and the sentence underneath it.
  gc_text=$("$BIN" --config "$cfg_a" gc --apply --grace 1ns 2>/dev/null)
  assert_eq "$(grep -cF "spared        $tp_blob  peer_unreachable" <<<"$gc_text" | tr -d ' ')" "1" \
    "gc's plain output names the blob it spared and why"

  # -------------------------------------------------------------------------
  note "  the peer that went away lost its disk, and is rebuilt from the one that trusts it (§51, §82, M7-04)"
  # -------------------------------------------------------------------------
  #
  # THE MILESTONE'S SENTENCE, end to end: a peer that loses its disk is restored
  # from a peer that trusted it, comes back with the SAME identity, and NO OTHER
  # peer is re-enrolled or reconfigured. Node B is already gone — the refusal
  # above needed it to be — and now its disk is gone too. The one input with no
  # fetch path, its Ed25519 identity, is what an operator keeps aside; everything
  # else is rebuilt from the backup node A has held since the push at the top of
  # this arc.
  #
  # Under ADR-0038 there is no re-clone: a node with an empty control plane
  # computes zero gaps and fetches nothing, so this restore is load-bearing, not a
  # convenience. Node A, the survivor, is never stopped or reconfigured through
  # any of it — which is the third clause, and it is asserted FROM A at the end.
  local b_recover_dry b_recover_done b_recover_ping rb_sock rb_wait
  cp "$root/b/data/peer_ed25519.key" "$root/b-identity.key"
  rm -rf "$root/b/data"

  # A DRY RUN first: fetch from A, verify the backup against B's OWN identity,
  # report what a restore would do, and touch nothing. A recovery trusts a
  # signature over its identity, not the peer that served the file.
  b_recover_dry=$("$BIN" --config "$cfg_b" recover \
    --from-endpoint "https://$addr_a" --from-key "$key_a" \
    --identity-key "$root/b-identity.key" --json 2>>"$log_b")
  assert_eq "$(jq -r '.applied' <<<"$b_recover_dry")" "false" \
    "recover defaults to a dry run that restores nothing"
  assert_eq "$(jq -r '.plan.source_peer_id' <<<"$b_recover_dry")" "$b_self_id" \
    "and it names the control plane it would restore — this node's own, verified against this node's key"
  assert_eq "$(jq -r '.plan.signed' <<<"$b_recover_dry")" "true" \
    "the backup verified against this node's identity, not against the peer that served it"
  assert_eq "$(jq -r '.plan.inputs[] | select(.name == "content-cas") | .status' <<<"$b_recover_dry")" "refetched-by-convergence" \
    "recover rebuilds the control plane and identity and leaves the content store to convergence — it is not a disk copy"

  # A --confirm that does not match the data directory exactly is refused, and it
  # is refused BEFORE any fetch — so the guard is reachable without a peer at all,
  # and a typo on an unrecoverable command does not run.
  assert_refuses "a --confirm that does not match the data directory exactly is refused" \
    "does not match the data directory" \
    "$BIN" --config "$cfg_b" recover \
    --from-endpoint "https://$addr_a" --from-key "$key_a" \
    --identity-key "$root/b-identity.key" --confirm "$root/b/data-typo"

  # The restore.
  b_recover_done=$("$BIN" --config "$cfg_b" recover \
    --from-endpoint "https://$addr_a" --from-key "$key_a" \
    --identity-key "$root/b-identity.key" --confirm "$root/b/data" --json 2>>"$log_b")
  assert_eq "$(jq -r '.applied' <<<"$b_recover_done")" "true" \
    "recover rebuilt the control plane from the surviving peer"

  # Restart the rebuilt node and let it prove the control plane came back — the
  # same catalogue, from the same peer id, none of which the content store could
  # have returned.
  start_peer_node "$cfg_b" "$log_b" "$mode"; pids_b=("${NODE_PIDS[@]}")
  rb_sock="$root/b/data/heyarr.sock"; rb_wait=0
  while (( rb_wait < 600 )); do
    curl -sf --unix-socket "$rb_sock" http://heyarr/readyz >/dev/null 2>&1 && break
    sleep 0.1; rb_wait=$(( rb_wait + 1 ))
  done
  (( rb_wait < 600 )) || { fail "the rebuilt node never became ready"; tail -20 "$log_b"; }

  assert_eq "$(api_b /api/v1/works | jq -r '[.items[].title] | sort | join(",")')" "$b_works_before" \
    "the control plane came back with the same catalogue it had before the disk was lost"

  # 🔴 THE THIRD CLAUSE, asserted FROM THE OTHER PEER. The rebuilt node dials A
  # over mTLS and A answers with the identity it derives from the certificate.
  # That A names it "site-b" means A recognised the rebuilt node as the same peer
  # without being told anything — no re-enrolment, no key change. This is the
  # load-bearing assertion: a restore that completes but leaves the fabric no
  # longer trusting the node is the failure worth catching, not the restore.
  b_recover_ping=$(cli_b peers ping site-a --json 2>>"$log_b")
  assert_eq "$(jq -r '.name' <<<"$b_recover_ping")" "site-b" \
    "the peer that trusts it recognised the rebuilt node as the same peer, with no re-enrolment"

  # And node A's own membership is untouched: it still pins the SAME key it first
  # enrolled, never having re-added the node that went away and came back.
  assert_eq "$(cli_a peers list --json | jq -r '.[] | select(.name == "site-b") | .public_key')" "$key_b" \
    "and node A still pins the key it first enrolled — nothing about the survivor was reconfigured"

  # -------------------------------------------------------------------------
  note "  a delete at one site converges to the other (§49, ADR-0073, #449)"
  # -------------------------------------------------------------------------
  #
  # Both nodes ingested the SAME library, so both hold "Cold Harbour" under the
  # same work_key — content addressing, not a copy of one catalogue into the
  # other (invariant 1). Cold Harbour is the fixture nothing wants or follows,
  # so deleting it disturbs no earlier assertion.
  #
  # Delete it on A and force the editorial op-log exchange: A offers the signed
  # delete op to B over the pinned mTLS link, and B honours it because it pinned
  # A's key (ADR-0012) — nobody tells B directly, the op carries its own
  # authority the whole way. Without it, B's next scan of the same bytes would
  # rebuild exactly what A removed: the delete-then-rebuild churn ADR-0071
  # refuses inside one node, now refused across the pair.
  #
  # The recovery arc above restarted node B on a FRESH port (listen: 127.0.0.1:0)
  # and only re-proved B -> A. This exchange dials A -> B, so refresh A's endpoint
  # for B first: registering the same key moves the endpoint and keeps the
  # identity (peers add is an upsert on the key). Without this the dial lands on
  # the dead old port and the sibling is merely deferred.
  local conv_id conv_del conv_sync conv_addr_b
  conv_addr_b=$(peer_listen_addr "$log_b") || { fail "convergence: node B is not listening on a peer surface"; return 1; }
  cli_a peers add --name site-b --public-key "$key_b" --endpoint "https://$conv_addr_b" --json >/dev/null
  conv_id=$(api_a /api/v1/works | jq -r '.items[] | select(.title == "Cold Harbour") | .id')
  [[ -n "$conv_id" ]] || { fail "convergence: node A has no 'Cold Harbour' work to delete"; return 1; }
  assert_eq "$(api_b /api/v1/works | jq -r '[.items[] | select(.title == "Cold Harbour")] | length')" "1" \
    "the work exists on node B before the delete — both nodes ingested the same library"

  conv_del=$(api_a "/api/v1/works/$conv_id" -X DELETE -o /dev/null -w '%{http_code}')
  assert_eq "$conv_del" "204" "the work deletes cleanly on node A (nothing follows it)"

  # The force-sync the beat runs on a cadence, run now so the demo need not wait
  # (§49). It is synchronous: when it returns, B has recorded and applied the op.
  conv_sync=$(api_a "/api/v1/catalog/sync" -X POST)
  assert_eq "$(jq -r '.synced' <<<"$conv_sync")" "1" \
    "the on-demand catalog sync converged with the one sibling (§49, ADR-0073)"

  assert_eq "$(api_b /api/v1/works | jq -r '[.items[] | select(.title == "Cold Harbour")] | length')" "0" \
    "the delete made on node A converged to node B: a work deleted at one site does not survive at the other (#449)"

  local p
  for p in "${PEER_PIDS[@]:-}"; do kill -TERM "$p" 2>/dev/null || true; done
  for p in "${PEER_PIDS[@]:-}"; do wait "$p" 2>/dev/null || true; done
  PEER_PIDS=()
}

