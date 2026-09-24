# shellcheck shell=bash
# A section of scripts/acceptance.sh: sourced by it, in order, never run alone.
# ---------------------------------------------------------------------------
# THE SWARM: two peers converge, and the external source serves less than two
# copies (§23, §24, §33, §85, M6-06)
# ---------------------------------------------------------------------------
#
# THE MILESTONE'S GATE. Everything above this point moves a blob from a peer
# that holds ALL of it to one that holds NONE of it. That is `Internet → A`,
# then `A → B`, and it is the shape Milestone 6 exists to replace:
#
#                        external source
#                     ↙                  ↘
#                  peer A ◄──────────► peer B
#
# Both peers pulling from the external source WHILE exchanging with each other,
# and both arriving as complete replicas. Until this runs, M6 is a set of
# packages with a Go test.
#
# # Why there is a third node, and why it is not enrolled as a holder
#
# The swarm path is reached only where single-source replication has no answer.
# `replicas` has no `partial` state — §23's opening sentence, still true in the
# schema — so a peer holding a third of a blob is recorded exactly like one that
# never heard of it, and `BlobSources` therefore returns nothing for a blob no
# peer holds WHOLE. That branch used to be a dead end and is now where the swarm
# runs (#279). When some peer does hold it whole, the streamed pull is better in
# every way and is deliberately left alone.
#
# So the third node stands in for the outside world: it holds the blob, it is an
# enrolled member, and it never reports its inventory to the other two. They know
# it exists and can ask it for pieces; their catalogues do not record it as
# holding anything, which is exactly the state a peer that acquired bytes ten
# seconds ago is in.
#
# # Why it is countable
#
# The cooperation claim is an INEQUALITY and cannot be stated without a number
# from the source's own side. Every node records what left it — "served a piece
# to a peer", with bytes and the peer that asked — so the external source's
# contribution is a fact about the external source rather than a claim by the
# peers about themselves.
#
# # What is NOT asserted, and why
#
# Not how much each peer contributed. That is a property of timing and it will
# move; M5 learned this twice in one afternoon and the second lesson was the
# expensive one. The claims below are the fixture-independent ones: both peers
# hold the whole blob, the external source served less than two copies of it,
# and bytes moved directly between the two peers.

# piece_served_bytes is how many PIECE bytes a node has sent for a blob.
#
# The sibling of peer_served_bytes, against the piece route rather than the
# content route, and it exists for the same reason that one does: what left a
# source is a fact about the source, where a destination's account of what it
# fetched is a claim about itself.
#
# The field is `blob`, not `blob_hash` — the piece route logs the digest under
# the name the transport calls it. Matching the wrong field would sum nothing
# and read as a source that served nothing, which is a passing assertion in
# three of the four places it is used below.
piece_served_bytes() { # logfile blob-hash
  { grep -F '"msg":"served a piece to a peer"' "$1" 2>/dev/null || true; } |
    jq -s --arg h "$2" '[.[] | select(.blob == $h) | .bytes] | add // 0'
}

# piece_served_count counts those reads, optionally to ONE peer.
#
# `grep -c .` rather than `wc -l`, because a here-string of "" is one empty line
# to wc and would report a surface that served nothing as having served once.
piece_served_count() { # logfile blob-hash [peer-id]
  { grep -F '"msg":"served a piece to a peer"' "$1" 2>/dev/null || true; } |
    jq -r --arg h "$2" --arg p "${3:-}" \
      'select(.blob == $h and ($p == "" or .peer_id == $p)) | .peer_id' | grep -c . || true
}

swarm_demo() {
  local root="$WORK/swarm" lib
  lib="$root/library"
  mkdir -p "$lib/movies/Drift Season (2022)"

  # THIRTY-TWO megabytes, and the size is load-bearing rather than generous.
  #
  # A piece is fixed-length and derived from the blob's size, clamped at 256 KiB
  # below (internal/storagefabric/pieces), so this is 128 pieces — enough that
  # the work divides between three sources many times over and one piece is
  # under a per cent of the transfer.
  #
  # The number that actually chose it is not the piece count, though. It is HOW
  # LONG ONE TRANSFER TAKES. This section needs node B to join while node A is
  # still fetching, and it observes that from the shell, which cannot see
  # anything faster than its 100ms poll. At eight megabytes — thirty-two pieces
  # over loopback — node A finished INSIDE THE FIRST POLL, so the overlap this
  # section is about did not exist and the run was `A, then B` wearing a
  # swarm'"'"'s clothes. It still passed every assertion, which is why the overlap
  # is now asserted rather than arranged.
  #
  # It is also deliberately ABOVE §16's 4 MiB chunking threshold, so the blob
  # has a manifest — which takes no part in the exchange (ADR-0041) and is here
  # so that its absence is not what makes the exchange work.
  head -c 33554432 /dev/urandom > "$lib/movies/Drift Season (2022)/Drift.Season.2022.1080p.mkv"

  local n
  for n in a b origin; do
    mkdir -p "$root/$n"
    cat > "$WORK/swarm-$n.yaml" <<YAML
data_dir: $root/$n/data
peer:
  name: swarm-$n
  site: swarm-$n
  listen: 127.0.0.1:0
  # THE EXTERNAL SOURCE IS A WEB SEED, and the other two are piece peers
  # (§27, ADR-0042, #266).
  #
  # `serve_pieces: false` leaves the content route untouched and refuses the
  # piece routes permanently, which is exactly §27's shape: a member reachable
  # over HTTP that serves byte ranges of blobs it holds whole and takes no part
  # in swarms. Until this existed nothing in the tree could produce such a node,
  # so `transfer.WebSeed` was constructed nowhere outside a Go test and the
  # transport's web-seed half was unreachable from any running binary.
  #
  # It costs the demo nothing — the same three nodes, the same fixture, the same
  # transfers — and it makes this section a truer picture of §23 than it was:
  # the thing standing in for the outside world is now a plain byte source
  # rather than a full swarm participant, which is what an external source is.
  serve_pieces: $( [ "$n" = "origin" ] && echo false || echo true )
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

  local cfg_a="$WORK/swarm-a.yaml" cfg_b="$WORK/swarm-b.yaml" cfg_o="$WORK/swarm-origin.yaml"
  local log_a="$root/a.log" log_b="$root/b.log" log_o="$root/origin.log"
  local sock_a="$root/a/data/heyarr.sock" sock_b="$root/b/data/heyarr.sock"
  local sock_o="$root/origin/data/heyarr.sock"
  local tok_a tok_b tok_o
  tok_a=$("$BIN" --config "$cfg_a" token create acceptance --scopes admin --json | jq -r .token)
  tok_b=$("$BIN" --config "$cfg_b" token create acceptance --scopes admin --json | jq -r .token)
  tok_o=$("$BIN" --config "$cfg_o" token create acceptance --scopes admin --json | jq -r .token)

  start_peer_node "$cfg_a" "$log_a" all
  start_peer_node "$cfg_b" "$log_b" all
  start_peer_node "$cfg_o" "$log_o" all

  sw_a() { curl -sS --unix-socket "$sock_a" -H "Authorization: Bearer $tok_a" "${@:2}" "http://heyarr$1"; }
  sw_b() { curl -sS --unix-socket "$sock_b" -H "Authorization: Bearer $tok_b" "${@:2}" "http://heyarr$1"; }
  sw_o() { curl -sS --unix-socket "$sock_o" -H "Authorization: Bearer $tok_o" "${@:2}" "http://heyarr$1"; }
  cli_sa() { "$BIN" --config "$cfg_a" --token "$tok_a" "$@"; }
  cli_sb() { "$BIN" --config "$cfg_b" --token "$tok_b" "$@"; }
  cli_so() { "$BIN" --config "$cfg_o" --token "$tok_o" "$@"; }

  local s waited
  for s in "$sock_a" "$sock_b" "$sock_o"; do
    waited=0
    while (( waited < 900 )); do
      curl -sf --unix-socket "$s" http://heyarr/readyz >/dev/null 2>&1 && break
      sleep 0.1; waited=$(( waited + 1 ))
    done
    if (( waited >= 900 )); then fail "swarm: $s never became ready"; tail -20 "$log_a"; return 1; fi
  done

  local l
  for l in "$log_a" "$log_b" "$log_o"; do
    waited=0
    while (( waited < 1200 )); do
      (( $(grep -c '"msg":"ingested"' "$l" 2>/dev/null || true) >= 1 )) && break
      sleep 0.1; waited=$(( waited + 1 ))
    done
    if (( waited >= 1200 )); then fail "swarm: $l never ingested the fixture"; tail -20 "$l"; return 1; fi
  done

  local addr_a addr_b addr_o
  addr_a=$(peer_listen_addr "$log_a") || { fail "swarm: node A never bound a peer surface"; return 1; }
  addr_b=$(peer_listen_addr "$log_b") || { fail "swarm: node B never bound a peer surface"; return 1; }
  addr_o=$(peer_listen_addr "$log_o") || { fail "swarm: the source never bound a peer surface"; return 1; }

  # -------------------------------------------------------------------------
  note "  three enrolled members, and one of them never says what it holds"
  # -------------------------------------------------------------------------
  local key_a key_b key_o
  key_a=$(cli_sa peers list --json | jq -r '.[] | select(.is_self) | .public_key')
  key_b=$(cli_sb peers list --json | jq -r '.[] | select(.is_self) | .public_key')
  key_o=$(cli_so peers list --json | jq -r '.[] | select(.is_self) | .public_key')

  # Every pair, in both directions, plus each node's record of itself so it can
  # report its own inventory through the same door everyone else uses (M4-07).
  cli_sa peers add --name swarm-a --public-key "$key_a" --endpoint "https://$addr_a" --json >/dev/null
  cli_sb peers add --name swarm-b --public-key "$key_b" --endpoint "https://$addr_b" --json >/dev/null
  cli_so peers add --name swarm-origin --public-key "$key_o" --endpoint "https://$addr_o" --json >/dev/null
  cli_sa peers add --name swarm-b --site swarm-b --mode full --public-key "$key_b" --endpoint "https://$addr_b" --json >/dev/null
  cli_sa peers add --name swarm-origin --site swarm-origin --mode full --public-key "$key_o" --endpoint "https://$addr_o" --json >/dev/null
  cli_sb peers add --name swarm-a --site swarm-a --mode full --public-key "$key_a" --endpoint "https://$addr_a" --json >/dev/null
  cli_sb peers add --name swarm-origin --site swarm-origin --mode full --public-key "$key_o" --endpoint "https://$addr_o" --json >/dev/null
  cli_so peers add --name swarm-a --site swarm-a --mode full --public-key "$key_a" --endpoint "https://$addr_a" --json >/dev/null
  cli_so peers add --name swarm-b --site swarm-b --mode full --public-key "$key_b" --endpoint "https://$addr_b" --json >/dev/null

  assert_eq "$(cli_sa peers list --json | jq -r 'length')" "3" \
    "node A has three members: itself, the other peer, and the external source"

  # WHAT EACH MEMBER SPEAKS, asked over mTLS rather than assumed (§27, #266).
  #
  # ADR-0038 makes each peer authoritative for its own site, so this is the
  # peer's statement about itself and not an operator's claim about a machine
  # they are not. `peers ping` reads the identity route, which is where that
  # statement lives.
  local sw_speaks_o sw_speaks_b
  sw_speaks_o=$(cli_sa peers ping swarm-origin --json 2>/dev/null | jq -r '(.speaks // []) | sort | join(",")')
  sw_speaks_b=$(cli_sa peers ping swarm-b --json 2>/dev/null | jq -r '(.speaks // []) | sort | join(",")')
  assert_eq "$sw_speaks_o" "blob-content" \
    "the external source says it speaks blob content and NOT piece exchange — it is a web seed (§27)"
  assert_eq "$sw_speaks_b" "blob-content,piece-exchange" \
    "and the other peer says it speaks both, so the two are distinguishable before either is asked for anything"

  local blob size
  blob=$(sw_o /api/v1/assets | jq -r '.items[0].blob_hash')
  assert_contains "$blob" "blake3:" "the fixture names bytes all three nodes hashed to the same digest"
  size=$(sw_o "/api/v1/blobs/$blob" | jq -r '.size')
  assert_eq "$size" "33554432" "and the catalogue agrees how big they are"

  # -------------------------------------------------------------------------
  note "  the gap: two peers want a blob that, as far as either can tell, nobody holds"
  # -------------------------------------------------------------------------
  #
  # The bytes are really deleted from two real disks, and each node then reports
  # what it now holds. The external source reports NOTHING to anybody, so its
  # `replicas` rows on A and B stay unproven — which is the state §23 is about,
  # and the state that sends this through the swarm rather than the streamed
  # pull.
  assert_eq "$(peer_holds "$root/a/data/cas" "$blob")" "1" "node A holds the bytes before the gap is made"
  assert_eq "$(peer_holds "$root/b/data/cas" "$blob")" "1" "and so does node B"
  find "$root/a/data/cas/blobs" -name "${blob#blake3:}" -type f -delete
  find "$root/b/data/cas/blobs" -name "${blob#blake3:}" -type f -delete
  assert_eq "$(peer_holds "$root/a/data/cas" "$blob")" "0" "node A does not hold them after"
  assert_eq "$(peer_holds "$root/b/data/cas" "$blob")" "0" "and neither does node B"
  assert_eq "$(peer_holds "$root/origin/data/cas" "$blob")" "1" \
    "the external source still holds every byte — it is the only complete copy in the fabric"

  cli_sa peers report-inventory swarm-a --json >/dev/null
  cli_sb peers report-inventory swarm-b --json >/dev/null

  # Make cooperation STRUCTURAL rather than raced (#274). The coin toss was that
  # node A held NOTHING at the instant it started, so a joining node B could
  # survey a peer with nothing to offer and pull its whole copy from the fast web
  # seed before A had landed a single piece. So node A is pre-staged with the
  # first HALF of the blob's REAL pieces — a guaranteed piece source the moment B
  # surveys it.
  #
  # Node B starts empty, as before. When both reconcile, PullPieces surveys every
  # member and fetches rarest-first PER SOURCE (internal/peer/transfer): the pieces
  # only the web seed holds ([H, N)) are rarity 1 and go to the web seed, while
  # A's half ([0, H)) is rarity 2 and A's own worker serves it — so B takes A's
  # half FROM A, deterministically, whatever the scheduler or machine speed. Only
  # A is staged, not B, so B still fetches from empty and its playback-window
  # priority (piece 20, below) is exercised exactly as before.
  #
  # The bytes are the blob's OWN, pulled from the external source (the one complete
  # copy) and staged under its real digest via `stagepartial --content-in`, so the
  # pieces A advertises are the very pieces the fabric is transferring — not a
  # synthetic stand-in.
  local swarm_bytes pieces_total half
  swarm_bytes=$WORK/swarm-blob.bin
  sw_o "/api/v1/blobs/$blob/content" -o "$swarm_bytes"
  # 256 KiB pieces at this geometry (5 MiB is piece 20, staged below), so the
  # 32 MiB blob is 128 pieces. Stage node A with the first half.
  pieces_total=$(( size / 262144 ))
  half=$(( pieces_total / 2 ))
  # `seq 0 N | paste -sd,` rather than `seq -s, 0 N`: BSD seq (macOS) appends a
  # TRAILING separator to -s, which stagepartial then reads as an empty piece
  # index. paste joins with no trailer on both GNU and BSD.
  "$STAGEPARTIAL" --cas "$root/a/data/cas" --content-in "$swarm_bytes" \
    --landed "$(seq 0 $(( half - 1 )) | paste -sd, -)" >/dev/null

  # The precondition the whole section rests on is asserted AFTER the fact, from
  # the log, rather than before it from a route that does not exist: the swarm
  # branch and the streamed pull write different lines, and which one ran is the
  # only form of this claim that cannot be satisfied by a system that took the
  # other path. See the two "took the PIECE path" assertions below.

  # -------------------------------------------------------------------------
  note "  🔴 both peers converge, from an external source and from each other (§23)"
  # -------------------------------------------------------------------------
  #
  # Both peers start together. Node A already holds half the blob (staged above),
  # node B holds nothing — so A is a piece source B can use the moment it surveys,
  # and B fetches A's half from A while the web seed serves the half only it has.
  # Cooperation is therefore structural, not a race: see the pre-stage comment for
  # why rarest-first steers B to take A's half from A, deterministically.
  #
  # # Why this replaced "node B joins a transfer in progress"
  #
  # That earlier shape started A first and had B join while A was mid-transfer, to
  # create overlap. But whether B then took any piece FROM A rather than wholly
  # from the fast external source was a coin toss: A held no pieces at the instant
  # it started, so B could survey a peer with nothing and pull its entire copy from
  # the web seed before A had anything to offer. Measured, that reversed run to run
  # and sometimes produced ZERO peer-to-peer bytes — which is #274. Pre-staging A
  # removes the timing: A always has something to serve before B looks.

  # Node A holds only its half before converging, not the whole blob — a half is a
  # partial, which never reaches the blob tree (invariant 1). Node B holds nothing
  # yet. So this is an incomplete peer and an empty one converging with the source,
  # which is what makes it a swarm and not a queue.
  assert_eq "$(peer_holds "$root/a/data/cas" "$blob")" "0" \
    "node A holds only its half before converging, not the whole blob"
  assert_eq "$(peer_holds "$root/b/data/cas" "$blob")" "0" \
    "and node B holds nothing yet: it will assemble its copy from A and the source"

  # Give node B a playback window (§33, §84 — time-critical priority). A player
  # reading a partial records where it is, and the transfer fetches the pieces
  # there FIRST rather than in survey order. 5 MiB is piece 20, which falls in
  # NODE A's half — so honouring the window also means B takes that piece from A,
  # the very cooperation being demonstrated.
  "$STAGEPARTIAL" --cas "$root/b/data/cas" --blob "$blob" --playhead 5242880

  # Both reconcile; there is no staggering and none is needed now that pre-staging
  # A makes cooperation structural rather than dependent on catching A mid-flight.
  sw_a "/api/v1/peers/swarm-a/reconcile" -X POST -o /dev/null
  sw_b "/api/v1/peers/swarm-b/reconcile" -X POST -o /dev/null

  swarm_converged() {
    [[ "$(peer_holds "$root/a/data/cas" "$blob")" == "1" &&
       "$(peer_holds "$root/b/data/cas" "$blob")" == "1" ]]
  }
  wait_for "the two peers never both ended up holding the blob — a swarm that does not converge is the milestone failing, not a slow runner" \
    1800 swarm_converged

  # BOTH PEERS COMPLETE, each having verified the whole object against its own
  # BLAKE3 digest. `peer_holds` finds the blob by digest under the store's own
  # fanout, and a partial never reaches the blob tree: Publish re-reads the
  # assembled file and hashes it whole before linking anything (invariant 1), so
  # the file being there under that name IS the verification having passed.
  assert_eq "$(peer_holds "$root/a/data/cas" "$blob")" "1" \
    "node A holds the whole blob, assembled from pieces and verified against its own digest"
  assert_eq "$(peer_holds "$root/b/data/cas" "$blob")" "1" \
    "node B holds it too — both peers converged, which is §23's claim"
  assert_eq "$(sw_a "/api/v1/blobs/$blob" | jq -r '.size')" "$size" \
    "and node A's catalogue accounts for every byte of it"

  # The path is named from the log rather than inferred: this is the line the
  # swarm branch writes and the streamed pull does not.
  local assembled_a assembled_b
  assembled_a=$(grep -cF '"msg":"a blob was assembled from peers that each held part of it"' "$log_a" || true)
  assembled_b=$(grep -cF '"msg":"a blob was assembled from peers that each held part of it"' "$log_b" || true)
  assert_eq "$(( assembled_a >= 1 ))" "1" \
    "node A took the PIECE path, not the streamed pull — the branch this milestone added is the one that ran"
  assert_eq "$(( assembled_b >= 1 ))" "1" "and so did node B"

  # 🔴 TIME-CRITICAL PRIORITY (§33, §84). Node B was given a playback window
  # before it joined, and its transfer adopted it: the driver read the client's
  # byte offset, converted it to the piece that contains it, and logged that it
  # prioritised the window. The offset was 5 MiB, which is piece 20 — so a log
  # naming piece 20 is the client's read position having steered the fetch order,
  # not the survey order. The reader publishes the offset (blobs), the store
  # carries it across the role boundary (invariant 4), and the driver honours it.
  local prioritised_b
  prioritised_b=$(grep -F '"msg":"prioritising pieces near the playback window"' "$log_b" | grep -c '"piece":20' || true)
  assert_eq "$(( prioritised_b >= 1 ))" "1" \
    "the transfer fetched the playback window first: node B's driver adopted the playhead and prioritised piece 20, the piece at the client's 5 MiB read position"

  # -------------------------------------------------------------------------
  note "  🔴 cooperation, as an inequality the split cannot move"
  # -------------------------------------------------------------------------
  #
  # Two peers each needed a whole copy. If they had not cooperated the external
  # source would have served two of them — that is the `Internet → A, then
  # A → B` shape measured in bytes. Stated as an inequality rather than as a
  # share, because how the work divided is a property of timing and will move
  # run to run; that it divided AT ALL is a property of the feature.
  local served_o served_a served_b two_copies
  served_o=$(( $(piece_served_bytes "$log_o" "$blob") + $(peer_served_bytes "$log_o" "$blob") ))
  served_a=$(piece_served_bytes "$log_a" "$blob")
  served_b=$(piece_served_bytes "$log_b" "$blob")
  two_copies=$(( size * 2 ))

  assert_eq "$(( served_o > 0 ))" "1" \
    "the external source served something: the instrument is live, and an inequality against a dead counter proves nothing"
  assert_eq "$(( served_o < two_copies ))" "1" \
    "THE COOPERATION CLAIM: the external source served ${served_o} bytes, fewer than the ${two_copies} two full copies would have cost"
  assert_eq "$(( served_a + served_b > 0 ))" "1" \
    "and the difference came from the peers themselves: ${served_a} bytes left node A and ${served_b} left node B, peer to peer"

  # 🔴 THE WEB SEED WAS A REAL SOURCE, and it served no piece to anybody.
  #
  # Both halves, because either alone is satisfiable by the wrong thing. That
  # it served bytes says the web-seed contract carried a real share of the
  # transfer; that its PIECE record is empty says those bytes went over the
  # ordinary content route, which is what makes it a web seed rather than a
  # peer that happened to be asked politely.
  assert_eq "$(( $(peer_served_bytes "$log_o" "$blob") > 0 ))" "1" \
    "the web seed served blob content: §27's ordinary endpoint carried part of a swarm transfer"
  assert_eq "$(piece_served_count "$log_o" "$blob")" "0" \
    "AND IT SERVED NO PIECES AT ALL — a member that does not speak piece exchange was driven as a web seed, which is the half of the transport nothing could reach before (#266)"

  note "  the external source carried $(pct "$served_o" "$two_copies")% of what two independent pulls would have cost"

  # -------------------------------------------------------------------------
  note "  🔴 a client never learns what a piece is (§33, §85)"
  # -------------------------------------------------------------------------
  #
  # M6 is an INTERNAL optimisation, and this is the assertion that keeps it one.
  # It is about an absence, which is why it is easy to forget: a blob that
  # arrived as thirty-two pieces from three machines is served to an ordinary
  # HTTP client exactly as any other blob is — one range request, one bearer
  # token, no piece anywhere in the exchange.
  # Two counters are read around the client reads, and they must move in
  # opposite ways. The client API's own counter must MOVE — otherwise the
  # absence asserted next is an absence of instrumentation — and the piece
  # record must NOT, because a piece is something only the peer surface has ever
  # heard of.
  #
  # The negative alone is the weakest assertion in this file: it passes against
  # a metric that was never registered and a log line that was renamed. The
  # positive beside it is what makes it evidence.
  # The control is read from the EXTERNAL SOURCE's record, not from node A's.
  #
  # It was node A's first, and that was wrong for a reason worth keeping: which
  # of the two peers ends up serving the other is a race, and it reversed
  # between two runs on one machine. A control that is only true when node A
  # happened to be the one ahead is a control that fails on the run where it was
  # needed. The external source serves pieces in every run by construction, and
  # it is the same log line read by the same helper, so it establishes exactly
  # what the control has to establish: this instrument records piece reads.
  local pieces_before pieces_after ctrl_before range_code range_len
  # Read from the two PEERS rather than from the external source, which is a
  # web seed and serves no pieces by design (#266). Summed across both, because
  # which of them ends up serving the other is a race that reversed between two
  # runs on one machine — a control that is only true when node A happened to
  # be ahead is a control that fails on the run where it was needed. The
  # cooperation assertion above has already established that the sum is
  # non-zero, so this reads the same fact through the instrument the absence
  # below is measured with.
  assert_eq "$(( $(piece_served_count "$log_a" "$blob") + $(piece_served_count "$log_b" "$blob") >= 1 ))" "1" \
    "pieces were served and recorded between the two peers: the record the absence below is measured against is a live instrument"
  pieces_before=$(piece_served_count "$log_a" "$blob")
  ctrl_before=$(ctrl_blob_bytes "$(sw_a /metrics)")

  range_code=$(sw_a "/api/v1/blobs/$blob/content" -H 'Range: bytes=0-1023' -o "$root/range.bin" -w '%{http_code}')
  assert_eq "$range_code" "206" \
    "a cooperatively-assembled blob range-reads over ordinary HTTP, on a bearer token (§28, §33)"
  range_len=$(wc -c < "$root/range.bin" | tr -d ' ')
  assert_eq "$range_len" "1024" "and the bytes are the bytes: a 1 KiB range is 1 KiB"
  assert_eq "$(sw_a "/api/v1/blobs/$blob/content" -o /dev/null -w '%{http_code}')" "200" \
    "and the whole blob streams to a client that asked for no range at all"

  # >= 2, not == 2, and the difference is the whole of #274. ctrl_blob_bytes sums
  # heyarr_http_requests_total for the blob-content ROUTE across the whole node,
  # not for this blob — so a background probe self-read (a worker with ffprobe
  # fetches a blob's bytes over this exact route to hand them to ffprobe, see
  # peer_blob_reads and the drift note above ctrl_blob_bytes) can land in the
  # window between ctrl_before and here and make the delta 3. On a runner that
  # has ffprobe that is ordinary traffic, not a fault, and it is what flaked the
  # ubuntu acceptance run — a red on a docs-only branch, six consequences of one
  # extra read. The control only has to prove the counter MOVED by at least the
  # two reads this test just did, so the "pieces did not move" assertion below is
  # measured against a live instrument rather than a dead one; == 2 additionally
  # asserted a quiet node, which this node is not and need not be. The same >=
  # idiom this file uses for every other live-instrument control (the piece-served
  # control four lines up is >= 1 for exactly this reason).
  assert_eq "$(( $(ctrl_blob_bytes "$(sw_a /metrics)") - ctrl_before >= 2 ))" "1" \
    "the client API counted at least both of those reads: the instrument the absence below is measured against is live"
  pieces_after=$(piece_served_count "$log_a" "$blob")
  assert_eq "$pieces_after" "$pieces_before" \
    "AND THE PIECE RECORD DID NOT MOVE: a client asking for bytes over HTTP causes no piece anywhere, which is what keeps M6 an internal optimisation rather than a client requirement (§33, §85)"

  # -------------------------------------------------------------------------
  note "  🔴 progressive playback: a client range-reads a blob that has not finished arriving (§33, §84, ADR-0044)"
  # -------------------------------------------------------------------------
  #
  # Everything above served a blob held WHOLE. M10's premise is the opposite: a
  # player consuming ordinary HTTP over content that is still arriving. M6 built
  # the byte machinery — cas.ReadPartialAt serves out of a still-assembling blob —
  # and wired it to the peer surface alone, so nothing a PLAYER talks to could
  # reach it. This is the client route's partial path (ADR-0044): consult the
  # availability record, serve a landed range, and BLOCK on a range that has not
  # landed rather than serving the hole (ADR-0042/0043).
  #
  # Staged directly into node A's CAS rather than raced out of the swarm above: a
  # blob with a middle-piece HOLE is genuinely incomplete — Publish would fail on
  # it — deterministic, and the landed range is known. Only the client mount grows
  # this; the peer content route never does (ADR-0042).
  local pp_full="$root/pp-full.bin" pp_blob pp_got="$root/pp-got.bin"
  pp_blob=$("$STAGEPARTIAL" --cas "$root/a/data/cas" --size 786432 --landed 0,2 --content-out "$pp_full")
  assert_contains "$pp_blob" "blake3:" "the staged partial has a digest node A can be asked for"
  assert_eq "$(peer_holds "$root/a/data/cas" "$pp_blob")" "0" \
    "the staged blob is genuinely incomplete: nowhere in the blob tree, only a partial in staging with a hole where piece 1 should be"

  # A range wholly inside piece 0 (0..262143), which has landed: it must serve at
  # once, over the client route, from the still-assembling partial.
  local pp_code
  pp_code=$(sw_a "/api/v1/blobs/$pp_blob/content" -H 'Range: bytes=1000-9191' -o "$pp_got" -w '%{http_code}')
  assert_eq "$pp_code" "206" \
    "a client range-read a blob that had not finished arriving — 206 from the still-assembling partial, over ordinary HTTP"
  dd if="$pp_full" of="$root/pp-expect.bin" bs=1 skip=1000 count=8192 2>/dev/null
  assert_eq "$("$GEN" -hash "$pp_got")" "$("$GEN" -hash "$root/pp-expect.bin")" \
    "and the served bytes are the true content of that range, not a zero-filled hole — the bitset gate held (ADR-0043)"

  # A range inside the HOLE (piece 1, 262144..524287) must BLOCK: the reader waits
  # for bytes that never come rather than 500ing or serving zeroes. A short client
  # timeout proves it — curl exits 28, no body delivered.
  local pp_hole_rc=0
  sw_a "/api/v1/blobs/$pp_blob/content" -H 'Range: bytes=300000-300999' --max-time 2 -o /dev/null >/dev/null 2>&1 || pp_hole_rc=$?
  assert_eq "$pp_hole_rc" "28" \
    "a range that has NOT landed blocks rather than serving a hole or failing — the client times out waiting, it is never handed garbage (ADR-0044)"

  # Land the missing piece — what a worker does as it arrives — and the same range
  # now serves: block-then-serve resolves, and the client never knew it waited.
  "$STAGEPARTIAL" --cas "$root/a/data/cas" --size 786432 --landed 0,1,2 >/dev/null
  local pp_code2
  pp_code2=$(sw_a "/api/v1/blobs/$pp_blob/content" -H 'Range: bytes=300000-300999' -o "$root/pp-hole.bin" -w '%{http_code}')
  assert_eq "$pp_code2" "206" \
    "once the missing piece lands, the same range serves — the transparent transition a player sees as an ordinary read"
  dd if="$pp_full" of="$root/pp-hole-expect.bin" bs=1 skip=300000 count=1000 2>/dev/null
  assert_eq "$("$GEN" -hash "$root/pp-hole.bin")" "$("$GEN" -hash "$root/pp-hole-expect.bin")" \
    "and those bytes are the true content too, once the piece that carried them landed"

  # -------------------------------------------------------------------------
  note "  🔴 ensure-on-GET: a client reaches for a DESIRED blob nobody is transferring, and the GET starts the fetch (§33, #371)"
  # -------------------------------------------------------------------------
  #
  # Everything above served a blob already held, arriving, or staged. #371 is the
  # case none of those cover: a player presses play on content this node has
  # DECIDED it wants — a live asset names it — but does not hold, with no transfer
  # running. Before this the client route 404'd; the fetch had to be started by
  # some other trigger first. Now the GET ensures one, through the job table
  # (invariant 4), gated on the blob being desired so a GET can never make the
  # node fetch arbitrary content (the DoS #371 names).
  #
  # $blob is converged onto A and B. Delete it from A's disk ALONE — the asset row
  # stays, so A still DESIRES it — and do not reconcile. A now desires a blob it
  # does not hold with nothing in flight: exactly the state the route must turn
  # into a transfer.
  local selfA
  selfA=$(cli_sa peers list --json | jq -r '.[] | select(.is_self) | .id')
  find "$root/a/data/cas/blobs" -name "${blob#blake3:}" -type f -delete
  assert_eq "$(peer_holds "$root/a/data/cas" "$blob")" "0" \
    "node A no longer holds the desired blob, and nothing is fetching it"

  # ALL-STATE count of A-destined replicate_blob jobs for a blob. All-state, not
  # just live: a 32 MiB local reassembly can COMPLETE inside a request, so a "live
  # jobs" count races the transfer finishing (the first CI run failed exactly
  # there — a job already succeeded read as zero). Counting every state and
  # asserting on the DELTA is immune: a succeeded job still counts, so two GETs
  # that collapse to one job read as one whether it is still running or already
  # done. limit=200 keeps the whole set on one page.
  count_repl() { # blob-hash -> number of A-destined replicate_blob jobs, any state
    sw_a "/api/v1/jobs?type=replicate_blob&limit=200" | jq --arg b "$1" --arg p "$selfA" \
      '[.items[] | select(.payload.blob_hash == $b and .payload.destination_peer_id == $p)] | length'
  }
  ensure_blob_held_on_a() { [[ "$(peer_holds "$root/a/data/cas" "$1")" == "1" ]]; }

  # The baseline: the convergence transfer that put $blob on A before we deleted
  # it. Whatever it is, the GET below must add exactly one to it.
  local repl_before repl_after1 repl_after2
  repl_before=$(count_repl "$blob")

  # The GET. It block-then-serves off the transfer it starts, which for a 32 MiB
  # reassembly can outlast one request — a bounded "still fetching" a client
  # retries, never a hang (#371). So the request is capped and its code ignored:
  # what it PROVES is server-side — that asking ensured a transfer through the job
  # table.
  sw_a "/api/v1/blobs/$blob/content" --max-time 3 -o /dev/null >/dev/null 2>&1 || true
  repl_after1=$(count_repl "$blob")
  assert_eq "$repl_after1" "$(( repl_before + 1 ))" \
    "the GET ensured a transfer through the job table: exactly one new replicate_blob job targets A for the blob it desires"

  # A SECOND GET must not stack a second transfer. The enqueue is keyed on
  # blob + destination — the key reconcile_peer uses — so it collapses onto the
  # one already accounted for, whether that job is still running or has completed
  # and the blob is now whole (invariant 9).
  sw_a "/api/v1/blobs/$blob/content" --max-time 3 -o /dev/null >/dev/null 2>&1 || true
  repl_after2=$(count_repl "$blob")
  assert_eq "$repl_after2" "$repl_after1" \
    "a second GET created no second transfer: the idempotent enqueue collapsed both onto one job (two GETs → one job)"

  # And it ENDS UP SERVED: the ensured transfer completes in the worker and the
  # same route returns the true bytes.
  wait_for "node A never re-assembled the blob the GET asked for — the ensured transfer did not complete" \
    1800 ensure_blob_held_on_a "$blob"
  local ensure_got="$root/ensure-got.bin" ensure_code
  ensure_code=$(sw_a "/api/v1/blobs/$blob/content" -o "$ensure_got" -w '%{http_code}')
  assert_eq "$ensure_code" "200" \
    "once the ensured transfer landed, the same GET serves the blob — press-play-on-not-yet-here content, end to end"
  assert_eq "$("$GEN" -hash "$ensure_got")" "$blob" \
    "and the served bytes are exactly the blob's content, not a partial or a hole"

  # THE GATE, the load-bearing negative: a GET for a blob NOBODY desires still
  # 404s and starts nothing. This is what keeps ensure-on-GET from being
  # fetch-on-request.
  printf 'ensure-on-get-nobody-desires-these-bytes-%s' "$RANDOM$RANDOM$$" > "$root/nd.bin"
  local nd nd_code
  nd=$("$GEN" -hash "$root/nd.bin")
  nd_code=$(sw_a "/api/v1/blobs/$nd/content" --max-time 3 -o /dev/null -w '%{http_code}')
  assert_eq "$nd_code" "404" \
    "a GET for a blob no asset references is a plain 404 — the gate refused to fetch it"
  assert_eq "$(count_repl "$nd")" "0" \
    "and it started no transfer: a hash nobody desires cannot be pulled by asking for it (the DoS gate holds)"

  local p
  for p in "${PEER_PIDS[@]:-}"; do kill -TERM "$p" 2>/dev/null || true; done
  for p in "${PEER_PIDS[@]:-}"; do wait "$p" 2>/dev/null || true; done
  PEER_PIDS=()
}

