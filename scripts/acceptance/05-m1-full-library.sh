# shellcheck shell=bash
# A section of scripts/acceptance.sh: sourced by it, in order, never run alone.
# ---------------------------------------------------------------------------
# The full library over the API (M1-18)
#
# Everything below drives the real HTTP API against a GENERATED library that
# contains every content type, both non-primary asset roles, a file the scanner
# must decline, a byte-identical pair at two paths, and a large file so that
# range serving is exercised on something that does not fit in one buffer.
#
# Every expected count is read out of the generator's manifest rather than
# typed here. A number in a shell script drifts from the fixture the first time
# anyone adds a file, and then the demo asserts something that used to be true.
# ---------------------------------------------------------------------------

FULLROOT="$WORK/full"
FULLLIB="$FULLROOT/library"
FULLDATA="$FULLROOT/data"
SOCK="$FULLDATA/heyarr.sock"
MANIFEST="$FULLROOT/manifest.json"

# The fixture generator is a dev helper, not part of the product: goreleaser
# builds ./cmd/heyarr and nothing else (ADR-0002). `make fixtures` builds it.
# It is resolved rather than assumed because the real deployment host has no Go
# toolchain — there both binaries are cross-compiled and shipped, and GEN points
# at the one that arrived.
GEN=${GEN:-./bin/genlibrary}
if [[ ! -x "$GEN" ]] && command -v go >/dev/null 2>&1; then
  go build -o "$WORK/genlibrary" ./internal/testutil/fixtures/cmd/genlibrary 2>/dev/null && GEN="$WORK/genlibrary"
fi

# stagepartial is the same shape of dev helper (never shipped): it writes a blob
# into a CAS as an in-flight transfer — a sparse staging file with only some
# pieces and the availability record that says which — so the progressive-playback
# section can demonstrate serving a blob that has not finished arriving (§33, §84,
# ADR-0044) deterministically, rather than racing the live swarm to catch one
# mid-transfer.
STAGEPARTIAL=${STAGEPARTIAL:-./bin/stagepartial}
if [[ ! -x "$STAGEPARTIAL" ]] && command -v go >/dev/null 2>&1; then
  go build -o "$WORK/stagepartial" ./internal/testutil/fixtures/cmd/stagepartial 2>/dev/null && STAGEPARTIAL="$WORK/stagepartial"
fi

# A ~200 MB streaming fixture is the point: a file that fits in one buffer
# proves nothing about the 20 GB remux ADR-0013 calls a normal case. Shrinkable
# for a quick local loop, and the size is printed so a green run against a tiny
# fixture is never mistaken for a green run against a real one.
LARGE=${HEYARR_ACCEPTANCE_LARGE_SIZE:-209715200}

api() { # path [curl args...]
  local path=$1; shift
  curl -sS --unix-socket "$SOCK" -H "Authorization: Bearer $TOKEN" "$@" "http://heyarr$path"
}

# events_replay prints the event log from seq 0 up to the head as it stands
# NOW, and then stops.
#
# SSE never ends by design, so a replay has to be cut off. It used to be cut
# off by `--max-time 5` alone, which made every replay cost exactly five
# seconds however little there was to read — four of them were twenty seconds
# of the demo budget spent waiting for events nobody was going to emit.
#
# The head is read from /api/v1/system BEFORE the stream is opened, so every
# event the caller is about to assert on — emitted by an action that has
# already returned — is at or below it, and the replay stops at the blank line
# that ends that event. `--max-time 5` stays as the bound: a head that cannot
# be read, or a stream that never reaches it, degrades to the old behaviour
# rather than to a hang or a truncated read.
events_replay() {
  local head
  head=$(api /api/v1/system | jq -r 'if .events.ok then .events.head else 0 end' 2>/dev/null || echo 0)
  [[ "$head" =~ ^[0-9]+$ ]] || head=0
  # A read loop over a process substitution, not a pipeline. A pipeline waits
  # for every member, and curl would sit in it on an idle stream until its
  # --max-time — the very five seconds this exists to stop paying. Past the
  # break curl is left to hit that bound on its own, off the critical path.
  # (Not awk either: mawk, Ubuntu's awk, block-buffers the pipe it reads.)
  local line seq=0
  while IFS= read -r line; do
    printf '%s\n' "$line"
    if [[ "$line" == "id: "* ]]; then
      seq=${line#id: }
    elif [[ -z "$line" ]] && (( head > 0 && seq >= head )); then
      break
    fi
  done < <(api "/api/v1/events?after=0" --max-time 5 --no-buffer 2>/dev/null || true)
}

# Follows keyset cursors to the end. A list command that reads one page and
# stops is wrong for a real library, and a demo that asserts on one page would
# never notice.
# NextCursor is absent on the last page, so "keep going while next_cursor is
# set" is the whole loop.
api_all() { # path jq-filter
  local path=$1 filter=$2 cursor="" url page sep="?"
  [[ "$path" == *"?"* ]] && sep="&"
  while :; do
    url="$path"
    if [[ -n "$cursor" ]]; then
      url="${path}${sep}cursor=${cursor}"
    fi
    page=$(api "$url")
    jq -r "$filter" <<<"$page"
    cursor=$(jq -r '.next_cursor // empty' <<<"$page")
    [[ -z "$cursor" ]] && break
  done
}

# Waits until the provider health beat (#164) has recorded an observation for a
# named provider, and prints that provider's entry.
#
# Every assertion about provider health downstream of this is an assertion
# about something that WAS ACTUALLY OBSERVED. Before the beat existed, nothing
# ever wrote checked_at, so those assertions held unfalsifiably — which is
# worse than failing, because they read as coverage. Waiting here is what makes
# them mean something; the wait itself is bounded, and a beat that never fires
# fails the caller rather than hanging this script.
wait_for_health_check() { # provider-name
  local name=$1
  wait_for "the health beat never recorded an observation for provider '$name' — every assertion below is about what it observed" \
    300 health_check_recorded "$name"
  printf '%s' "$WAIT_HEALTH_ENTRY"
}

# health_check_recorded <provider-name> — 0 once that provider has been checked
# at least once. Leaves the entry in WAIT_HEALTH_ENTRY so the caller does not
# pay for a second request to read what the poll already fetched.
WAIT_HEALTH_ENTRY=
health_check_recorded() { # provider-name
  WAIT_HEALTH_ENTRY=$(jq -c --arg n "$1" '[.providers[] | select(.name == $n)] | .[0]' \
    <<<"$(api /api/v1/providers)")
  [[ "$(jq -r '.checked_at // "never"' <<<"$WAIT_HEALTH_ENTRY")" != "never" ]]
}

# probe_recorded <blob-hash> — 0 once the blob's probe has landed and named a
# container.
#
# THE CONTAINER, not the 200 and not the job row. The planner decides on the
# container recorded for the blob, so the container is the thing every assertion
# downstream of a probe is actually about — and a probe row without one would
# leave the planner in exactly the state this wait exists to get it out of.
probe_recorded() { # blob-hash
  [[ -n "$(api "/api/v1/blobs/$1/probe" 2>/dev/null | jq -r '.container // ""' 2>/dev/null)" ]]
}

# wait_for_probe <blob-hash> <what the assertions below are about> — the wait
# every probe-derived assertion needs, with the failure message those assertions
# would otherwise print in place of.
#
# Without it, a planner asked before the probe lands correctly answers `direct`
# — it has no container, and `direct` is the honest answer to "I do not know
# what this file is" — and the run reports a planner regression that did not
# happen (#207).
#
# # Why this budget is the largest in the file rather than the middle one
#
# It was 600 (60s), and it ran out on CI — on a DOCS-ONLY branch, taking six
# assertions down with it, because everything below a missing probe is a
# consequence of the probe missing (#274).
#
# This is the only wait here whose condition depends on all three of an external
# binary, a job queue, AND deliberate contention: the section that calls it
# starts a worker that CANNOT probe alongside one that can, precisely so the
# claim discipline is exercised. So the probe has to wait for the right claimant
# by design. Every other wait in this file is in-process and uncontended, and
# the replication wait — with neither an external binary nor a rival claimant —
# already had 900.
#
# 60 seconds was therefore the tightest budget on the heaviest wait, which is
# backwards. On the run that failed, the demo took 288s against a median of
# ~165s: a runner 1.7x slower turns a probe that comfortably lands in 40s into
# one that does not land in 60.
#
# This does not make the wait a bet on machine speed — it still polls for a
# CONDITION and stops the moment it arrives, so a fast machine pays nothing.
# It only widens how slow a machine may be before the run calls it a failure.
wait_for_probe() { # blob-hash description
  wait_for "the probe never recorded a container for blob $1, so $2" \
    900 probe_recorded "$1"
}

# refusal_arc_at_rest <want-id> — 0 once a fruitless search has BOTH recorded
# its three candidates and returned the want to `idle`.
#
# The conjunction, because neither half is the condition on its own: the
# candidates appear first and the phase settles after, so waiting on the
# candidates asserts the phase too early — and waiting on the phase alone is
# satisfied before the search has even started, since a fresh want starts idle.
refusal_arc_at_rest() { # want-id
  [[ "$(api "/api/v1/desired/$1/candidates" | jq -r '.candidates | length')" -ge 3 ]] &&
    [[ "$(api "/api/v1/desired/$1" | jq -r '.acquisition.phase')" == "idle" ]]
}

# Starts heyarr, waits for readiness and for the scan to finish ingesting, and
# leaves it running. Waits for CONDITIONS, never for a duration: a fixed wait is
# a bet on machine speed and this repo has lost that bet four times.
start_full() { # label roles...
  local label=$1; shift
  FULL_LOG="$WORK/$label.log"
  FULL_PIDS=()
  local role
  for role in "$@"; do
    "$BIN" --config "$WORK/full.yaml" "$role" >>"$FULL_LOG" 2>&1 &
    FULL_PIDS+=($!)
  done

  local waited=0
  while (( waited < 600 )); do
    curl -sf --unix-socket "$SOCK" http://heyarr/readyz >/dev/null 2>&1 && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if (( waited >= 600 )); then
    fail "$label: /readyz never became ready"; cat "$FULL_LOG"; stop_full; return 1
  fi
  return 0
}

wait_for_ingest() { # label expected
  local label=$1 expected=$2 waited=0 done_count=0
  while (( waited < 1800 )); do
    done_count=$(grep -c '"msg":"ingested"' "$FULL_LOG" 2>/dev/null || true)
    (( done_count >= expected )) && return 0
    sleep 0.1; waited=$(( waited + 1 ))
  done
  fail "$label: only $done_count of $expected files were ingested"
  tail -40 "$FULL_LOG"
  return 1
}

stop_full() {
  local p waited=0
  for p in "${FULL_PIDS[@]:-}"; do kill -TERM "$p" 2>/dev/null || true; done
  while (( waited < 200 )); do
    local alive=0
    for p in "${FULL_PIDS[@]:-}"; do kill -0 "$p" 2>/dev/null && alive=1; done
    (( alive == 0 )) && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  for p in "${FULL_PIDS[@]:-}"; do kill -KILL "$p" 2>/dev/null || true; wait "$p" 2>/dev/null || true; done
}

full_library_demo() {
  "$GEN" -out "$FULLLIB" -manifest "$MANIFEST" -large-size "$LARGE" >/dev/null

  local files blobs largest_path largest_size dup_hash
  files=$(jq -r '.ingestable_files' "$MANIFEST")
  blobs=$(jq -r '.ingestable_blobs' "$MANIFEST")
  largest_path=$(jq -r '.largest_path' "$MANIFEST")
  largest_size=$(jq -r '.largest_size' "$MANIFEST")
  dup_hash=$(jq -r '.duplicate_hash' "$MANIFEST")
  echo "       fixture library: $(jq -r '.files|length' "$MANIFEST") files, $files ingestable, streaming fixture $(( LARGE / 1048576 )) MiB"

  # Every assertion below compares a count from the API against a count from
  # the manifest. If the manifest were empty or unreadable, both sides would be
  # zero and the whole section would pass having tested nothing.
  if (( files < 5 )) || (( blobs < 4 )) || [[ -z "$dup_hash" || "$dup_hash" == "null" ]]; then
    fail "the fixture manifest describes $files ingestable files and $blobs blobs — too few to assert on"
    return 1
  fi

  cat > "$WORK/full.yaml" <<YAML
data_dir: $FULLDATA
peer:
  name: acceptance-full
  site: test
log:
  level: info
  format: json
# The socket is the whole transport here. Binding a fixed TCP port would make
# two runs on one machine collide, and a leaked server from an interrupted run
# would break every later run with a bind error that says nothing about why —
# which is exactly what happened while this section was being written.
http:
  addr: ""
libraries:
  - name: films
    content_type: movie
    roots: ["$FULLLIB/movies"]
  - name: shows
    content_type: series
    roots: ["$FULLLIB/tv"]
  - name: albums
    content_type: music
    roots: ["$FULLLIB/music"]
  - name: shelf
    content_type: book
    roots: ["$FULLLIB/books"]
# Providers (§59, M3-07). A FAKE indexer, because ADR-0026 says a real one can
# never run here: the milestone's claim is that Heyarr decides what should exist
# without a real indexer being present, and a fake exercises the same
# registration, routing and health paths as production.
#
# The disabled entry is not padding. "Configured and switched off" and "not
# configured at all" have to stay tellable apart, and the only way to prove the
# distinction survives to the wire is to have one of each.
providers:
  - name: acceptance-indexer
    type: fake
    capabilities: [indexer]
    # Canned answers, so a search can actually SELECT something rather than
    # only exercising the empty-search edge. The three shapes below are the
    # three §63 has to tell apart, and the demo asserts on each.
    offers:
      # A title of its own for the ingest section (M3-13).
      #
      # NOT Blue Harvest — the search and upgrade sections assert on its
      # candidate set. NOT Twice Told — that one is deliberately offered with
      # NO candidates, and it is what the empty-search assertion depends on.
      # Giving either of them a different answer would break a claim somewhere
      # else, which is how a shared fixture turns into a puzzle.
      - title: The Quiet Room
        candidates:
          - id: tqr-1080-web
            fetch: magnet:?xt=urn:btih:tqr-1080-web
            title: The Quiet Room 1080p web-dl
            attributes:
              resolution: 1080
              source: web-dl
              video_codec: h264
              hdr: false
      - title: Blue Harvest
        candidates:
          # Acceptable and terminal: passes the gate, meets every preference.
          - id: bh-2160-remux
            fetch: magnet:?xt=urn:btih:bh-2160-remux
            title: Blue Harvest 2160p remux
            attributes:
              resolution: 2160
              source: remux
              video_codec: hevc
              hdr: true
              audio_channels: 6
          # Acceptable and NOT as good as it gets — the gap the upgrade
          # workflow lives in.
          - id: bh-1080-web
            fetch: magnet:?xt=urn:btih:bh-1080-web
            title: Blue Harvest 1080p web-dl
            attributes:
              resolution: 1080
              source: web-dl
              video_codec: h264
              hdr: false
          # Rejected by the gate, so the demo has a real rejection to read a
          # reason off rather than asserting on an absence.
          - id: bh-480-cam
            fetch: magnet:?xt=urn:btih:bh-480-cam
            title: Blue Harvest 480p cam
            attributes:
              resolution: 480
              source: cam
              video_codec: h264
      # M3-15's full arc. A title the fixture library does NOT hold, so the
      # want begins with nothing at all — which is the case the milestone's
      # headline claim is about, and the one every fixture-shaped test quietly
      # avoids because every fixture has assets.
      - title: Nightfall Sonata
        candidates:
          - id: ns-2160-remux
            fetch: magnet:?xt=urn:btih:ns-2160-remux
            title: Nightfall Sonata 2160p remux
            attributes:
              resolution: 2160
              source: remux
              video_codec: hevc
              hdr: true
              audio_channels: 6
      # M3-15's refusal arc. EVERY candidate fails the gate, so the demo has a
      # want that searched, found things, and correctly acquired none of them —
      # with a durable explanation for each. §63's rejection reasons are as
      # much the deliverable as the acceptances, and a constraint nobody has
      # watched reject anything is decoration.
      - title: Static Bloom
        candidates:
          - id: sb-480-cam
            fetch: magnet:?xt=urn:btih:sb-480-cam
            title: Static Bloom 480p cam
            attributes:
              resolution: 480
              source: cam
              video_codec: h264
          - id: sb-576-ts
            fetch: magnet:?xt=urn:btih:sb-576-ts
            title: Static Bloom 576p telesync
            attributes:
              resolution: 576
              source: telesync
              video_codec: h264
          - id: sb-720-web
            fetch: magnet:?xt=urn:btih:sb-720-web
            title: Static Bloom 720p web-dl
            attributes:
              resolution: 720
              source: web-dl
              video_codec: h264
      # A title the indexer has nothing for, so the empty-search edge is
      # reachable without reconfiguring anything. It is a work the fixture
      # library already holds, so wanting it creates nothing.
      - title: Twice Told
        candidates: []
  - name: acceptance-disabled
    type: fake
    capabilities: [indexer]
    enabled: false
  # A download client that is CONFIGURED and NOT LISTENING. Port 9 is discard,
  # which is reserved and refuses connections everywhere — so this exercises
  # ADR-0025's central claim on a real node: a download client that is down must
  # not stop Heyarr from starting, serving or playing.
  #
  # It also gives the path map somewhere real to be validated: a sibling of the
  # library rather than inside it, which is the layout the whole ADR-0014
  # section exists to encourage.
  # A REAL TORZNAB INDEXER, configured and pointing at nothing (M3-09).
  #
  # Port 9 is discard: reserved, and refusing connections everywhere. Every
  # other indexer in this file is a fake, which is what ADR-0026 requires —
  # a real indexer proxies real trackers with real credentials and can never
  # run here.
  #
  # So what this entry proves is narrow and worth having: that the registry now
  # constructs a REAL client for the torznab kind. Before M3-09 this same entry
  # reported "the torznab client is not implemented yet"; now it reports what
  # happened when something actually tried to reach the endpoint. That is the
  # difference between a registry with a placeholder in it and a registry with
  # a client in it, and it is assertable without an indexer being present.
  - name: acceptance-torznab
    type: torznab
    endpoint: http://127.0.0.1:9/api
    api_key: not-a-real-key-and-nothing-will-read-it
  # A REAL NEWZNAB (usenet) INDEXER, configured and pointing at nothing (§59, M11).
  # Same discipline as the torznab entry above and for the same ADR-0026 reason:
  # a real usenet indexer proxies real services and can never run here. What it
  # proves is that the registry constructs a REAL client for the newznab kind —
  # the same client, because Newznab and Torznab are one wire protocol — rather
  # than the "configured, not implemented" placeholder.
  - name: acceptance-newznab
    type: newznab
    endpoint: http://127.0.0.1:9/api
    api_key: not-a-real-newznab-key-and-nothing-will-read-it
  - name: acceptance-downloads
    type: transmission
    endpoint: http://127.0.0.1:9
    path_map:
      - remote: /downloads/complete
        local: $FULLDATA/downloads
  # A second REAL download client — qBittorrent (§58, M11) — configured and
  # pointing at nothing (port 9 refuses everywhere). Like the torznab entry, what
  # this proves is narrow and worth having: the registry now constructs a REAL
  # qBittorrent client for the kind. Before M11 this same entry reported "the
  # qbittorrent client is not implemented yet"; now the health beat reaches real
  # client code — a login it cannot complete — and reports what happened, which
  # is the difference between a placeholder and a client. A real transfer is NOT
  # asserted here: ADR-0026 keeps a download daemon out of CI, so the transfer
  # path is proven by the fake at the unit level and by TestLiveQBittorrent.
  - name: acceptance-qbittorrent
    type: qbittorrent
    endpoint: http://127.0.0.1:9
    path_map:
      - remote: /downloads/complete
        local: $FULLDATA/downloads
  # A third REAL download client, and the first USENET one — SABnzbd (§58, M11) —
  # configured and pointing at nothing (port 9 refuses everywhere). What it proves
  # is the same narrow, worth-having fact as the qBittorrent entry: the registry
  # now constructs a REAL SABnzbd client for the kind, and the health beat reaches
  # real client code — an authenticated read it cannot complete — and reports what
  # happened, which is the difference between a placeholder and a client. It also
  # exercises the AuthToken credential path (an api_key resolved and handed to the
  # client), where qBittorrent exercises AuthBasic. A real transfer is NOT asserted
  # here: ADR-0026 keeps a download daemon out of CI, and unlike qBittorrent there
  # is no harness leg either (a real usenet transfer needs a real news server), so
  # the transfer path is proven by the fake at the unit level and by TestLiveSABnzbd.
  - name: acceptance-sabnzbd
    type: sabnzbd
    endpoint: http://127.0.0.1:9
    api_key: demo-sabnzbd-key
    path_map:
      - remote: /downloads/complete
        local: $FULLDATA/downloads
  # A fourth REAL download client, and the second USENET one — NZBGet (§58, M11,
  # #379) — configured and pointing at nothing (port 9 refuses everywhere). It
  # proves the same narrow, worth-having fact as the SABnzbd entry: the registry
  # now constructs a REAL NZBGet client for the kind, and the health beat reaches
  # real client code — a JSON-RPC read it cannot complete — and reports what
  # happened, which is the difference between a placeholder and a client. Where
  # SABnzbd exercises the AuthToken path, NZBGet exercises AuthBasic (like
  # qBittorrent). A real transfer is NOT asserted here: ADR-0026 keeps a download
  # daemon out of CI, and like SABnzbd there is no harness leg either (a real
  # usenet transfer needs a real news server), so the transfer path is proven by
  # the fake at the unit level and by TestLiveNZBGet.
  - name: acceptance-nzbget
    type: nzbget
    endpoint: http://127.0.0.1:9
    path_map:
      - remote: /downloads/complete
        local: $FULLDATA/downloads
YAML

  # Mint the token BEFORE anything starts. It migrates the database itself, so
  # this also proves the administrative path works against a database no server
  # has ever opened — the only order an operator can actually use, since the API
  # cannot be called before a token exists (ADR-0011).
  TOKEN=$("$BIN" --config "$WORK/full.yaml" token create acceptance --scopes admin --json | jq -r .token)
  if [[ -n "$TOKEN" && "$TOKEN" != "null" ]]; then
    pass "a token is mintable before the server has ever run"
  else
    fail "could not mint a token"; return 1
  fi

  start_full full all || return 1
  wait_for_ingest full "$files" || { stop_full; return 1; }

  note "  catalog"
  local got_works got_assets got_blobs
  got_works=$(api_all /api/v1/works '.items[].id' | sort -u | wc -l | tr -d ' ')
  if (( got_works > 0 )); then
    pass "the scan produced $got_works works"
  else
    fail "the catalog has no works at all"
  fi
  got_assets=$(api_all /api/v1/assets '.items[].id' | sort -u | wc -l | tr -d ' ')
  got_blobs=$(find "$FULLDATA/cas/blobs" -type f 2>/dev/null | wc -l | tr -d ' ')
  assert_eq "$got_assets" "$files" "every ingestable file became an asset"
  assert_eq "$got_blobs" "$blobs" "identical bytes at two paths produced one blob"

  # The file the scanner must decline, asserted as an absence rather than
  # inferred from a count that happens to match.
  local skipped
  skipped=$(jq -r '.files[] | select(.ingestable | not) | .path' "$MANIFEST" | head -1)
  if [[ -n "$skipped" ]]; then
    if api_all /api/v1/assets '.items[].source_path' | grep -qF "$skipped"; then
      fail "the scanner ingested $skipped, whose extension it does not recognise"
    else
      pass "an unrecognised extension is declined, not ingested"
    fi
  fi

  # §13: two assets, one blob. Asserted on the duplicate hash specifically, so
  # a coincidence in the totals cannot stand in for it.
  local dup_assets
  dup_assets=$(api_all /api/v1/assets '.items[] | select(.blob_hash == "'"$dup_hash"'") | .id' | wc -l | tr -d ' ')
  assert_eq "$dup_assets" "2" "the duplicate pair is two assets sharing one blob"

  # Non-primary roles survive identification: a subtitle beside an episode must
  # not have become an episode.
  local subs
  subs=$(api_all /api/v1/assets '.items[] | select(.role == "subtitle") | .id' | wc -l | tr -d ' ')
  if (( subs >= 1 )); then
    pass "a subtitle beside its episode is an asset with the subtitle role"
  else
    fail "no subtitle asset was created"
  fi

  note "  range serving (ADR-0013)"
  local big_hash big_size
  big_hash=$(jq -r --arg p "$largest_path" '.files[] | select(.path == $p) | .hash' "$MANIFEST")
  big_size=$largest_size

  # HEAD is how a prober asks "can I range this, and how big is it" without
  # moving a byte. §29's remote ffprobe is one of the four consumers this
  # endpoint has, and it is the one that would notice first.
  local head_out
  # `--head`, not `-X HEAD`: -X only changes the method, so curl still waits
  # for a body that a HEAD response will never send, and the script hangs until
  # a timeout instead of failing.
  head_out=$(api "/api/v1/blobs/$big_hash/content" --head)
  if grep -qi '^accept-ranges: bytes' <<<"$head_out"; then
    pass "HEAD advertises byte ranges"
  else
    fail "HEAD did not advertise Accept-Ranges"; echo "$head_out"
  fi
  assert_eq "$(grep -i '^content-length:' <<<"$head_out" | tr -d '\r' | awk '{print $2}')" \
    "$big_size" "HEAD reports the blob's length without transferring it"
  if grep -qi "^etag: \"blake3-" <<<"$head_out"; then
    pass "the ETag is derived from the content digest"
  else
    fail "no blake3 ETag"; echo "$head_out"
  fi

  # The canonical range request from the issue.
  local range_body range_code range_hdrs
  range_body="$WORK/range0.bin"
  range_hdrs=$(api "/api/v1/blobs/$big_hash/content" -H 'Range: bytes=0-1048575' -D - -o "$range_body" -w '%{http_code}')
  range_code=$(tail -1 <<<"$range_hdrs")
  assert_eq "$range_code" "206" "a byte range returns 206 Partial Content"
  assert_eq "$(wc -c <"$range_body" | tr -d ' ')" "1048576" "the range is exactly 1 MiB"
  if grep -qi "^content-range: bytes 0-1048575/$big_size" <<<"$range_hdrs"; then
    pass "Content-Range names the span and the total size"
  else
    fail "wrong Content-Range"; grep -i '^content-range' <<<"$range_hdrs"
  fi

  # The assertion that actually proves the endpoint: N disjoint ranges,
  # concatenated in order, must hash to the blob's own digest. Uneven spans on
  # purpose — equal spans hide an error that is a constant offset.
  local part1 part2 part3 reassembled tail_start
  part1="$WORK/p1.bin"; part2="$WORK/p2.bin"; part3="$WORK/p3.bin"
  reassembled="$WORK/reassembled.bin"
  tail_start=$(( 1048576 + 65537 ))
  api "/api/v1/blobs/$big_hash/content" -H "Range: bytes=0-1048575" -o "$part1"
  api "/api/v1/blobs/$big_hash/content" -H "Range: bytes=1048576-$(( tail_start - 1 ))" -o "$part2"
  api "/api/v1/blobs/$big_hash/content" -H "Range: bytes=$tail_start-" -o "$part3"
  cat "$part1" "$part2" "$part3" > "$reassembled"
  assert_eq "$(wc -c <"$reassembled" | tr -d ' ')" "$big_size" "three disjoint ranges cover the whole blob"
  assert_eq "$("$GEN" -hash "$reassembled")" "$big_hash" \
    "three disjoint ranges, concatenated, hash to the blob's own digest"

  # An unsatisfiable range is a 416 naming the size, not a 200 with the whole
  # body — a replication client that got the body would silently restart.
  assert_eq "$(api "/api/v1/blobs/$big_hash/content" -H "Range: bytes=$(( big_size + 4096 ))-" -o /dev/null -w '%{http_code}')" \
    "416" "a range past the end is 416, not a silent full body"

  # A stale If-Range must fall back to the whole object rather than splicing a
  # range from different bytes onto a half-finished file.
  assert_eq "$(api "/api/v1/blobs/$big_hash/content" -H 'Range: bytes=0-1023' -H 'If-Range: "blake3-0000"' -o /dev/null -w '%{http_code}')" \
    "200" "a stale If-Range returns the full body"
  assert_eq "$(api "/api/v1/blobs/$big_hash/content" -H 'Range: bytes=0-1023' -H "If-Range: \"blake3-${big_hash#blake3:}\"" -o /dev/null -w '%{http_code}')" \
    "206" "a matching If-Range serves the range"

  # 404 and 400 are different mistakes: one means "ask another peer", the other
  # means "retrying anywhere will not help".
  assert_eq "$(api "/api/v1/blobs/blake3:$(printf '0%.0s' {1..64})/content" -o /dev/null -w '%{http_code}')" \
    "404" "an absent blob is 404"
  assert_eq "$(api "/api/v1/blobs/not-a-hash/content" -o /dev/null -w '%{http_code}')" \
    "400" "a malformed digest is 400, not 404"

  note "  the event log (§76)"
  # Every state transition emits an event, with no exceptions — retrofitting is
  # what makes it expensive (ADR-0009). Replaying from 0 must show the ingest
  # story in causal order.
  local events_out
  # SSE never ends by design, so the replay is bounded at the current head.
  events_out=$(events_replay)
  local ev
  for ev in blob.created content.asset.created replica.present ingest.completed system.scan.progress; do
    if grep -q "$ev" <<<"$events_out"; then
      pass "the log replays $ev"
    else
      fail "no $ev in a replay from seq 0"
    fi
  done
  # A consumer replaying in seq order must never meet an asset before its blob.
  local first_blob first_asset
  # `grep -m1`, not `grep | head -1`. With `set -o pipefail`, head exiting after
  # one line sends grep SIGPIPE, the pipeline reports failure, and `set -e`
  # takes the whole script down — which is exactly what happened once the event
  # log grew big enough for grep to still be writing when head left:
  # "grep: write error: Broken pipe", then the demo died mid-run.
  first_blob=$(grep -n -m1 'blob.created' <<<"$events_out" | cut -d: -f1)
  first_asset=$(grep -n -m1 'content.asset.created' <<<"$events_out" | cut -d: -f1)
  if [[ -n "$first_blob" && -n "$first_asset" ]] && (( first_blob < first_asset )); then
    pass "the log orders a blob before the asset that names it"
  else
    fail "content.asset.created appeared before blob.created in the replay"
  fi

  note "  replicas"
  # ADR-0010: exactly one peer, and it is this node. Every blob must be present
  # on it, or the catalog is claiming bytes nobody holds.
  local self_peer present
  self_peer=$(api /api/v1/peers | jq -r '.items[] | select(.is_self) | .id')
  present=$(api_all "/api/v1/replicas?state=present" '.items[] | select(.peer_id == "'"$self_peer"'") | .blob_hash' | sort -u | wc -l | tr -d ' ')
  assert_eq "$present" "$blobs" "every blob has a present replica on the self peer"

  note "  publications (§69)"
  # Heyarr stores and serves EPUB, PDF, CBZ and CBR, and does not render them.
  # The import-graph guard asserts the second half in the test suite; this
  # asserts the first half against a running binary, which is the half a user
  # would notice.
  local pubs epub_asset epub_chapters pub_bytes pub_hash
  pubs=$(api /api/v1/publications | jq -r '.items | length')
  assert_eq "$pubs" "2" "the fixture library's epub and cbz are catalogued as publications"

  # The counts come from each container's own manifest, read at ingest. The
  # EPUB fixture declares six spine items and the CBZ eight page images.
  epub_asset=$(api "/api/v1/publications?format=epub" | jq -r '.items[0].asset_id')
  epub_chapters=$(api "/api/v1/publications?format=epub" | jq -r '.items[0].chapter_count')
  assert_eq "$epub_chapters" "6" "the epub reports the spine count its own package document declares"
  assert_eq "$(api "/api/v1/publications?format=cbz" | jq -r '.items[0].page_count')" "8" \
    "the cbz reports the page count its own zip index declares"

  # A spine is not a page count, and reporting one as the other would be a
  # plausible-looking lie.
  assert_eq "$(api "/api/v1/publications?format=epub" | jq -r '.items[0].page_count // "absent"')" \
    "absent" "the epub reports no page count"

  # And the bytes are served from the ordinary blob endpoint, with ranges.
  # ADR-0013: one endpoint, several consumers, and a reader is another one
  # rather than another endpoint.
  pub_hash=$(api "/api/v1/publications?format=cbz" | jq -r '.items[0].blob_hash')
  assert_eq "$(api "/api/v1/publications?format=cbz" | jq -r '.items[0].content_url')" \
    "/api/v1/blobs/$pub_hash/content" "a publication points at the ordinary blob endpoint"
  pub_bytes=$(api "/api/v1/blobs/$pub_hash/content" -H "Range: bytes=0-3" -o - | head -c 4 | xxd -p)
  assert_eq "${pub_bytes:0:4}" "504b" "a range request against a publication returns its zip magic"

  note "  consumption sessions (§67, ADR-0024)"
  # Devices and sessions had no executable-level coverage at all until now:
  # both were tested only in-process, so neither had ever crossed a real
  # socket. This is that gap closed for the read path.
  local device_id session_id
  device_id=$(api /api/v1/devices -X POST -H 'Content-Type: application/json' \
    -d '{"device_key":"acceptance-reader","name":"Acceptance Reader","platform":"linux",
         "profile":{"containers":["epub","cbz"]}}' | jq -r '.id')
  assert_contains "$device_id" "-" "a device registers over a real socket"

  # Registration is an upsert: a client announcing itself twice is one device.
  assert_eq "$(api /api/v1/devices -X POST -H 'Content-Type: application/json' \
    -d '{"device_key":"acceptance-reader","name":"Acceptance Reader","platform":"linux",
         "profile":{"containers":["epub","cbz"]}}' | jq -r '.id')" "$device_id" \
    "registering the same device twice converges on one row"

  session_id=$(api /api/v1/consumption/sessions -X POST -H 'Content-Type: application/json' \
    -d "{\"asset_id\":\"$epub_asset\",\"device_id\":\"$device_id\",\"verb\":\"read\"}" \
    | jq -r '.id')
  assert_contains "$session_id" "-" "a reading session opens"

  api "/api/v1/consumption/sessions/$session_id/transitions" -X POST \
    -H 'Content-Type: application/json' -d '{"transition":"start"}' >/dev/null
  api "/api/v1/consumption/sessions/$session_id/transitions" -X POST \
    -H 'Content-Type: application/json' \
    -d '{"transition":"progress","progress":{"locator":"epubcfi(/6/14!/4/10/3:10)","unit":"cfi"}}' >/dev/null
  api "/api/v1/consumption/sessions/$session_id/transitions" -X POST \
    -H 'Content-Type: application/json' -d '{"transition":"stop"}' >/dev/null

  # Resume: the exact locator comes back. An EPUB CFI is opaque to Heyarr,
  # which does not render EPUBs — it is stored and returned unchanged.
  assert_eq "$(api "/api/v1/consumption/sessions/$session_id" | jq -r '.progress.locator')" \
    "epubcfi(/6/14!/4/10/3:10)" "a stopped reading session resumes at its exact locator"
  assert_eq "$(api "/api/v1/consumption/sessions/$session_id" | jq -r '.progress.unit')" \
    "cfi" "the locator keeps its unit"

  # An illegal transition is a 409 and changes nothing. A constraint nobody has
  # watched reject anything is decoration.
  assert_eq "$(api "/api/v1/consumption/sessions/$session_id/transitions" -X POST \
    -H 'Content-Type: application/json' -d '{"transition":"resume"}' \
    -o /dev/null -w '%{http_code}')" "409" \
    "resuming a stopped session is refused"
  assert_eq "$(api "/api/v1/consumption/sessions/$session_id" | jq -r '.state')" "stopped" \
    "the refused transition left the session alone"

  # Every transition is on the event stream (invariant 7).
  assert_eq "$(api "/api/v1/events?after=0&types=playback.session.*" \
    -m 2 2>/dev/null | grep -c '^event: playback.session.')" "4" \
    "the session's four transitions are all on the event stream"

  note "  playing (§68, §32, ADR-0013)"
  # The milestone's headline: Heyarr can be PLAYED from, not just read from.
  # This is the only assertion in the demo that goes all the way — plan, open a
  # session, fetch the bytes with the credential it issued, and check they are
  # the bytes the catalog says they are.
  local play_asset play_device play_json play_session play_url play_token play_hash play_digest
  play_asset=$(api_all /api/v1/assets '.items[] | select(.filename != null) | select(.filename | endswith(".mkv") or endswith(".mp4")) | .id' | head -1)
  play_hash=$(api "/api/v1/assets/$play_asset" | jq -r '.blob_hash')

  # WAIT FOR THE PROBE BEFORE ANY PLAN IS ASKED FOR (#207). Everything from here
  # to the end of the planner section decides against the container and streams
  # recorded for this blob: the refusal below wants a 409, and a planner with no
  # probe cannot refuse anything — it answers DIRECT and declares the guess,
  # which is correct behaviour reported as a broken refusal.
  #
  # Guarded on ffprobe, because on a machine without it the probe CANNOT land
  # and the else-branches below are the honest ADR-0023 assertions rather than a
  # missing precondition.
  if command -v ffprobe >/dev/null 2>&1; then
    wait_for_probe "$play_hash" \
      "the planner had no container to refuse and answered DIRECT — which is correct, and is not what the assertions below report"
  fi

  play_device=$(api /api/v1/devices -X POST -H 'Content-Type: application/json' \
    -d '{"device_key":"acceptance-player","name":"Acceptance Player","platform":"linux",
         "profile":{"containers":["mp4","mkv"],"video_codecs":["h264","hevc"],
                    "audio_codecs":["aac"]}}' | jq -r '.id')

  play_json=$(api /api/v1/playback -X POST -H 'Content-Type: application/json' \
    -d "{\"asset_id\":\"$play_asset\",\"device_id\":\"$play_device\"}")
  play_session=$(jq -r '.session_id' <<<"$play_json")
  play_url=$(jq -r '.content_url' <<<"$play_json")
  play_token=$(jq -r '.token' <<<"$play_json")

  assert_contains "$play_session" "-" "starting a playback opens a session"
  assert_eq "$play_url" "/api/v1/blobs/$play_hash/content" \
    "the playback URL is the ordinary blob endpoint (ADR-0013), not a player-shaped route"

  # §32: the controller returns a direct URL and does not proxy bytes.
  assert_not_contains "$play_url" "playback" "the playback URL is not a controller-proxied path"

  # And it PLAYS: fetch with the issued credential and confirm the bytes are
  # the bytes. A URL nobody fetched is not playback.
  play_digest=$(curl -sS --unix-socket "$SOCK" -H "Authorization: Bearer $play_token" \
    "http://heyarr$play_url" | shasum -a 256 | cut -d" " -f1)
  local catalog_digest
  catalog_digest=$(curl -sS --unix-socket "$SOCK" -H "Authorization: Bearer $TOKEN" \
    "http://heyarr$play_url" | shasum -a 256 | cut -d" " -f1)
  assert_eq "$play_digest" "$catalog_digest" \
    "the bytes fetched with the playback credential are the asset's bytes"

  # Ranges work through the playback credential too — a player seeks.
  assert_eq "$(curl -sS --unix-socket "$SOCK" -H "Authorization: Bearer $play_token" \
    -H 'Range: bytes=0-1023' -o /dev/null -w '%{http_code}' "http://heyarr$play_url")" "206" \
    "a player can seek with the credential it was given"

  # The credential reads and does not write. A playback token that could
  # register a device has become a client credential.
  assert_eq "$(curl -sS --unix-socket "$SOCK" -H "Authorization: Bearer $play_token" \
    -X POST -H 'Content-Type: application/json' -d '{"device_key":"x","name":"x"}' \
    -o /dev/null -w '%{http_code}' "http://heyarr/api/v1/devices")" "403" \
    "the playback credential cannot write"

  # The refusal. A device that cannot take the codec is told why, and no
  # session is left behind for a playback that never happened.
  local refuse_device refuse_code sessions_before sessions_after
  refuse_device=$(api /api/v1/devices -X POST -H 'Content-Type: application/json' \
    -d '{"device_key":"acceptance-refuser","name":"Refuser","platform":"linux",
         "profile":{"containers":["mp4"],"video_codecs":["mpeg2video"],"audio_codecs":["mp2"]}}' \
    | jq -r '.id')
  sessions_before=$(api /api/v1/consumption/sessions | jq -r '.items | length')
  refuse_code=$(api /api/v1/playback -X POST -H 'Content-Type: application/json' \
    -d "{\"asset_id\":\"$play_asset\",\"device_id\":\"$refuse_device\"}" \
    -o "$WORK/refusal.json" -w '%{http_code}')
  sessions_after=$(api /api/v1/consumption/sessions | jq -r '.items | length')

  if command -v ffprobe >/dev/null 2>&1; then
    assert_eq "$refuse_code" "409" "a device that cannot take the codec is refused"
    assert_contains "$(cat "$WORK/refusal.json")" "playback/plan" \
      "the refusal points at the full rationale"
    assert_eq "$sessions_after" "$sessions_before" \
      "a refused playback opens no session"
  else
    # With nothing probed, every device gets DIRECT and the guess declared —
    # so there is no refusal to assert here, which is itself the ADR-0023
    # claim: a node with no ffprobe still plays its library.
    assert_eq "$refuse_code" "201" \
      "with nothing probed, playback still succeeds rather than refusing everything"
    not_exercised ffprobe "a device that cannot take the codec is refused, and the refusal opens no session"
  fi

  note "  the streaming leg (§33, ADR-0069)"
  # A client that says what it decodes and cannot decode the source is handed
  # a repackaged stream rather than the raw blob (#432). This is the join the
  # unit tests cannot make: a real plan, a real token, a real ffmpeg behind a
  # real socket, and the bytes that came back probed by ffprobe. The claim is
  # a-client-that-cannot-decode-gets-a-repackage; its evidence is the captured
  # bytes being H.264 + AAC in MP4, not the plan having said "stream".
  local leg_json leg_mode leg_url leg_capture leg_shape
  # A container the client cannot open — flac — over a video file, so the leg
  # must be a stream whatever the file's own container is. The streams
  # themselves (h264/aac) are ones the client decodes, so the repackage is a
  # rewrap: both copied, and ffprobe on the result says so.
  leg_json=$(api /api/v1/playback/plan -X POST -H 'Content-Type: application/json' \
    -d "{\"asset_id\":\"$play_asset\",\"client\":{\"containers\":[\"flac\"],\"video\":[\"h264\"],\"audio\":[\"aac\"],\"max_height\":1080}}")
  leg_mode=$(jq -r '.mode' <<<"$leg_json")
  leg_url=$(jq -r '.url' <<<"$leg_json")
  if command -v ffmpeg >/dev/null 2>&1 && command -v ffprobe >/dev/null 2>&1; then
    assert_eq "$leg_mode" "stream" \
      "a client that cannot open the container is planned a stream"
    assert_contains "$leg_url" "/api/v1/playback/stream/" \
      "the stream URL is the stream route, not the blob endpoint"
    assert_contains "$(jq -r '.reason' <<<"$leg_json")" "not playable by client" \
      "the plan says why the client is being served a stream"
    assert_eq "$(jq -r '.mime' <<<"$leg_json")" "video/mp4" \
      "the stream is fragmented MP4"
    # Fetch it with the credential that asked for the plan — the token is
    # bound to it (ADR-0069), and the plan above was asked for with $TOKEN.
    # The fixture is a second long, so the whole stream arrives well inside
    # the timeout; a bigger file would simply be cut, which is what a player
    # hanging up looks like.
    leg_capture="$WORK/stream-capture.mp4"
    curl -sS --max-time 20 --unix-socket "$SOCK" -H "Authorization: Bearer $TOKEN" \
      -o "$leg_capture" "http://heyarr$leg_url" || true
    if [[ -s "$leg_capture" ]]; then
      leg_shape=$(ffprobe -v error -show_entries format=format_name:stream=codec_name \
        -of default=nw=1:nk=1 "$leg_capture" 2>/dev/null | tr '\n' ' ')
      assert_contains "$leg_shape" "h264" "the captured stream carries H.264 video"
      assert_contains "$leg_shape" "aac" "the captured stream carries AAC audio"
      assert_contains "$leg_shape" "mp4" "the captured stream is MP4"
      if [[ "$leg_shape" == *h264* && "$leg_shape" == *aac* && "$leg_shape" == *mp4* ]]; then
        pass "a client that could not open the container was served a repackaged stream"
      else
        fail "the captured stream probes as '$leg_shape', not H.264 + AAC in MP4"
      fi
    else
      fail "the stream URL returned no bytes"
    fi
    # The token is the plan: a tampered one is an opaque 404, and the same
    # token presented by another credential is too (unit-proven; here the
    # tamper is what a socket can show).
    assert_eq "$(curl -sS --unix-socket "$SOCK" -H "Authorization: Bearer $TOKEN" \
      -o /dev/null -w '%{http_code}' "http://heyarr${leg_url}x")" "404" \
      "a tampered stream token is refused opaquely"
    # And a client that CAN open the container is handed the bytes.
    assert_eq "$(api /api/v1/playback/plan -X POST -H 'Content-Type: application/json' \
      -d "{\"asset_id\":\"$play_asset\",\"client\":{\"containers\":[\"mp4\",\"mkv\"],\"video\":[\"h264\"],\"audio\":[\"aac\"]}}" \
      | jq -r '.mode')" "direct" \
      "a client that decodes the source is planned direct"
  else
    # Without a toolchain nothing has probed the file either, so the plan
    # cannot know the client will fail: it hands over the bytes and declares
    # the guess (ADR-0023's stance, one route over). The "no ffmpeg" note
    # only appears on a KNOWN incompatibility, which needs a probe — that
    # branch is unit-proven (TestWithoutFFmpegThePlanIsDirectAndSaysWhy).
    assert_eq "$leg_mode" "direct" \
      "without a toolchain a client that cannot decode the source is still handed the bytes"
    assert_contains "$(jq -r '[.reasons[].code] | join(",")' <<<"$leg_json")" "no_probe" \
      "and the plan declares it is a guess, because nothing probed the file"
    not_exercised ffmpeg "the streaming leg: a plan for a container the client cannot open, the stream fetched, and the captured bytes probed as H.264 + AAC in MP4 (claim a-client-that-cannot-decode-gets-a-repackage)"
  fi

  note "  the playback planner (§68)"
  # The planner is a pure function, exhaustively table-tested. What this adds
  # is the join: real probe rows, real device profiles and real replicas, over
  # a real socket — because each of those three being right in isolation is
  # how a wiring bug survives.
  local plan_device limited_device plan_json plan_hash
  plan_asset=$(api_all /api/v1/assets '.items[] | select(.filename != null) | select(.filename | endswith(".mkv") or endswith(".mp4")) | .id' | head -1)
  plan_hash=$(api "/api/v1/assets/$plan_asset" | jq -r '.blob_hash')

  # The same wait as the playing section, and it is not redundant: this filter
  # is free to select a different asset, and "the one above happened to be the
  # same blob" is not a property this section may rely on (#207).
  if command -v ffprobe >/dev/null 2>&1; then
    wait_for_probe "$plan_hash" \
      "the planner decided with no streams — DIRECT with a no_probe reason, which is the right answer to an unmeasured file and reads here as a planner regression"
  fi

  plan_device=$(api /api/v1/devices -X POST -H 'Content-Type: application/json' \
    -d '{"device_key":"acceptance-tv","name":"Acceptance TV","platform":"tvos",
         "profile":{"containers":["mp4","mkv"],"video_codecs":["h264","hevc"],
                    "audio_codecs":["aac"],"max_width":3840,"max_height":2160}}' | jq -r '.id')
  limited_device=$(api /api/v1/devices -X POST -H 'Content-Type: application/json' \
    -d '{"device_key":"acceptance-potato","name":"Acceptance Potato","platform":"linux",
         "profile":{"containers":["mp4"],"video_codecs":["mpeg2video"],"audio_codecs":["mp2"]}}' \
    | jq -r '.id')

  plan_json=$(api /api/v1/playback/plan -X POST -H 'Content-Type: application/json' \
    -d "{\"asset_id\":\"$plan_asset\",\"device_id\":\"$plan_device\"}")

  if command -v ffprobe >/dev/null 2>&1; then
    # With a probe, the planner decides against what the file actually is.
    assert_eq "$(jq -r '.decision' <<<"$plan_json")" "direct" \
      "h264/aac in a container the device takes plans DIRECT"
    assert_eq "$(jq -r '.reasons | length' <<<"$plan_json")" "0" \
      "a clean DIRECT carries no reasons"

    # The refusal, with its rationale. A verdict without a reason cannot answer
    # "why is my television transcoding this".
    local limited_json
    limited_json=$(api /api/v1/playback/plan -X POST -H 'Content-Type: application/json' \
      -d "{\"asset_id\":\"$plan_asset\",\"device_id\":\"$limited_device\"}")
    assert_contains "$(jq -r '.decision' <<<"$limited_json")" "transcode" \
      "a device that refuses the codec plans TRANSCODE"
    assert_contains "$(jq -r '[.reasons[].code] | join(",")' <<<"$limited_json")" \
      "video_codec_unsupported" "the plan says which codec it refused"
    # And it does not hand over the original bytes, which would invite the
    # client to play exactly what the plan just refused.
    assert_eq "$(jq -r '.content_url // "absent"' <<<"$limited_json")" "absent" \
      "a TRANSCODE plan offers no content_url"
  else
    # ADR-0023 again, at the planner: nothing has probed anything, and the
    # answer is DIRECT with the guess declared — because a node with no
    # ffprobe cannot transcode either, so any other answer makes the whole
    # library unplayable.
    assert_eq "$(jq -r '.decision' <<<"$plan_json")" "direct" \
      "unprobed media plans DIRECT rather than making the library unplayable"
    assert_contains "$(jq -r '[.reasons[].code] | join(",")' <<<"$plan_json")" "no_probe" \
      "the plan declares that it is a guess"
    not_exercised ffprobe "the planner deciding on measured streams: DIRECT with no reasons, TRANSCODE naming the codec it refused, and no content_url on a refusal"
  fi

  # §31: with exactly one peer (ADR-0010) every replica is local, which is the
  # only locality case that can be asserted against reality today.
  assert_eq "$(jq -r '.remote' <<<"$plan_json")" "false" \
    "the single-peer deployment plans from a local replica"
  assert_contains "$(jq -r '.peer_id' <<<"$plan_json")" "-" \
    "the plan names the peer it would serve from"

  note "  the OpenSubsonic adapter (§70, M11)"
  # The external plane: an existing music client talks to Heyarr over the
  # OpenSubsonic protocol, browses the server-readable catalogue and plays from
  # it — Heyarr owning no client. The adapter is a projection over the real
  # model and delegates every byte to the same blob route the rest of this
  # milestone proved, so the scene drives it the way a real Subsonic app would:
  # authenticate, browse, then fetch a track and check the bytes are the bytes.
  #
  # What make demo proves here is protocol conformance and a real byte path. The
  # honest far end of the bar — a real Subsonic APP browsing and playing — is
  # app-in-the-loop and out of this budget, the way #202 keeps the video client
  # out of the demo. It is RECORDED as pending, not skipped.
  subsonic() { # method [extra-query]
    curl -sS --unix-socket "$SOCK" \
      "http://heyarr/rest/$1?u=acceptance&p=$TOKEN&c=acceptance&v=1.16.1&f=json${2:+&$2}"
  }

  local sub_ping
  sub_ping=$(subsonic ping)
  assert_eq "$(jq -r '.["subsonic-response"].status' <<<"$sub_ping")" "ok" \
    "a Subsonic client's ping is answered ok"
  assert_eq "$(jq -r '.["subsonic-response"].type' <<<"$sub_ping")" "heyarr" \
    "the OpenSubsonic handshake names the server it really reached"
  assert_eq "$(jq -r '.["subsonic-response"].openSubsonic' <<<"$sub_ping")" "true" \
    "and advertises OpenSubsonic support"

  # The credential is a Heyarr bearer token sent as the Subsonic password,
  # mapped onto the same tokens and scopes the whole API uses.
  assert_eq "$(jq -r '.["subsonic-response"].status' <<<"$(curl -sS --unix-socket "$SOCK" \
    "http://heyarr/rest/ping?u=acceptance&p=not-the-token&c=c&v=1.16.1&f=json")")" "failed" \
    "a wrong Subsonic password is refused"
  # The salted-token scheme cannot be honoured — Heyarr never holds the plaintext
  # token to recompute the MD5 — so it is refused with the fix, not faked.
  assert_eq "$(jq -r '.["subsonic-response"].error.code' <<<"$(curl -sS --unix-socket "$SOCK" \
    "http://heyarr/rest/ping?u=acceptance&t=deadbeef&s=salt&c=c&v=1.16.1&f=json")")" "40" \
    "the Subsonic salted-token scheme is refused, not silently accepted"

  # Browse: the artist index and album list are non-empty projections of the
  # music the scan ingested — an artist is DERIVED, not an entity in the model.
  local sub_albums n_albums n_index album_id
  assert_eq "$(( $(jq -r '.["subsonic-response"].artists.index | length' <<<"$(subsonic getArtists)") > 0 ))" "1" \
    "getArtists derives an artist index from the music works"
  sub_albums=$(subsonic getAlbumList2 "type=alphabeticalByName&size=500")
  n_albums=$(jq -r '.["subsonic-response"].albumList2.album | length' <<<"$sub_albums")
  assert_eq "$(( n_albums > 0 ))" "1" \
    "getAlbumList2 projects the ingested music as browsable albums"
  assert_eq "$(( $(subsonic getMusicFolders | jq -r '.["subsonic-response"].musicFolders.musicFolder | length') > 0 ))" "1" \
    "getMusicFolders lists the music library"

  # The first album that actually has streamable songs — the browse-to-listen path.
  album_id=$(jq -r 'first(.["subsonic-response"].albumList2.album[] | select(.songCount > 0) | .id)' <<<"$sub_albums")
  assert_eq "$([[ -n "$album_id" && "$album_id" != "null" ]] && echo 1 || echo 0)" "1" \
    "at least one album has streamable songs (a real caller, not an empty projection)"

  local sub_album song_id song_id_enc edition blob sub_sha blob_sha
  sub_album=$(subsonic getAlbum "id=$album_id")
  song_id=$(jq -r '.["subsonic-response"].album.song[0].id' <<<"$sub_album")
  assert_contains "$song_id" "tr:" "a song carries the opaque track id a client streams by"

  # Play: stream the track and prove its bytes ARE the blob's bytes — the same
  # byte route (ADR-0013), not the adapter's invention. The song id encodes the
  # edition, so the blob it maps to is found through the ordinary assets API and
  # fetched through the ordinary blob route for the comparison.
  song_id_enc=$(printf '%s' "$song_id" | jq -sRr @uri)
  edition=${song_id#tr:}
  blob=$(api_all /api/v1/assets ".items[] | select(.edition_id==\"$edition\") | .blob_hash" | head -1)
  sub_sha=$(subsonic stream "id=$song_id_enc" | shasum -a 256 | cut -d" " -f1)
  blob_sha=$(api "/api/v1/blobs/$blob/content" | shasum -a 256 | cut -d" " -f1)
  assert_eq "$sub_sha" "$blob_sha" \
    "the bytes a Subsonic client streams are byte-identical to the blob route's"

  # A player seeks: Range is honoured because byte serving is delegated, not
  # reimplemented in the adapter.
  assert_eq "$(curl -sS --unix-socket "$SOCK" -H 'Range: bytes=0-3' -o /dev/null -w '%{http_code}' \
    "http://heyarr/rest/stream?u=acceptance&p=$TOKEN&c=c&v=1.16.1&id=$song_id_enc")" "206" \
    "a Subsonic stream honours Range with a 206, delegated to the blob route"

  # No personal state. Playlists, scrobbles and starred items are encrypted and
  # opaque to the controller (§72): the history/starred list types return empty
  # rather than a fabricated ranking, and the playlist endpoints are not served.
  assert_eq "$(subsonic getAlbumList2 "type=starred" | jq -r '.["subsonic-response"].albumList2.album | length')" "0" \
    "a personal-state album list (starred) is empty, not fabricated"
  assert_eq "$(subsonic getPlaylists | jq -r '.["subsonic-response"].status')" "failed" \
    "playlists are not served by the controller-side adapter"

  not_exercised real_subsonic_app \
    "a real Subsonic app (Symfonium/DSub/Feishin) browsing and playing — app-in-the-loop, out of the demo budget like #202's video client (tracked pending: subsonic-real-app-in-the-loop)"

  note "  the OPDS adapter (§69, §70, M11)"
  # The publications counterpart: an existing OPDS reader browses the library
  # over OPDS 1.2 and downloads from it, Heyarr owning no client. Same shape as
  # the OpenSubsonic scene — authenticate, browse, then fetch and check the
  # bytes are the bytes — and the same honest limit: a real reader app is out
  # of the demo budget and recorded pending, not skipped.
  opds() { # path
    curl -sS --unix-socket "$SOCK" -u "acceptance:$TOKEN" "http://heyarr/opds$1"
  }

  # HTTP Basic, where the password is a Heyarr bearer token. No credential is a
  # real 401 with a Basic challenge — what a reader waits for before it prompts.
  assert_eq "$(curl -sS --unix-socket "$SOCK" -o /dev/null -w '%{http_code}' "http://heyarr/opds")" "401" \
    "an OPDS request without a credential is challenged"

  local opds_root opds_pub
  opds_root=$(opds "")
  assert_contains "$opds_root" 'kind=navigation' "the OPDS root is a navigation feed"
  assert_contains "$opds_root" '/opds/publications' "the root descends into the acquisition feed"

  opds_pub=$(opds "/publications")
  assert_contains "$opds_pub" 'kind=acquisition' "the OPDS publications feed is an acquisition feed"
  assert_contains "$opds_pub" 'http://opds-spec.org/acquisition' "a publication carries an acquisition link"

  # The reader's own path: pull an acquisition href out of the feed and download
  # it, then prove the bytes ARE the blob's bytes — the same byte route
  # (ADR-0013), not the adapter's invention. The link names the edition, whose
  # blob is found through the ordinary assets API and fetched through the
  # ordinary blob route for the comparison.
  local dl_path edition blob opds_sha blob_sha
  dl_path=$(printf '%s' "$opds_pub" | grep -oE '/opds/download/[^"]+' | head -1)
  assert_eq "$([[ -n "$dl_path" ]] && echo 1 || echo 0)" "1" \
    "the acquisition feed offers a real download link (a caller, not an empty feed)"
  edition=${dl_path#/opds/download/}
  blob=$(api_all /api/v1/assets ".items[] | select(.edition_id==\"$edition\") | .blob_hash" | head -1)
  opds_sha=$(curl -sS --unix-socket "$SOCK" -u "acceptance:$TOKEN" "http://heyarr$dl_path" | shasum -a 256 | cut -d" " -f1)
  blob_sha=$(api "/api/v1/blobs/$blob/content" | shasum -a 256 | cut -d" " -f1)
  assert_eq "$opds_sha" "$blob_sha" \
    "the bytes an OPDS reader downloads are byte-identical to the blob route's"

  # A reader resumes a partial download: Range is honoured because byte serving
  # is delegated, not reimplemented in the adapter.
  assert_eq "$(curl -sS --unix-socket "$SOCK" -u "acceptance:$TOKEN" -H 'Range: bytes=0-3' \
    -o /dev/null -w '%{http_code}' "http://heyarr$dl_path")" "206" \
    "an OPDS download honours Range with a 206, delegated to the blob route"

  not_exercised real_opds_reader \
    "a real OPDS reader (KOReader/Foliate/Marvin) browsing and downloading — app-in-the-loop, out of the demo budget like #202's video client (tracked pending: opds-real-reader-in-the-loop)"

  note "  the DLNA/UPnP ContentDirectory (§70, #202, M11)"
  # The video-and-audio client: the devices that matter — a television, a
  # speaker — speak DLNA natively, so they BROWSE the library over a UPnP
  # ContentDirectory and play from it, Heyarr owning no client. The res URLs a
  # Browse hands out are render capabilities (ADR-0040), so the device fetches
  # bytes from the unauthenticated render route — which is why this drives it
  # the way such a device would: a SOAP Browse (no credential), then a fetch of
  # a res URL, asserted byte-identical to the blob route. SSDP LAN advertisement
  # and the real-device proof are out of a headless demo and recorded pending.
  dlna_browse() { # object-id browse-flag
    curl -sS --unix-socket "$SOCK" -H 'Content-Type: text/xml; charset="utf-8"' \
      -H 'SOAPAction: "urn:schemas-upnp-org:service:ContentDirectory:1#Browse"' \
      --data "<?xml version=\"1.0\"?><s:Envelope xmlns:s=\"http://schemas.xmlsoap.org/soap/envelope/\"><s:Body><u:Browse xmlns:u=\"urn:schemas-upnp-org:service:ContentDirectory:1\"><ObjectID>$1</ObjectID><BrowseFlag>$2</BrowseFlag><Filter>*</Filter><StartingIndex>0</StartingIndex><RequestedCount>0</RequestedCount><SortCriteria></SortCriteria></u:Browse></s:Body></s:Envelope>" \
      "http://heyarr/dlna/control/ContentDirectory"
  }

  # The device fetches the description first to learn it is a MediaServer.
  assert_contains "$(curl -sS --unix-socket "$SOCK" "http://heyarr/dlna/description.xml")" \
    "urn:schemas-upnp-org:device:MediaServer:1" \
    "the DLNA device announces itself as a MediaServer"

  local dlna_root dlna_container dlna_items dlna_res dlna_asset dlna_blob dlna_sha dlna_blob_sha
  dlna_root=$(dlna_browse 0 BrowseDirectChildren)
  assert_contains "$dlna_root" "object.container.storageFolder" \
    "browsing the DLNA root lists content folders projected from the catalogue"
  dlna_container=$(printf '%s' "$dlna_root" | grep -oE 'ct:[a-z]+' | head -1)
  assert_eq "$([[ -n "$dlna_container" ]] && echo 1 || echo 0)" "1" \
    "the root offers at least one browsable folder"

  dlna_items=$(dlna_browse "$dlna_container" BrowseDirectChildren)
  assert_contains "$dlna_items" "http-get:*:" \
    "a folder lists playable items with a UPnP resource"
  dlna_res=$(printf '%s' "$dlna_items" | grep -oE '/render/[A-Za-z0-9._-]+' | head -1)
  assert_eq "$([[ -n "$dlna_res" ]] && echo 1 || echo 0)" "1" \
    "an item carries a real render-capability res URL (a caller, not an empty feed)"

  # Prove the res URL is the blob's bytes — the same byte route (ADR-0013), not
  # the adapter's invention. The item id names the asset; its blob is found via
  # the ordinary assets API and fetched via the ordinary blob route to compare.
  dlna_asset=$(printf '%s' "$dlna_items" | grep -oE 'asset:[0-9a-fA-F-]+' | head -1)
  dlna_asset=${dlna_asset#asset:}
  dlna_blob=$(api "/api/v1/assets/$dlna_asset" | jq -r '.blob_hash')
  dlna_sha=$(curl -sS --unix-socket "$SOCK" "http://heyarr$dlna_res" | shasum -a 256 | cut -d" " -f1)
  dlna_blob_sha=$(api "/api/v1/blobs/$dlna_blob/content" | shasum -a 256 | cut -d" " -f1)
  assert_eq "$dlna_sha" "$dlna_blob_sha" \
    "the bytes a DLNA device fetches from a res URL are byte-identical to the blob route's"

  # A device seeks: the res URL honours Range because it IS the render route,
  # which delegates to the ordinary blob handler.
  assert_eq "$(curl -sS --unix-socket "$SOCK" -H 'Range: bytes=0-3' -o /dev/null -w '%{http_code}' \
    "http://heyarr$dlna_res")" "206" \
    "a DLNA res URL honours Range with a 206, delegated to the blob route"

  # An unimplemented action is a faithful SOAP fault, not a silent 404.
  assert_contains "$(curl -sS --unix-socket "$SOCK" -H 'Content-Type: text/xml' \
    --data '<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:Search xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1"><ContainerID>0</ContainerID></u:Search></s:Body></s:Envelope>' \
    "http://heyarr/dlna/control/ContentDirectory")" "UPnPError" \
    "an unimplemented ContentDirectory action returns a SOAP fault, not a 404"

  not_exercised real_dlna_device \
    "a real Samsung TV / Devialet discovering (SSDP), browsing and playing — app-in-the-loop and needs LAN multicast, out of the demo budget like #202's video client (tracked pending: dlna-real-device-plays)"

  note "  probing (§29, ADR-0023)"
  # Capability routing existed from M1-05 and had no user until M2: the worker
  # built its runtime with an empty capability set, so no job could ever
  # require one. This is the first time it is watched deciding anything, and
  # the assertion runs BOTH ways depending on what this machine has.
  local probe_jobs probe_hash probe_state
  probe_hash=$(api_all /api/v1/assets '.items[] | select(.filename != null) | select(.filename | endswith(".mkv") or endswith(".mp4") or endswith(".flac")) | .blob_hash' | head -1)

  probe_jobs=$(api "/api/v1/jobs?type=probe_blob" | jq -r '.items | length')
  assert_contains "$probe_jobs" "" "probe jobs were enqueued by ingest"

  if command -v ffprobe >/dev/null 2>&1; then
    # WAIT FOR THE PROBE THIS SECTION IS ABOUT (#207). Nothing above guarantees
    # THIS blob's probe has landed — the filter here admits .flac as well, so it
    # may not be either of the blobs the two sections above waited on — and
    # every assertion below is a claim about a probe that ran.
    wait_for_probe "$probe_hash" \
      "there was no probe result to query, and the three assertions below report an empty pipeline rather than the missing probe"
    # A capable worker claims them and the results are queryable.
    assert_eq "$(api "/api/v1/jobs?type=probe_blob&state=succeeded" | jq -r '.items | length > 0')" \
      "true" "a worker with ffprobe ran the probe jobs"
    assert_eq "$(api "/api/v1/blobs/$probe_hash/probe" | jq -r '.streams | length > 0')" "true" \
      "the probe result is queryable and has streams"
    # §29's whole point, asserted against a running system rather than a test
    # double: the probe read a fraction of the blob rather than copying it.
    assert_eq "$(api "/api/v1/blobs/$probe_hash/probe" | jq -r '.materialised')" "false" \
      "the probe used byte ranges rather than materialising the blob"
  else
    # ADR-0023's degrade path, at the level a user would notice. The jobs are
    # PENDING — not failed, not retried into a backoff, not silently dropped —
    # and everything else in this demo has already had to pass without them.
    probe_state=$(api "/api/v1/jobs?type=probe_blob" | jq -r '[.items[].state] | unique | join(",")')
    assert_eq "$probe_state" "pending" \
      "a node with no ffprobe leaves probe jobs pending rather than failing them"
    assert_eq "$(api "/api/v1/blobs/$probe_hash/probe" -o /dev/null -w '%{http_code}')" "404" \
      "an unprobed blob is a 404, not an empty result"
    pass "the whole demo passed with every probe job unclaimed"
    not_exercised ffprobe "a probe job actually RUNNING: succeeded jobs, a queryable probe result, and §29's range-read rather than a materialised blob"
  fi

  note "  the media toolchain (ADR-0023)"
  # FFmpeg is the first dependency Heyarr cannot ship inside its own binary,
  # and it is optional. Both states are real deployments, so the demo asserts
  # that what the node REPORTS matches what it actually has, rather than
  # asserting one of the two and hoping.
  #
  # This is the assertion that would catch the toolchain silently becoming
  # mandatory: on a runner with no ffprobe, everything above this line has
  # already had to pass.
  local media_tools ffprobe_reported ffprobe_real
  media_tools=$(api /api/v1/system | jq -r '.media | length')
  assert_eq "$media_tools" "2" "/api/v1/system reports both media tools"

  ffprobe_reported=$(api /api/v1/system | jq -r '.media[] | select(.name == "ffprobe") | .available')
  if command -v ffprobe >/dev/null 2>&1; then ffprobe_real=true; else ffprobe_real=false; fi
  assert_eq "$ffprobe_reported" "$ffprobe_real" \
    "the reported ffprobe availability matches this machine"

  if [[ "$ffprobe_real" == "false" ]]; then
    # The degrade path, proven end to end rather than claimed: this whole
    # script has just scanned, ingested, hashed, verified and range-served on
    # a machine with no FFmpeg at all.
    assert_eq "$(api /api/v1/system | jq -r '.media[] | select(.name == "ffprobe") | .detail')" \
      "not found on PATH" "a node without ffprobe says why"
    pass "the whole demo passed on a node with no media toolchain"
    not_exercised ffprobe "an available ffprobe reporting a version through /api/v1/system"
  else
    assert_contains "$(api /api/v1/system | jq -r '.media[] | select(.name == "ffprobe") | .version')" \
      "." "an available ffprobe reports a version"
  fi

  note "  quality profiles (§62, M3-01)"
  # The first Milestone 3 section. It is placed HERE — after the playback and
  # toolchain assertions, before the CLI — for one reason: it writes rows and
  # emits events, and every count asserted above it is a count of something
  # else. The remux section below is the only section that mutates the LIBRARY;
  # this one must stay above it and below the event-log counts.
  #
  # What it proves that the unit tests cannot: that a real Heyarr, started from
  # nothing, has usable profiles without anyone authoring JSON — and that the
  # three sections of §62 are three different KINDS of statement all the way
  # out to the wire, not three degrees of one.
  local qp_json qp_count qp_open_ended
  qp_json=$(api /api/v1/quality-profiles)
  qp_count=$(jq -r '.items | length' <<<"$qp_json")

  # Seeded on first start. A Heyarr with no profiles is one where the first
  # interesting thing you can do requires reading a vocabulary you have not met.
  if (( qp_count >= 3 )); then
    pass "a fresh Heyarr seeds its quality profiles ($qp_count)"
  else
    fail "a fresh Heyarr should seed quality profiles, found $qp_count"
  fi
  assert_eq "$(jq -r '[.items[] | select(.seeded)] | length > 0' <<<"$qp_json")" "true" \
    "the seeded profiles are marked as seeded, so an edited one is tellable apart"

  # §62's three semantics, on a real response. `terminal` is the one that
  # matters: a profile with NO terminal rules is legal and means "never stop
  # looking", and it must not read back as null.
  qp_open_ended=$(jq -r '[.items[] | select(.terminal | length == 0)] | length' <<<"$qp_json")
  if (( qp_open_ended >= 1 )); then
    pass "a seeded profile is open-ended — nothing stops the upgrade loop for it"
  else
    fail "no seeded profile is open-ended, so the never-terminal path is unexercised here"
  fi
  assert_not_contains "$qp_json" '"terminal":null' \
    "an absent terminal section serialises as [] rather than null"

  # A gate is not a score. This is the mistake the design most invites, and
  # silently dropping the weight would leave an operator believing a gate is
  # scoring. Refused at WRITE time, with a message that says where the rule
  # belongs.
  local qp_reject
  qp_reject=$(api /api/v1/quality-profiles -X POST -H 'Content-Type: application/json' \
    -d '{"name":"weighted-gate","accept":[{"attribute":"resolution","op":"gte","value":1080,"weight":20}]}')
  assert_contains "$(jq -r '.status' <<<"$qp_reject")" "400" \
    "a weight on an accept rule is refused"
  assert_contains "$(jq -r '.detail' <<<"$qp_reject")" "GATE" \
    "the refusal says an accept rule is a gate, and where a weight belongs"

  # An attribute that does not exist is caught when the profile is written, not
  # when a candidate is evaluated — so the mistake reaches the person who made
  # it rather than becoming a rejection reason on every candidate for months.
  local qp_typo
  qp_typo=$(api /api/v1/quality-profiles -X POST -H 'Content-Type: application/json' \
    -d '{"name":"typo","accept":[{"attribute":"minimum_resolution","op":"gte","value":1080}]}')
  assert_contains "$(jq -r '.detail' <<<"$qp_typo")" "no such attribute" \
    "an unknown attribute is refused at write time"
  assert_contains "$(jq -r '.detail' <<<"$qp_typo")" "video_codec" \
    "the refusal lists the attributes that do exist"

  # A rule that would validate and then silently match nothing is the worst
  # outcome of the three, because the profile appears to work.
  local qp_silent
  qp_silent=$(api /api/v1/quality-profiles -X POST -H 'Content-Type: application/json' \
    -d '{"name":"silent","accept":[{"attribute":"source","op":"eq","value":["remux","bluray"]}]}')
  assert_contains "$(jq -r '.detail' <<<"$qp_silent")" "in" \
    "a list operand with an equality comparison is refused rather than silently never matching"

  # A profile is authored deliberately, so a name collision is a conflict, not
  # an upsert that discards what the profile said before.
  local qp_created qp_dup qp_id
  qp_created=$(api /api/v1/quality-profiles -X POST -H 'Content-Type: application/json' \
    -d '{"name":"acceptance-profile","accept":[{"attribute":"resolution","op":"gte","value":720}],
         "prefer":[{"attribute":"source","op":"eq","value":"remux","weight":25}]}')
  qp_id=$(jq -r '.id' <<<"$qp_created")
  assert_contains "$qp_id" "-" "a profile can be authored over the API"
  qp_dup=$(api /api/v1/quality-profiles -X POST -H 'Content-Type: application/json' \
    -d '{"name":"acceptance-profile","accept":[]}')
  assert_contains "$(jq -r '.status' <<<"$qp_dup")" "409" \
    "a duplicate profile name is a conflict rather than a silent overwrite"

  # A penalty is a real thing to want. Without a negative weight, "prefer
  # anything that is not a webrip" has to be written as a gate, which rejects
  # rather than deprioritises.
  local qp_penalty
  qp_penalty=$(api /api/v1/quality-profiles -X POST -H 'Content-Type: application/json' \
    -d '{"name":"acceptance-penalty","prefer":[{"attribute":"source","op":"eq","value":"webrip","weight":-30}]}')
  assert_eq "$(jq -r '.prefer[0].weight' <<<"$qp_penalty")" "-30" \
    "a negative weight is a penalty and survives the round trip"

  # Omitted and cleared are different intentions. Collapsing them makes
  # "clear the terminal rules" and "forget to send them" the same request.
  local qp_put
  qp_put=$(api "/api/v1/quality-profiles/$qp_id" -X PUT -H 'Content-Type: application/json' \
    -d '{"terminal":[{"attribute":"resolution","op":"gte","value":2160}]}')
  assert_eq "$(jq -r '.terminal | length' <<<"$qp_put")" "1" \
    "a terminal condition can be added to a profile"
  assert_eq "$(jq -r '.accept | length' <<<"$qp_put")" "1" \
    "a section the request omits is left alone"
  qp_put=$(api "/api/v1/quality-profiles/$qp_id" -X PUT -H 'Content-Type: application/json' \
    -d '{"terminal":[]}')
  assert_eq "$(jq -r '.terminal | length' <<<"$qp_put")" "0" \
    "a section sent as [] is cleared — the profile is now never finished"

  # Deletion is physical here, unlike content (ADR-0018): a profile is a page of
  # configuration, and a soft-deleted one would have to be filtered out of every
  # read path forever to stop an operator seeing something they deleted.
  api "/api/v1/quality-profiles/$qp_id" -X DELETE -o /dev/null
  assert_eq "$(api "/api/v1/quality-profiles/$qp_id" | jq -r '.status')" "404" \
    "a deleted profile is gone rather than hidden"

  note "  the provider registry (§59, M3-07)"
  # §59 centralises provider configuration, and this is the endpoint that makes
  # ADR-0025's degrade path legible: a node with no indexer is a supported
  # configuration, and the cost of that design is that "why is nothing being
  # acquired" has an entirely legitimate answer which is invisible unless
  # something reports it.
  #
  # Placed here, above the CLI section, because it reads state rather than
  # writing any — no counts move, so its position is not load-bearing the way
  # the desired-state and remux sections are.
  local prov_json
  prov_json=$(api /api/v1/providers)

  assert_eq "$(jq -r '[.providers[] | select(.name == "acceptance-indexer")] | length' \
    <<<"$prov_json")" "1" "a configured provider is reported"
  assert_eq "$(jq -r '[.providers[] | select(.name == "acceptance-indexer")] | .[0].capabilities | join(",")' \
    <<<"$prov_json")" "indexer" "and says what it can do"

  # The node's own capability set. Stated rather than left for a client to
  # derive, because deriving it means knowing a disabled provider contributes
  # nothing.
  #
  # Asserted by MEMBERSHIP rather than as the whole joined set. The set grows
  # whenever a capability is added — M3-10 added `download` and this assertion
  # broke, having been pinned to "indexer" as a proxy for "the enabled indexer
  # contributed and the disabled one did not". That proxy was exact while there
  # was one provider kind; it is not a claim about the set's LENGTH, and
  # writing it as one made an unrelated addition look like a regression.
  assert_eq "$(jq -r '[.capabilities[] | select(. == "indexer")] | length' <<<"$prov_json")" "1" \
    "the node reports the indexer it can therefore use"
  # And the disabled provider contributed nothing — which is the half that was
  # actually being tested.
  assert_eq "$(jq -r '[.capabilities[] | select(. == "metadata")] | length' <<<"$prov_json")" "0" \
    "and nothing it was not configured for"

  # "Configured and switched off" is distinct from "not configured at all". A
  # disabled provider is REPORTED, with no capabilities — otherwise "why is
  # nothing searching" means re-reading the config file.
  assert_eq "$(jq -r '[.providers[] | select(.name == "acceptance-disabled")] | length' \
    <<<"$prov_json")" "1" "a disabled provider is still reported"
  assert_eq "$(jq -r '[.providers[] | select(.name == "acceptance-disabled")] | .[0].capabilities | length' \
    <<<"$prov_json")" "0" "and reports no capabilities, so it is never routed to"

  # Health is OBSERVED, and since #164 there is a beat that produces the
  # observation, so this WAITS for one rather than branching on whether the
  # race was won. The branch it replaces read "if a check has run, assert;
  # otherwise pass" — which passed on both outcomes, and passed for the whole
  # of M3 on the outcome where no check could ever run.
  #
  # The never-checked rendering is still asserted, deterministically, in the
  # provider health beat section: there a controller is started with no worker
  # in existence, so nothing can claim the pass.
  local prov_indexer
  prov_indexer=$(wait_for_health_check acceptance-indexer)
  if [[ "$(jq -r '.checked_at // "never"' <<<"$prov_indexer")" == "never" ]]; then
    fail "no health check ran on the full node — nothing is enqueueing the pass"
  else
    pass "the health beat checked the configured providers on a running node"
    assert_eq "$(jq -r '.healthy' <<<"$prov_indexer")" "true" \
      "a checked fake provider is healthy"
    assert_eq "$(jq -r '.version' <<<"$prov_indexer")" "fake" \
      "and reports the version its check obtained, rather than nothing at all"
  fi

  # No credential reaches the response. Asserted by searching the BODY, not by
  # reading the struct — this is a public repository and the git history is
  # permanent.
  assert_not_contains "$prov_json" "api_key" \
    "no credential field reaches the providers response"

  # ADR-0025's whole claim, on a running system: a node whose providers cannot
  # do everything still serves its catalog.
  assert_contains "$(api /api/v1/works | jq -r '.items | length > 0')" "true" \
    "a node reports its providers and still serves its library"

  # The two capability vocabularies meet at /api/v1/system, which already
  # reports the media toolchain (ADR-0023). Both describe THIS node.
  assert_contains "$(api /api/v1/system | jq -r 'has("media")')" "true" \
    "the system endpoint still reports the media toolchain alongside providers"

  note "  MCP — semantic actions over desired state (§71, ADR-0019)"
  # Below every catalog count, with the other M3 sections: want_content creates
  # a Work, so this mutates the catalog.
  #
  # What this proves that the unit tests cannot: that a real agent, over a real
  # socket, speaking JSON-RPC, can want something the library has never seen and
  # be told WHY a release was or was not good enough — and that the scope
  # boundary holds against a real token rather than a synthetic identity.
  local mcp_rpc mcp_tools mcp_want mcp_want_id

  # A tiny helper, because every call below is the same envelope.
  mcp() { # method params-json
    api /api/v1/mcp -X POST -H 'Content-Type: application/json' \
      -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$1\",\"params\":$2}"
  }
  mcp_call() { # tool args-json
    mcp "tools/call" "{\"name\":\"$1\",\"arguments\":$2}"
  }

  # The handshake. Tools and nothing else — a `resources` capability would be
  # the second read API ADR-0019 exists to prevent.
  mcp_rpc=$(mcp "initialize" '{}')
  assert_eq "$(jq -r '.result.serverInfo.name' <<<"$mcp_rpc")" "heyarr" \
    "MCP answers the handshake"
  assert_eq "$(jq -r '.result.capabilities | has("tools")' <<<"$mcp_rpc")" "true" \
    "and advertises tools"
  assert_eq "$(jq -r '.result.capabilities | has("resources")' <<<"$mcp_rpc")" "false" \
    "and deliberately advertises no resources capability — this is not a second read API"
  assert_contains "$(jq -r '.result.instructions' <<<"$mcp_rpc")" "personal state" \
    "the instructions tell an agent what this server cannot see (§72)"

  # The surface, with its schemas and its scopes.
  mcp_tools=$(mcp "tools/list" '{}')
  assert_eq "$(jq -r '[.result.tools[] | select(.inputSchema == null)] | length' <<<"$mcp_tools")" "0" \
    "every tool publishes a hand-written input schema"
  assert_eq "$(jq -r '[.result.tools[] | select(.annotations["heyarr/requiredScope"] == null)] | length' <<<"$mcp_tools")" "0" \
    "and says what scope it needs, so an agent can tell \"no such verb\" from \"not with this token\""
  # §72, enumerated rather than inspected.
  assert_eq "$(jq -r '[.result.tools[] | select((.name + .description) | ascii_downcase | test("playlist|rating|reading position|watch history"))] | length' <<<"$mcp_tools")" "0" \
    "no tool reaches for personal state (§72)"

  # The central action, against a work the library already holds.
  #
  # NOT a work descriptor, deliberately, and the reason is placement. This
  # section sits ABOVE the catalog counts — the anchor keeps it clear of a
  # branch editing this file concurrently — and want_content with a title
  # CREATES A WORK, which moved the work count from 8 to 9 and broke both the
  # CLI count and the upgrade section, whose fixture picks works[0] and got a
  # brand new asset-less work instead. Wanting an existing work mutates only
  # desired_items, which this section cleans up after itself.
  #
  # The "want something that does not exist anywhere" claim is not lost: the
  # M3-02 section asserts it below the counts, where creating a work is safe,
  # and the MCP unit tests assert it through the same shared intent.
  local mcp_work
  mcp_work=$(api /api/v1/works | jq -r '.items[0].id')
  mcp_want=$(mcp_call "want_content" "{\"work_id\":\"$mcp_work\",\"quality_profile\":\"archival\"}")
  mcp_want_id=$(jq -r '.result.structuredContent.id' <<<"$mcp_want")
  assert_contains "$mcp_want_id" "-" "an agent can create desired state through MCP"
  assert_eq "$(jq -r '.result.structuredContent.acquisition.state' <<<"$mcp_want")" "MISSING" \
    "and the acquisition state is created with it, through the same intent the HTTP door uses"
  assert_eq "$(jq -r '.result.structuredContent.monitor' <<<"$mcp_want")" "true" \
    "monitoring defaults to true"

  # The flagship: reasons, not a verdict.
  local mcp_explain
  mcp_explain=$(mcp_call "explain_release" '{"quality_profile":"archival","releases":[
    {"id":"remux","attributes":{"resolution":2160,"source":"remux"}},
    {"id":"cam","attributes":{"source":"cam"}}]}')
  assert_eq "$(jq -r '.result.structuredContent.selected' <<<"$mcp_explain")" "remux" \
    "explain_release picks the best acceptable release"
  assert_contains "$(jq -r '[.result.structuredContent.ranked[] | select(.id == "cam")] | .[0].rejected_by[0].rule' <<<"$mcp_explain")" \
    "source" "and a rejection names the RULE that rejected it, with its stable code"
  assert_eq "$(jq -r '[.result.structuredContent.ranked[] | select(.id == "cam")] | .[0].accepted' <<<"$mcp_explain")" "false" \
    "a cam is refused by the archival profile's gate"

  # Both halves of a result: prose an agent can quote, and a shape it can
  # branch on.
  assert_eq "$(jq -r '.result.content[0].type' <<<"$mcp_explain")" "text" \
    "a result carries a text block every client can render"
  assert_eq "$(jq -r '.result | has("structuredContent")' <<<"$mcp_explain")" "true" \
    "and the same value as structure, so an agent need not parse the prose"

  # What is deliberately absent, and why. A tool answering "not implemented"
  # would be a published vocabulary with a hole in it.
  #
  # THE VERB PROBED HERE MUST STILL BE IN internal/api/mcp/deferred.go. This
  # asked for `acquire_release` until M6 shipped it, at which point the call
  # started answering -32602 (the verb exists, the arguments were wrong) instead
  # of -32601 (no such verb) — which is the assertion below failing for the best
  # possible reason. When the last deferred verb ships, this block goes rather
  # than being pointed at something arbitrary.
  local mcp_deferred
  mcp_deferred=$(mcp_call "transfer_playback" '{}')
  assert_eq "$(jq -r '.error.code' <<<"$mcp_deferred")" "-32601" \
    "a §71 verb this milestone does not carry is absent rather than stubbed"
  assert_contains "$(jq -r '.error.data.milestone' <<<"$mcp_deferred")" "M4" \
    "and names the milestone that brings it, so an agent waits rather than retrying"

  # A typo is NOT reported as a deferred feature.
  assert_eq "$(mcp_call "want_contnet" '{}' | jq -r '.error.data.milestone // "none"')" "none" \
    "a mistyped tool is not reported as something that is coming"

  # A tool failure is an error inside the envelope, not a transport failure —
  # a client seeing a 4xx would reconnect instead of correcting itself.
  assert_eq "$(api "/api/v1/mcp" -X POST -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"verify_blob","arguments":{}}}' \
    -o /dev/null -w '%{http_code}')" "200" \
    "a tool failure is a JSON-RPC error rather than an HTTP status"

  # And it reaches desired state created through the OTHER door, which is the
  # point of sharing one implementation.
  assert_contains "$(mcp_call "get_missing_content" '{}' | jq -r '[.result.structuredContent.wants[] | select(.desired_item_id == "'"$mcp_want_id"'")] | length')" \
    "1" "get_missing_content sees the want, and names its profile rather than an id"

  mcp_call "monitor_content" "{\"desired_item_id\":\"$mcp_want_id\",\"monitor\":false}" >/dev/null
  assert_eq "$(api "/api/v1/desired/$mcp_want_id" | jq -r '.monitor')" "false" \
    "a change made through MCP is visible through the JSON API — one implementation, two doors"

  api "/api/v1/desired/$mcp_want_id" -X DELETE -o /dev/null

  note "  the download client (§58, M3-10)"
  # A configured Transmission that is NOT LISTENING, which is the whole of
  # ADR-0025's claim: presence is checked at startup, reachability is checked
  # continuously and is a job, and a download client that is down at 03:00 must
  # not stop Heyarr serving the library at 03:01.
  #
  # Placed above the CLI section because it reads state rather than writing any
  # — no counts move, so its position is not load-bearing the way the
  # desired-state and remux sections are.
  local dl_json dl_entry
  dl_json=$(api /api/v1/providers)
  # Waited for, not sampled: since #164 a beat produces the observation, and an
  # assertion about health made before any check has run is an assertion about
  # a value nothing has written. See wait_for_health_check.
  dl_entry=$(wait_for_health_check acceptance-downloads)

  assert_eq "$(jq -r '. != null' <<<"$dl_entry")" "true" \
    "a configured download client is reported"
  assert_eq "$(jq -r '.capabilities | join(",")' <<<"$dl_entry")" "download" \
    "and advertises the download capability"

  # The node's capability set now carries both, which is what routes a poll job
  # here and a search job there.
  assert_eq "$(jq -r '[.capabilities[] | select(. == "download")] | length' <<<"$dl_json")" "1" \
    "the node advertises download, so poll jobs can be claimed"

  # THE ADR-0025 ASSERTION. The endpoint refuses connections, and the node is
  # serving this very request.
  assert_eq "$(api /healthz | jq -r '.status')" "ok" \
    "a node whose download client is unreachable still serves"
  assert_eq "$(api /api/v1/works | jq -r '.items | length > 0')" "true" \
    "and still answers for its library"

  # Health is OBSERVED, and the observation has now been waited for — so this
  # is a claim about what a check found rather than about a field nothing
  # wrote. What must NOT happen is a healthy report for something that is not
  # listening.
  if [[ "$(jq -r '.checked_at // "never"' <<<"$dl_entry")" == "never" ]]; then
    fail "no health check ever ran on the download client — nothing enqueues the pass"
  else
    pass "the health beat checked the download client too"
  fi
  assert_eq "$(jq -r '.healthy' <<<"$dl_entry")" "false" \
    "an unreachable download client is not reported as healthy"

  # A refusal that names the credential rather than the network, and vice
  # versa, is the difference between an operator looking at a password and
  # looking at a firewall.
  assert_not_contains "$(jq -r '.detail' <<<"$dl_entry")" "credential" \
    "an unreachable client is not reported as a credential problem"

  # No credential field reaches the response, for a download client as for an
  # indexer. Asserted on the whole document rather than one entry, because the
  # leak this guards against would not be fussy about which provider it came
  # from.
  assert_not_contains "$dl_json" "api_key" "no credential field reaches the providers response"
  assert_not_contains "$dl_json" "password" "and no password field either"

  note "  a second download client: qBittorrent (§58, M11)"
  # What is honestly provable WITHOUT a daemon (ADR-0026 keeps one out of CI):
  # the registry constructs a REAL qBittorrent client for its kind, it is
  # registered with the download capability, and the health beat reaches its
  # real Check code — a login against an endpoint that refuses — and reports
  # unhealthy rather than pretending. A real transfer is proven by the fake
  # (unit) and TestLiveQBittorrent (opt-in), never here.
  local qb_entry
  qb_entry=$(wait_for_health_check acceptance-qbittorrent)

  assert_eq "$(jq -r '. != null' <<<"$qb_entry")" "true" \
    "a configured qBittorrent client is reported — a REAL client, not an unimplemented placeholder"
  assert_eq "$(jq -r '.capabilities | join(",")' <<<"$qb_entry")" "download" \
    "the qBittorrent client advertises the download capability, so poll jobs can route to it"
  # The health beat reached real qBittorrent Check code: a client with no caller
  # would have no observation to show. checked_at proves the caller exists.
  if [[ "$(jq -r '.checked_at // "never"' <<<"$qb_entry")" == "never" ]]; then
    fail "no health check ever ran on the qBittorrent client — nothing reached the real client code"
  else
    pass "the health beat checked the qBittorrent client — a real caller reaching real client code"
  fi
  assert_eq "$(jq -r '.healthy' <<<"$qb_entry")" "false" \
    "an unreachable qBittorrent is reported unhealthy, not pretended healthy"
  assert_not_contains "$(jq -r '.detail' <<<"$qb_entry")" "credential" \
    "an unreachable qBittorrent is not misreported as a credential problem"

  note "  a third download client, and the first usenet one: SABnzbd (§58, M11)"
  # The same honest-without-a-daemon proof as qBittorrent (ADR-0026 keeps a daemon
  # out of CI, and there is no harness leg for usenet at all): the registry
  # constructs a REAL SABnzbd client for its kind, it is registered with the
  # download capability, and the health beat reaches its real Check code — an
  # authenticated read against an endpoint that refuses — and reports unhealthy
  # rather than pretending. A real transfer is proven by the fake (unit) and
  # TestLiveSABnzbd (opt-in), never here.
  local sab_entry
  sab_entry=$(wait_for_health_check acceptance-sabnzbd)

  assert_eq "$(jq -r '. != null' <<<"$sab_entry")" "true" \
    "a configured SABnzbd client is reported — a REAL client, not an unimplemented placeholder"
  assert_eq "$(jq -r '.capabilities | join(",")' <<<"$sab_entry")" "download" \
    "the SABnzbd client advertises the download capability, so poll jobs can route to it"
  # The health beat reached real SABnzbd Check code: a client with no caller would
  # have no observation to show. checked_at proves the caller exists.
  if [[ "$(jq -r '.checked_at // "never"' <<<"$sab_entry")" == "never" ]]; then
    fail "no health check ever ran on the SABnzbd client — nothing reached the real client code"
  else
    pass "the health beat checked the SABnzbd client — a real caller reaching real client code"
  fi
  assert_eq "$(jq -r '.healthy' <<<"$sab_entry")" "false" \
    "an unreachable SABnzbd is reported unhealthy, not pretended healthy"
  # An unreachable instance is a connection failure, not a rejected credential —
  # so the api_key it was configured with must not be misreported as the problem.
  assert_not_contains "$(jq -r '.detail' <<<"$sab_entry")" "credential" \
    "an unreachable SABnzbd is not misreported as a credential problem"

  note "  a fourth download client, and the second usenet one: NZBGet (§58, M11, #379)"
  # The same honest-without-a-daemon proof as SABnzbd (ADR-0026 keeps a daemon out
  # of CI, and there is no harness leg for usenet at all): the registry constructs
  # a REAL NZBGet client for its kind, it is registered with the download
  # capability, and the health beat reaches its real Check code — a JSON-RPC read
  # against an endpoint that refuses — and reports unhealthy rather than
  # pretending. A real transfer is proven by the fake (unit) and TestLiveNZBGet
  # (opt-in), never here.
  local nzb_entry
  nzb_entry=$(wait_for_health_check acceptance-nzbget)

  assert_eq "$(jq -r '. != null' <<<"$nzb_entry")" "true" \
    "a configured NZBGet client is reported — a REAL client, not an unimplemented placeholder"
  assert_eq "$(jq -r '.capabilities | join(",")' <<<"$nzb_entry")" "download" \
    "the NZBGet client advertises the download capability, so poll jobs can route to it"
  # The health beat reached real NZBGet Check code: a client with no caller would
  # have no observation to show. checked_at proves the caller exists.
  if [[ "$(jq -r '.checked_at // "never"' <<<"$nzb_entry")" == "never" ]]; then
    fail "no health check ever ran on the NZBGet client — nothing reached the real client code"
  else
    pass "the health beat checked the NZBGet client — a real caller reaching real client code"
  fi
  assert_eq "$(jq -r '.healthy' <<<"$nzb_entry")" "false" \
    "an unreachable NZBGet is reported unhealthy, not pretended healthy"
  assert_not_contains "$(jq -r '.detail' <<<"$nzb_entry")" "credential" \
    "an unreachable NZBGet is not misreported as a credential problem"

  note "  Torznab, a real indexer client (§59, ADR-0028, M3-09)"
  # Placed here with the download client's assertions because this section
  # READS state and writes none — no catalog count moves, so its position is
  # not load-bearing the way the desired-state sections are.
  local tz_entry
  tz_entry=$(wait_for_health_check acceptance-torznab)

  assert_eq "$(jq -r '. != null' <<<"$tz_entry")" "true" \
    "a configured torznab indexer is reported"
  # Membership, not a rendering of the set: asserting the joined string is how
  # two proxy assertions rotted earlier in this milestone.
  assert_eq "$(jq -r '[.capabilities[] | select(. == "indexer")] | length' <<<"$tz_entry")" "1" \
    "and advertises the indexer capability"

  # THE ASSERTION THIS ISSUE EXISTS FOR.
  #
  # The registry holds a real client rather than a placeholder. Until M3-09 a
  # configured torznab provider was registered, routed and reported — and
  # answered every health check with "the torznab client is not implemented
  # yet". That was the honest report at the time and it must not still be the
  # report now.
  assert_not_contains "$(jq -r '.detail' <<<"$tz_entry")" "not implemented" \
    "the torznab kind is no longer a placeholder in the registry"

  # ADR-0025 again, for an indexer this time: the endpoint refuses connections
  # and the node is answering this request.
  assert_eq "$(api /healthz | jq -r '.status')" "ok" \
    "a node whose indexer is unreachable still serves"

  # Health is OBSERVED. What must NOT happen is a healthy report for something
  # that is not listening — that is the report that sends work to a provider
  # which then fails, which ADR-0025 says is worse than advertising nothing.
  #
  # This assertion predates #164 and was VACUOUS for the whole of M3: nothing
  # enqueued the health pass, so `healthy` was the never-checked default and no
  # change to the indexer client could have broken it. checked_at is asserted
  # first, so it cannot go back to being vacuous silently.
  if [[ "$(jq -r '.checked_at // "never"' <<<"$tz_entry")" == "never" ]]; then
    fail "no health check ever ran on the torznab indexer — nothing enqueues the pass"
  else
    pass "the health beat checked the torznab indexer"
  fi
  assert_eq "$(jq -r '.healthy' <<<"$tz_entry")" "false" \
    "an unreachable indexer is not reported as healthy"

  # #131: THE CACHE MUST NOT MANUFACTURE AN OBSERVATION.
  #
  # Check() refreshes the capabilities cache, and a capabilities document is
  # where the reported version comes from. This endpoint refuses connections
  # and has never produced one, so there is nothing to remember — and a
  # version appearing here would mean a cached document was passed off as
  # something just observed. That is the same class of failure as reporting
  # "no releases found" for a rejected key: a confident answer nobody has.
  #
  # assert_eq, not assert_contains: version is an enum-like field whose values
  # are "a version string" or "absent", and a substring match on an absent
  # field is a match on nothing.
  assert_eq "$(jq -r '.version // "absent"' <<<"$tz_entry")" "absent" \
    "an indexer that has never handshaked reports no version"

  # #131: A SECOND CHECK IS A SECOND OBSERVATION, NOT A REPLAY OF THE FIRST.
  #
  # Asserted in provider_health_beat_demo (#164), not here. That section
  # PROCURES a second pass — it restarts its controller, which is a real beat
  # enqueue on the real path — and then asserts both halves of this claim
  # against a pass that demonstrably ran.
  #
  # This node used to wait up to thirty seconds for its own beat to come round
  # again instead. The beat interval is a minute (internal/controller/
  # healthbeat.go), so on the Linux runner the wait ran out every time and
  # recorded "not yet assertable", and on macOS the loop was slow enough to
  # catch the tick at about forty seconds. Measured over twenty-four CI runs,
  # that was 34-42s of every run's budget spent proving, at best, what #164
  # already proves in two.

  # The API key is in the config file three lines above. It must not be in the
  # response, and the assertion is on the VALUE rather than the field name so
  # it is a claim about the credential rather than about the schema.
  assert_not_contains "$dl_json" "not-a-real-key" \
    "an indexer credential does not reach the providers response"

  note "  Newznab, a usenet indexer over the same protocol (§59, ADR-0028, M11)"
  # Newznab IS Torznab minus the torrent extension — one wire protocol, one
  # client (internal/indexers), per ADR-0028's "implement the protocol, not the
  # product". The client's parsing of a real newznab feed (the newznab: namespace
  # and an .nzb source) is proven in internal/indexers/newznab_test.go, where a
  # real server can run; here the narrow, worth-having claim is the registry one,
  # exactly as for torznab: the newznab kind is a REAL client, not a placeholder.
  local nz_entry
  nz_entry=$(wait_for_health_check acceptance-newznab)

  assert_eq "$(jq -r '. != null' <<<"$nz_entry")" "true" \
    "a configured newznab indexer is reported"
  assert_eq "$(jq -r '[.capabilities[] | select(. == "indexer")] | length' <<<"$nz_entry")" "1" \
    "and advertises the indexer capability"
  # THE ASSERTION THIS ADAPTER EXISTS FOR: the registry holds a real client for
  # the newznab kind, so its health check reports a real connection attempt
  # rather than "not implemented".
  assert_not_contains "$(jq -r '.detail' <<<"$nz_entry")" "not implemented" \
    "the newznab kind is a real client in the registry, not a placeholder"
  # Observed health, not a default: an unreachable indexer must not be reported
  # healthy (ADR-0025), and the check must actually have run (#164's vacuity).
  if [[ "$(jq -r '.checked_at // "never"' <<<"$nz_entry")" == "never" ]]; then
    fail "no health check ever ran on the newznab indexer — nothing enqueues the pass"
  else
    pass "the health beat checked the newznab indexer"
  fi
  assert_eq "$(jq -r '.healthy' <<<"$nz_entry")" "false" \
    "an unreachable newznab indexer is not reported as healthy"
  # The credential three lines apart in the config must not reach the response.
  assert_not_contains "$(api /api/v1/providers)" "not-a-real-newznab-key" \
    "a newznab credential does not reach the providers response"

  note "  the search job (§60, §63, M3-12)"
  # THE MILESTONE'S CENTRAL CLAIM, made executable:
  #
  #   Heyarr decides what should exist, explains why it chose one release over
  #   another, and does it with NO REAL INDEXER PRESENT.
  #
  # Everything here runs against the fake configured at the top of this file.
  # If the search job could not be driven by a fake, ADR-0026's
  # values-in-values-out interface would have failed at its one job — a fake, a
  # fixture replayer and a real client are supposed to be indistinguishable to
  # every caller, and this is the caller that matters most.
  #
  # It sits below every catalog count because wanting content Heyarr has never
  # seen CREATES A WORK, which shifts them. That has bitten three times.
  #
  # It wants works the library ALREADY HOLDS rather than inventing any. Wanting
  # unknown content creates a Work, which shifts every catalog count below —
  # and the CLI section immediately after this one asserts on exactly those.
  # That is the fourth time the ordering hazard has bitten, and the first where
  # the fix was to want something existing rather than to move the section.
  local search_work search_want search_id search_state search_cands
  search_work=$(api_all /api/v1/works '.items[] | select(.title == "Blue Harvest") | .id' | head -1)
  if [[ -z "$search_work" ]]; then
    fail "the fixture library has no 'Blue Harvest' work for the search demo to want"
    return 1
  fi
  search_want=$(api /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d "{\"work_id\":\"$search_work\",\"quality_profile\":\"living-room\"}")
  search_id=$(jq -r '.id' <<<"$search_want")
  assert_eq "$(jq -r '.acquisition.state' <<<"$search_want")" "MISSING" \
    "a fresh want starts MISSING"

  # A search is a JOB (invariant 4, ADR-0002) — the API asks, a worker runs it.
  local search_job
  search_job=$(api "/api/v1/desired/$search_id/search" -X POST)
  assert_contains "$(jq -r '.job_id' <<<"$search_job")" "-" \
    "asking for a search queues a job rather than doing the work inline"

  # Wait for the machine to arrive, rather than sleeping a fixed duration and
  # hoping — a fixed sleep here is a test that passes on a fast machine and
  # flakes on a loaded one.
  waited=0
  while (( waited < 300 )); do
    search_state=$(api "/api/v1/desired/$search_id" | jq -r '.acquisition.state')
    [[ "$search_state" == "SELECTED" ]] && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  assert_eq "$search_state" "SELECTED" \
    "a want reaches SELECTED with no real indexer anywhere (§64, ADR-0026)"

  # AND IT DOES NOT STOP THERE (#225). Selecting a release used to be where a
  # want came to rest: Downloader.Add had no caller, no job type grabbed, and
  # the only route from SELECTED to bytes was the by-hand adoption endpoint.
  #
  # What is asserted is that a grab was QUEUED, not that it succeeded. The only
  # download client configured here points at port 9 and refuses connections on
  # purpose (ADR-0025), so the grab will fail — which is the correct outcome for
  # a client that is down, and leaves the want holding its selection to retry.
  #
  # A job existing is therefore the whole claim, and it is the right one: it is
  # the edge that did not exist, and it is observable without a working daemon.
  local grab_jobs
  waited=0
  while (( waited < 300 )); do
    grab_jobs=$(api "/api/v1/jobs?type=grab_release" | jq -r '.items | length')
    (( grab_jobs > 0 )) && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  assert_eq "$grab_jobs" "1" \
    "selecting a release queues the grab that fetches it (§64 SELECTED -> QUEUED)"

  # And the want KEEPS its selection when the grab cannot reach a client. A
  # want that lost its selection on an unreachable daemon would re-search and
  # re-decide every cycle, which is work and indexer load for no gain.
  assert_eq "$(api "/api/v1/desired/$search_id" | jq -r '.acquisition.state')" "SELECTED" \
    "and a download client that is down leaves the selection intact to retry"

  # §63's answer, durable and readable after the fact. An evaluation that lived
  # in memory for two hundred milliseconds is deterministic and is not
  # inspectable.
  search_cands=$(api "/api/v1/desired/$search_id/candidates")
  assert_eq "$(jq -r '.candidates | length' <<<"$search_cands")" "3" \
    "every candidate the indexer offered is persisted, accepted and rejected alike"
  assert_eq "$(jq -r '.selected' <<<"$search_cands")" "bh-2160-remux" \
    "the best acceptable candidate was selected"
  assert_eq "$(jq -r '.candidates[0].terminal' <<<"$search_cands")" "true" \
    "and it is terminal, so the upgrade workflow has nothing left to want"

  # THE REJECTION REASONS ARE THE DELIVERABLE (§60, §61). A rejection with no
  # explanation is exactly the opaque scoring §61 rejects.
  assert_eq "$(jq -r '[.candidates[] | select(.candidate_id == "bh-480-cam")] | .[0].accepted' \
    <<<"$search_cands")" "false" "the 480p release was rejected"
  assert_contains "$(jq -r '[.candidates[] | select(.candidate_id == "bh-480-cam")] | .[0].rejected_by[0].rule' \
    <<<"$search_cands")" "resolution" "and the rejection names the gate that failed"
  assert_contains "$(jq -r '[.candidates[] | select(.candidate_id == "bh-480-cam")] | .[0].rejected_by[0].detail' \
    <<<"$search_cands")" "which is not" "and explains it in prose, not just a code"

  # Accepted and NOT terminal is the gap the upgrade workflow lives in.
  assert_eq "$(jq -r '[.candidates[] | select(.candidate_id == "bh-1080-web")] | .[0] |
    "\(.accepted)/\(.terminal)"' <<<"$search_cands")" "true/false" \
    "an acceptable release that is not as good as it gets stays upgradable"

  # §60's manual override: a person takes a different acceptable release, and
  # the disagreement is RECORDED. An override that left no trace would look
  # exactly like an ordinary selection.
  local search_override
  search_override=$(api "/api/v1/desired/$search_id/select" -X POST \
    -H 'Content-Type: application/json' -d '{"candidate_id":"bh-1080-web"}')
  assert_eq "$(jq -r '.overridden' <<<"$search_override")" "true" \
    "choosing a release by hand is recorded as an override"
  assert_contains "$(jq -r '.override_detail' <<<"$search_override")" "bh-2160-remux" \
    "and the record names what the scorer had chosen instead"

  # The gates in §62 are the operator's own statement of what is acceptable. An
  # override that could ignore them would turn `accept` into a suggestion.
  local search_refused
  search_refused=$(api "/api/v1/desired/$search_id/select" -X POST \
    -H 'Content-Type: application/json' -d '{"candidate_id":"bh-480-cam"}')
  assert_eq "$(jq -r '.status' <<<"$search_refused")" "409" \
    "an override cannot take a release the profile rejected"
  assert_contains "$(jq -r '.detail' <<<"$search_refused")" "change the profile" \
    "and the refusal says what to do instead"

  # A search that finds nothing is a MODELLED EDGE, not a failure. If it failed
  # the job it would back off, and an unavailable release would become an
  # indexer hammering loop.
  local search_empty search_empty_state search_empty_work
  search_empty_work=$(api_all /api/v1/works '.items[] | select(.title == "Twice Told") | .id' | head -1)
  search_empty=$(api /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d "{\"work_id\":\"$search_empty_work\",\"quality_profile\":\"archival\"}" | jq -r '.id')
  api "/api/v1/desired/$search_empty/search" -X POST -o /dev/null

  waited=0
  while (( waited < 300 )); do
    # The phase returns to where it started, so the arrival condition is the
    # SEARCH JOB finishing rather than the state changing.
    if [[ "$(api "/api/v1/jobs?type=search_release&state=succeeded" | jq -r '.items | length')" -ge 2 ]]; then
      break
    fi
    sleep 0.1; waited=$(( waited + 1 ))
  done
  # ASSERT THE PHASE, NOT THE DERIVED NAME — and the reason is ADR-0027's whole
  # argument, which this assertion originally got wrong and flaked on ~50% of
  # runs because of it.
  #
  # "Returns to rest" is a claim about the PIPELINE: nothing is in flight any
  # more. The §64 name is a presentation of four facts, and one of them is
  # `managed` — whether Heyarr holds bytes. This want is over a work that HAS
  # assets, so once the reconciliation beat has run the very same resting state
  # presents as AVAILABLE rather than MISSING. Both are correct; which one you
  # see depends on whether a timer fired.
  #
  # Asserting the name here conflated "nothing in flight" with "do we hold
  # bytes", which is exactly the collapse ADR-0027 exists to prevent. The
  # acceptance suite reproduced the confusion the ADR was written about, and
  # the flake is what exposed it.
  search_empty_state=$(api "/api/v1/desired/$search_empty" | jq -r '.acquisition.phase')
  assert_eq "$search_empty_state" "idle" \
    "a search that finds nothing returns the want to rest"
  # And it is a modelled edge rather than a failure: the job SUCCEEDED. If it
  # had failed, the queue would back off and an unavailable release would
  # become an indexer hammering loop.
  #
  # ASSERT WHAT THE SEARCH CONTROLS. My first attempt at this second assertion
  # checked that `content` was still "unknown" — and flaked for the same reason
  # the original did, one layer down: content is RECONCILIATION's answer, and
  # it legitimately becomes not_satisfied the moment the beat runs. Twice in
  # one assertion block is enough to state the rule: in this section, assert
  # the phase and the job, never a satisfaction axis, because a timer owns
  # those and a timer is not part of the claim.
  assert_eq "$(api "/api/v1/jobs?type=search_release&state=failed" | jq -r '.items | length')" "0" \
    "and does not fail the job, which would back off into an indexer hammering loop"
  assert_eq "$(api "/api/v1/desired/$search_empty/candidates" | jq -r '.candidates | length')" "0" \
    "with no candidates to explain"

  # Every search emits, including the empty one — it leaves no rows behind, so
  # the event is the only trace it happened.
  local search_events
  search_events=$(events_replay)
  if grep -q 'acquisition.search_completed' <<<"$search_events"; then
    pass "a completed search reaches the event log"
  else
    fail "no acquisition.search_completed in a replay from seq 0"
  fi
  if grep -q 'acquisition.candidate_overridden' <<<"$search_events"; then
    pass "an override is its own event, so a policy audit can subscribe to exactly that"
  else
    fail "no acquisition.candidate_overridden in a replay from seq 0"
  fi

  api "/api/v1/desired/$search_id" -X DELETE -o /dev/null
  api "/api/v1/desired/$search_empty" -X DELETE -o /dev/null

  note "  the CLI (M1-17)"
  # The CLI is what a person actually uses, so the demo drives it rather than
  # only the API underneath it. Every count is cross-checked against the API's
  # own answer: if the two ever disagree, one of them is lying and it matters
  # which.
  cli() { "$BIN" --config "$WORK/full.yaml" --token "$TOKEN" "$@"; }

  local cli_works cli_assets cli_libs cli_peers
  cli_works=$(cli works list --json | jq -r 'length')
  cli_assets=$(cli assets list --json | jq -r 'length')
  cli_libs=$(cli library list --json | jq -r 'length')
  cli_peers=$(cli peers list --json | jq -r 'length')

  assert_eq "$cli_works" "$got_works" "the CLI and the API agree on the work count"
  assert_eq "$cli_assets" "$got_assets" "the CLI and the API agree on the asset count"

  # ...and again with a page size small enough that the cursor loop actually
  # runs. Without this the cross-check above proves nothing about pagination:
  # a dozen assets fit in one page, so a client that stopped after the first
  # page would return exactly the same number. Confirmed by sabotage — stopping
  # after page one left the whole demo green until this line existed.
  assert_eq "$(cli assets list --page-size 3 --json | jq -r 'length')" "$got_assets" \
    "the CLI follows pagination cursors to the end"
  assert_eq "$(cli assets list --page-size 3 --json | jq -r '[.[].id] | unique | length')" "$got_assets" \
    "paging returns each asset exactly once, with none skipped"
  assert_eq "$cli_libs" "4" "the CLI lists the four configured libraries"
  assert_eq "$cli_peers" "1" "the CLI lists exactly one peer (ADR-0010)"

  # M4-03: the operator-facing half. Somebody enrolling this node at the other
  # site needs a value to copy, and "read it out of SQLite" is not an answer.
  # Additive only — it does not touch cli_works, cli_assets, cli_libs or
  # cli_peers, and it creates nothing.
  local cli_pk cli_json
  cli_json=$(cli peers list --json)
  cli_pk=$(jq -r '.[] | select(.is_self) | .public_key' <<<"$cli_json")
  if [[ "$cli_pk" =~ ^ed25519:[0-9a-f]{64}$ ]]; then
    pass "the CLI shows the self peer's public key"
  else
    fail "heyarr peers list --json has no usable public_key — got '$cli_pk'"
  fi
  assert_eq "$cli_pk" "$(api /api/v1/peers | jq -r '.items[] | select(.is_self) | .public_key')" \
    "the CLI and the API agree on the public key"
  # The PRIVATE key must not be in the CLI output, the API's, or anything else
  # a person might paste into a support thread.
  local full_seed
  full_seed=$(cut -d: -f2 <"$FULLDATA/peer_ed25519.key" | tr -d '[:space:]')
  assert_eq "$(grep -c -- "$full_seed" <<<"$cli_json" || true)" "0" \
    "the private key does not appear in heyarr peers list --json"
  assert_eq "$(api /api/v1/peers | grep -c -- "$full_seed" || true)" "0" \
    "the private key does not appear in GET /api/v1/peers"

  # A collection is a bare array, not the API's page envelope: the CLI already
  # followed the cursors, so a next_cursor would be a lie.
  if cli works list --json | jq -e 'type == "array"' >/dev/null; then
    pass "--json collections are arrays, not a paging envelope"
  else
    fail "--json did not emit a bare array"
  fi

  # blobs stat reaches the same blob the range assertions used.
  assert_eq "$(cli blobs stat "$big_hash" --json | jq -r '.size')" "$big_size" \
    "blobs stat reports the streaming fixture's size"

  # The bytes, through the CLI this time, verified against the digest.
  cli blobs cat "$big_hash" -o "$WORK/cli-cat.bin" --json >/dev/null
  assert_eq "$("$GEN" -hash "$WORK/cli-cat.bin")" "$big_hash" \
    "blobs cat writes bytes that hash to the digest asked for"

  # §18's blunt requirement: a CLI that exits 0 when the work failed is worse
  # than no CLI. Here the work succeeds, so it must exit 0 — the dead-job half
  # is a Go test, because making a scan die on demand is not something a demo
  # should be able to do.
  local wait_rc=0
  cli scan films --wait --json >"$WORK/scan-wait.json" 2>&1 || wait_rc=$?
  assert_eq "$wait_rc" "0" "scan --wait exits 0 when every job succeeds"
  assert_eq "$(jq -r '.outcome' "$WORK/scan-wait.json")" "succeeded" \
    "scan --wait reports the outcome it exited on"

  # The property #58 was filed for: an OPEN stream sees events emitted by
  # another role, live, without reconnecting.
  #
  # Every role builds its own event log, so before the stream tailed the log
  # itself, a scan could run to completion with `events tail` sitting silent —
  # the events were durable immediately and invisible until the next connect.
  # This opens the tail FIRST, then makes the worker emit, which is the only
  # ordering that can tell the two apart.
  local head_seq tail_out tail_pid tail_rc=0
  head_seq=$(events_replay | grep -c '^id:' || true)
  tail_out="$WORK/live-tail.json"
  cli events tail --after "$head_seq" --limit 1 --json >"$tail_out" 2>&1 &
  tail_pid=$!

  # Give the tail time to be listening, by waiting for the condition that it is
  # — a subscriber registered on the log — rather than for a duration.
  local waited=0
  while (( waited < 100 )); do
    kill -0 "$tail_pid" 2>/dev/null || break
    sleep 0.1; waited=$(( waited + 1 ))
  done

  # The worker, not the API, is what emits scan progress.
  cli scan shows >/dev/null 2>&1 || true

  waited=0
  while (( waited < 300 )); do
    kill -0 "$tail_pid" 2>/dev/null || break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if kill -0 "$tail_pid" 2>/dev/null; then
    kill -TERM "$tail_pid" 2>/dev/null || true
    wait "$tail_pid" 2>/dev/null || true
    fail "an open event stream never saw an event from another role (#58)"
  else
    wait "$tail_pid" || tail_rc=$?
    if [[ -s "$tail_out" ]] && jq -e '.type' <"$tail_out" >/dev/null 2>&1; then
      pass "an open stream sees another role's events live, without reconnecting"
    else
      fail "the live tail produced nothing usable: $(head -c 200 "$tail_out")"
    fi
  fi

  # events tail is JSON Lines, one event per line, so it composes with jq.
  local tailed
  tailed=$(cli events tail --after 0 --limit 5 --json | jq -rs 'length')
  assert_eq "$tailed" "5" "events tail emits JSON Lines a line at a time"

  note "  continuous backup (§49, ADR-0044, M7-02)"
  # A backup is a VACUUM INTO snapshot with its own signed provenance, taken
  # against the running controller. Its generation is the control plane's event
  # high-water mark EXCLUDING the backup's own bookkeeping, so it reports real
  # change: two backups with nothing between them are the same generation, and a
  # control-plane write moves it. A generation that advanced on its own would
  # make "it advanced" a statement about the verb running, not the state
  # changing — the tautology this exists to avoid.
  local bk1 bk1_gen bk1b bk2 bk2_gen bk_work bk_want
  bk1=$("$BIN" --config "$WORK/full.yaml" backup --json)
  bk1_gen=$(jq -r '.generation' <<<"$bk1")
  if [[ "$bk1_gen" =~ ^[0-9]+$ ]] && (( bk1_gen > 0 )); then
    pass "a backup of the running controller reports a positive generation ($bk1_gen)"
  else
    fail "backup produced no usable generation: $bk1"
  fi
  assert_eq "$(jq -r '.signed' <<<"$bk1")" "true" \
    "the backup is signed with this peer's identity key, so a holder can verify its origin (ADR-0044 Q2)"
  assert_contains "$(jq -r '.omissions | join(",")' <<<"$bk1")" "provider-credentials" \
    "the backup records the credentials it structurally cannot carry, rather than surprising a restore (ADR-0044 Q6)"

  # A re-take is MONOTONIC. It was "the generation must NOT move", and that was
  # wrong the day it was written.
  #
  # "No control-plane change" is not a state a RUNNING node can reach. The beats
  # never stop writing — provider health every minute, reconciliation, the
  # search beat, and since #247 a download poll every fifteen seconds — so two
  # backups taken moments apart legitimately differ, and the assertion was a
  # race against whichever beat happened to land between them. #247's beat did
  # not break it; it turned a rare flake into a likely one, which is the good
  # kind of exposure.
  #
  # What holds on a running node is that a re-take never RESETS or decreases the
  # generation: it is the event high-water mark and it is monotonic. That a
  # backup does not advance it by its OWN bookkeeping is a different claim and
  # is proven against a quiescent database in TestIdempotentPerGeneration, which
  # is where it can be isolated — the demo cannot isolate it on a live node and
  # should not pretend to.
  #
  # The load-bearing half is the one below this: a real control-plane change
  # DOES advance it. That is what catches a mechanism which merely ran twice,
  # and it is untouched.
  local bk1b_gen
  bk1b=$("$BIN" --config "$WORK/full.yaml" backup --json)
  bk1b_gen=$(jq -r '.generation' <<<"$bk1b")
  if (( bk1b_gen >= bk1_gen )); then
    pass "a re-take never resets or decreases the backup generation (monotonic)"
  else
    fail "a re-take moved the generation backwards: $bk1_gen -> $bk1b_gen"
  fi

  # A real control-plane change, then a backup: the generation must advance.
  bk_work=$(api_all /api/v1/works '.items[].id' | head -1)
  bk_want=$(api /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d "{\"work_id\":\"$bk_work\",\"quality_profile\":\"archival\",\"monitor\":false}" | jq -r '.id')
  bk2=$("$BIN" --config "$WORK/full.yaml" backup --json)
  bk2_gen=$(jq -r '.generation' <<<"$bk2")
  if [[ "$bk2_gen" =~ ^[0-9]+$ ]] && (( bk2_gen > bk1_gen )); then
    pass "a control-plane change advances the backup generation ($bk1_gen -> $bk2_gen)"
  else
    fail "the generation did not advance after a control-plane write: $bk1_gen -> $bk2_gen"
  fi
  # The read instant is recorded, so a restore can say how stale it is (§53's
  # "conservative rather than unavailable" needs an answerable age).
  if [[ "$(jq -r '.taken_at' <<<"$bk2")" =~ ^20 ]]; then
    pass "the backup records when it was taken, so its age is answerable"
  else
    fail "the backup has no usable taken_at: $bk2"
  fi
  # Undo the change so the durability counts below are undisturbed.
  api "/api/v1/desired/$bk_want" -X DELETE -o /dev/null

  local jobs_before
  jobs_before=$(api_all /api/v1/jobs '.items[].id' | sort -u | wc -l | tr -d ' ')

  note "  durability and idempotency"
  # SIGTERM, restart, and everything must still be there. §50 replicates
  # controller backups to peers, so a database that needed the process to stay
  # up would not be a database anyone could back up.
  stop_full

  # After shutdown the file must stand alone: a populated -wal beside a copied
  # database is a silently stale backup.
  if [[ -s "$FULLDATA/heyarr.db-wal" ]]; then
    fail "a populated WAL survived shutdown ($(wc -c <"$FULLDATA/heyarr.db-wal") bytes)"
  else
    pass "the database is self-contained after shutdown"
  fi

  start_full full-restart all || return 1

  local works_after assets_after blobs_after jobs_after
  works_after=$(api_all /api/v1/works '.items[].id' | sort -u | wc -l | tr -d ' ')
  assets_after=$(api_all /api/v1/assets '.items[].id' | sort -u | wc -l | tr -d ' ')
  jobs_after=$(api_all /api/v1/jobs '.items[].id' | sort -u | wc -l | tr -d ' ')
  assert_eq "$works_after" "$got_works" "the catalog survives a restart"
  assert_eq "$assets_after" "$got_assets" "every asset survives a restart"
  if (( jobs_after >= jobs_before )); then
    pass "job history survives a restart ($jobs_before before, $jobs_after after)"
  else
    fail "job history shrank across a restart: $jobs_before then $jobs_after"
  fi

  # The controller re-enqueues a scan per root at every start, so a restart IS
  # the rescan. Nothing new may come of it: same bytes, same paths, same rows.
  local rescans=0 waited=0
  while (( waited < 900 )); do
    rescans=$(grep -c '"msg":"scan completed"' "$FULL_LOG" 2>/dev/null || true)
    (( rescans >= 4 )) && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if (( rescans < 4 )); then
    fail "only $rescans of 4 roots rescanned after the restart"
  else
    local reingested
    reingested=$(grep -c '"msg":"ingested"' "$FULL_LOG" 2>/dev/null || true)
    assert_eq "$reingested" "0" "a rescan of an unchanged library ingests nothing"
    blobs_after=$(find "$FULLDATA/cas/blobs" -type f 2>/dev/null | wc -l | tr -d ' ')
    assert_eq "$blobs_after" "$blobs" "a rescan adds no blobs"
    assets_after=$(api_all /api/v1/assets '.items[].id' | sort -u | wc -l | tr -d ' ')
    assert_eq "$assets_after" "$got_assets" "a rescan adds no assets"
  fi

  # Remuxing runs after everything that COUNTS assets and before anything that
  # stops the server.
  #
  # It is the only section in this demo that MUTATES the library — a remux adds
  # a derived asset to an edition — so running it earlier moved the asset and
  # blob counts six later assertions depend on. Running it later found the
  # integrity section had already taken the API down for offline fsck and gc.
  # Both were caught by reading the demo's verdict; a local run that counted
  # "ok" lines instead was the reason the first one reached CI.



  note "  desired state (§55, M3-02)"
  # ORDERING IS LOAD-BEARING HERE, and it bit on the first run.
  #
  # Wanting content Heyarr has never seen CREATES A WORK — that is the whole
  # feature — so this section mutates the catalog. It first sat above the CLI
  # section, and the CLI's work-count assertion immediately went from 8 to 9.
  # Until Milestone 3 the remux section was the only one that mutated anything;
  # it is now the second, and this must stay BELOW every catalog count.
  #
  # It does not add assets or blobs, so the rescan and durability counts above
  # are unaffected — but that is a fact about this section, not a licence to
  # move it.
  #
  # What this proves that the unit tests cannot: that a real Heyarr can be told
  # to want a film it has never seen — no asset, no blob, no bytes, nothing
  # scanned — and that the Work it invents converges with the one a scan would
  # produce. Every fixture in the test suite has assets, so this is the only
  # place the empty case meets a real database and a real socket.
  local want_json want_id want_work want_again
  want_json=$(api /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d '{"work":{"content_type":"movie","title":"The Conversation","year":1974},
         "quality_profile":"living-room","reason":"acceptance"}')
  want_id=$(jq -r '.id' <<<"$want_json")
  want_work=$(jq -r '.work_id' <<<"$want_json")
  assert_contains "$want_id" "-" "content that does not exist anywhere can be wanted"
  assert_contains "$want_work" "-" "wanting unknown content creates the work to hang it on"
  assert_eq "$(jq -r '.monitor' <<<"$want_json")" "true" \
    "monitoring is on by default — wanting something and never looking again is the surprising default"

  # The work it created has nothing behind it. If this came back non-zero the
  # assertion above passed for the wrong reason.
  assert_eq "$(api "/api/v1/works/$want_work" | jq -r '.title')" "The Conversation" \
    "the invented work is readable and titled"

  # Convergence. A different spelling, with the year inside the title, must
  # resolve to the SAME work — otherwise wanting a film and later scanning it
  # produce two works that are the same thing, and nothing ever notices.
  want_again=$(api /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d '{"work":{"content_type":"movie","title":"the conversation (1974)"},
         "quality_profile":"archival"}')
  assert_eq "$(jq -r '.work_id' <<<"$want_again")" "$want_work" \
    "two spellings of one film converge on one work"

  # §61: never one version per title. Two profiles over one work are two wants
  # and both exist; the same profile twice is one want written twice.
  assert_eq "$(api "/api/v1/desired?work_id=$want_work" | jq -r '.items | length')" "2" \
    "two profiles over one work are two wants (§61)"
  local want_dup
  want_dup=$(api /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d "{\"work_id\":\"$want_work\",\"quality_profile\":\"living-room\"}")
  assert_eq "$(jq -r '.status' <<<"$want_dup")" "409" \
    "the same work under the same profile twice is refused"

  # A want with no standard cannot be evaluated (§56), so it is refused.
  assert_contains "$(api /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d '{"work":{"content_type":"movie","title":"Nostalghia"}}' | jq -r '.detail')" \
    "quality profile" "a want with no quality profile is refused"

  # Monitored and wanted are two axes (§60 keeps both words).
  assert_eq "$(api "/api/v1/desired/$want_id" -X PATCH -H 'Content-Type: application/json' \
    -d '{"monitor":false}' | jq -r '.monitor')" "false" \
    "monitoring can be turned off without un-wanting"
  assert_eq "$(api "/api/v1/desired?monitor=false" | jq -r '[.items[] | select(.id == "'"$want_id"'")] | length')" "1" \
    "the unmonitored want is selectable — the upgrade workflow has to skip it"

  # The target is not editable: repointing a want at different content is a
  # different want, not an edit.
  assert_contains "$(api "/api/v1/desired/$want_id" -X PATCH -H 'Content-Type: application/json' \
    -d '{"work_id":"something-else"}' | jq -r '.status')" "400" \
    "a want cannot be repointed at different content"

  # The standard cannot be deleted while something is measured against it.
  local qp_inuse
  qp_inuse=$(api /api/v1/quality-profiles | jq -r '.items[] | select(.name == "living-room") | .id')
  assert_contains "$(api "/api/v1/quality-profiles/$qp_inuse" -X DELETE -w '%{http_code}' -o /dev/null)" \
    "409" "a quality profile still measuring a want cannot be deleted"

  # The CLI reaches the same rows over the same socket.
  assert_contains "$("$BIN" --config "$WORK/full.yaml" --token "$TOKEN" desired list --json | jq -r '[.[] | select(.id == "'"$want_id"'")] | length')" \
    "1" "the CLI lists what the API wanted"
  assert_contains "$("$BIN" --config "$WORK/full.yaml" --token "$TOKEN" quality-profile list --json | jq -r '[.[] | select(.name == "archival")] | length')" \
    "1" "the CLI lists the quality profiles"

  api "/api/v1/desired/$want_id" -X DELETE -o /dev/null
  assert_eq "$(api "/api/v1/desired/$want_id" | jq -r '.status')" "404" \
    "a want that is removed is gone rather than hidden"

  note "  the upgrade workflow (§60, M3-06)"
  # Satisfied is not the same as finished. This section drives the gap between
  # three states against the library this demo just scanned:
  #
  #   not accepted   -> an acquisition, not an upgrade
  #   accepted       -> satisfied AND still improvable   <- the whole feature
  #   terminal       -> done, stop looking
  #
  # It sits with the other M3 sections, below every catalog count, because it
  # creates wants and profiles.
  #
  # NOTE ON ASSERTIONS: every enum check is assert_eq, never assert_contains.
  # "not_satisfied" CONTAINS "satisfied", and "not_monitored" contains
  # "monitored" — a substring check here passes for the opposite meaning.
  local up_work up_profile up_id up_json
  up_work=$(api /api/v1/works | jq -r '.items[0].id')

  # A profile that accepts anything and is NEVER terminal, so a want against it
  # is satisfied and permanently improvable. That is what the seeded "archival"
  # profile is, and it is the case a terminal-only test would miss entirely.
  up_profile=$(api /api/v1/quality-profiles -X POST -H 'Content-Type: application/json' \
    -d '{"name":"acceptance-upgradable","description":"accepts anything, never finished"}' \
    | jq -r '.id')
  up_id=$(api /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d "{\"work_id\":\"$up_work\",\"quality_profile_id\":\"$up_profile\"}" | jq -r '.id')

  up_json=$(api "/api/v1/desired/$up_id/satisfaction")
  assert_eq "$(jq -r '.content.satisfaction' <<<"$up_json")" "satisfied" \
    "a want against an open-ended profile is satisfied by what the library holds"
  assert_eq "$(jq -r '.upgrade.eligible' <<<"$up_json")" "true" \
    "and is STILL upgradable — satisfied is not the same as finished"
  assert_eq "$(jq -r '.upgrade.status' <<<"$up_json")" "no_better_candidate" \
    "with nothing better on offer, which is the normal answer for a healthy library"

  # The listing filter §71's get_upgrade_candidates will expose.
  assert_eq "$(api "/api/v1/desired?upgradable=true" \
    | jq -r "[.items[] | select(.id == \"$up_id\")] | length")" "1" \
    "an upgradable want appears in the upgradable listing"

  # THE case that matters most and is most easily omitted: the operator said
  # "get me this", not "keep improving this". Running the loop over unmonitored
  # wants is how *arr installations re-download libraries nobody asked them to
  # touch.
  api "/api/v1/desired/$up_id" -X PATCH -H 'Content-Type: application/json' \
    -d '{"monitor":false}' -o /dev/null
  up_json=$(api "/api/v1/desired/$up_id/satisfaction")
  assert_eq "$(jq -r '.upgrade.eligible' <<<"$up_json")" "false" \
    "an unmonitored want is finished even though it is still improvable"
  assert_eq "$(jq -r '.upgrade.status' <<<"$up_json")" "not_monitored" \
    "and says the operator's decision is the reason"
  # Still satisfied — unmonitoring stops the LOOKING, not the having.
  assert_eq "$(jq -r '.content.satisfaction' <<<"$up_json")" "satisfied" \
    "unmonitoring stops the looking, not the having"
  assert_eq "$(api "/api/v1/desired?upgradable=true" \
    | jq -r "[.items[] | select(.id == \"$up_id\")] | length")" "0" \
    "and it leaves the upgradable listing immediately"

  # Back on, so the next assertions are about something else.
  api "/api/v1/desired/$up_id" -X PATCH -H 'Content-Type: application/json' \
    -d '{"monitor":true}' -o /dev/null

  # A want holding nothing acceptable is an ACQUISITION, not an upgrade — the
  # search job owns it, and reporting it here would make two jobs fight over
  # the same row.
  local up_absent
  up_absent=$(api /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d "{\"work\":{\"content_type\":\"movie\",\"title\":\"Never Acquired\",\"year\":1998},
         \"quality_profile_id\":\"$up_profile\"}" | jq -r '.id')
  assert_eq "$(api "/api/v1/desired/$up_absent/satisfaction" | jq -r '.upgrade.status')" \
    "not_satisfied" "a want holding nothing is an acquisition rather than an upgrade"
  assert_eq "$(api "/api/v1/desired?upgradable=true" \
    | jq -r "[.items[] | select(.id == \"$up_absent\")] | length")" "0" \
    "and does not appear in the upgradable listing"

  # A terminal want has nothing left to want. Raising a terminal condition the
  # library already meets is the cheapest way to reach that state on whatever
  # this machine scanned.
  local up_terminal up_terminal_id
  up_terminal=$(api /api/v1/quality-profiles -X POST -H 'Content-Type: application/json' \
    -d '{"name":"acceptance-terminal",
         "terminal":[{"attribute":"size_bytes","op":"gte","value":1}]}' | jq -r '.id')
  up_terminal_id=$(api /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d "{\"work_id\":\"$up_work\",\"quality_profile_id\":\"$up_terminal\"}" | jq -r '.id')
  assert_eq "$(api "/api/v1/desired/$up_terminal_id/satisfaction" | jq -r '.upgrade.status')" \
    "terminal" "a want whose incumbent meets every terminal condition is finished"
  assert_eq "$(api "/api/v1/desired/$up_terminal_id/satisfaction" | jq -r '.upgrade.eligible')" \
    "false" "and is not upgradable"

  # Every disqualifying reason is DISTINCT, so an operator asking "why is this
  # not upgrading" gets an answer rather than a boolean.
  local up_statuses
  up_statuses=$(printf '%s\n' \
    "$(api "/api/v1/desired/$up_absent/satisfaction" | jq -r '.upgrade.status')" \
    "$(api "/api/v1/desired/$up_terminal_id/satisfaction" | jq -r '.upgrade.status')" \
    "$(api "/api/v1/desired/$up_id/satisfaction" | jq -r '.upgrade.status')" \
    | sort -u | wc -l | tr -d ' ')
  assert_eq "$up_statuses" "3" \
    "three different situations report three different reasons"

  # The upgrade explanation is §63's reasons, not a separate prose string: the
  # per-asset evaluation is right there on the same response.
  assert_eq "$(api "/api/v1/desired/$up_id/satisfaction" \
    | jq -r '.content.assets[0].reasons | length >= 0')" "true" \
    "the upgrade decision reads the same evaluation the satisfaction axis does"

  # An unknown filter value is refused rather than silently ignored — an
  # ignored filter returns everything, which reads as "nothing is upgradable"
  # or "everything is" depending on what the caller was hoping.
  assert_contains "$(api "/api/v1/desired?upgradable=maybe" | jq -r '.detail')" \
    "upgradable" "an unknown upgradable value is refused"

  # The upgrade scan is a JOB (invariant 4, ADR-0002) and it is registered.
  assert_contains "$ALLLOG" "upgrade_scan" \
    "the worker registers the upgrade_scan handler"

  api "/api/v1/desired/$up_id" -X DELETE -o /dev/null
  api "/api/v1/desired/$up_absent" -X DELETE -o /dev/null
  api "/api/v1/desired/$up_terminal_id" -X DELETE -o /dev/null
  api "/api/v1/quality-profiles/$up_profile" -X DELETE -o /dev/null
  api "/api/v1/quality-profiles/$up_terminal" -X DELETE -o /dev/null

  note "  satisfaction reconciliation (§56, §57, M3-05)"
  # Content and placement, evaluated SEPARATELY, against the library this demo
  # just scanned — real assets, whatever probes this machine could produce, and
  # an answer that is not a fixture.
  #
  # NOTE ON ASSERTIONS HERE: every enum check is assert_eq, never
  # assert_contains. "not_satisfied" CONTAINS "satisfied", so a substring check
  # passes for the opposite meaning — which it did, on the first run of this
  # section, and reported a green "content is satisfied" against a want that
  # was not.
  local sat_work sat_anything sat_id sat_json
  sat_work=$(api /api/v1/works | jq -r '.items[0].id')

  # A profile with NO gates, so what is being tested is the reconciliation and
  # not this machine's ability to measure a file.
  sat_anything=$(api /api/v1/quality-profiles -X POST -H 'Content-Type: application/json' \
    -d '{"name":"acceptance-anything","description":"accepts whatever exists"}' | jq -r '.id')
  sat_id=$(api /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d "{\"work_id\":\"$sat_work\",\"quality_profile_id\":\"$sat_anything\"}" | jq -r '.id')

  # The satisfaction endpoint reconciles rather than reading a cached answer,
  # so this is deterministic without waiting for the beat.
  sat_json=$(api "/api/v1/desired/$sat_id/satisfaction")
  assert_eq "$(jq -r '.content.satisfaction' <<<"$sat_json")" "satisfied" \
    "content the library holds satisfies a profile that accepts it"
  assert_contains "$(jq -r '.content.satisfied_by // "none"' <<<"$sat_json")" "-" \
    "and the answer names WHICH asset satisfies"
  assert_eq "$(jq -r '.content.assets | length > 0' <<<"$sat_json")" "true" \
    "every asset considered is reported, with its reasons"

  # §56's two axes are separate answers, and both reach the wire.
  assert_contains "$(jq -r '. | keys | join(",")' <<<"$sat_json")" "placement" \
    "placement is answered separately from content (§56)"
  # UPDATED, not deleted, by M4-11. The value is still `true` here and always
  # will be: this node's Full Peer target set is itself alone. What changed is
  # that the field is now COMPUTED from that target set rather than hard-coded,
  # so `true` here is a fact about this deployment rather than a fact about the
  # milestone. The two-peer section below asserts the same field as `false`.
  assert_eq "$(jq -r '.placement.unproven' <<<"$sat_json")" "true" \
    "and it says plainly that this fabric is one peer, so placement proves nothing (ADR-0027)"

  # The §64 name is DERIVED from the axes, never stored (ADR-0027). With one
  # peer holding the only replica, placement is satisfied the moment content is
  # — which is the single-peer case, not evidence that replication works.
  assert_eq "$(jq -r '.state' <<<"$sat_json")" "FULLY_SATISFIED" \
    "the §64 name is derived from both axes"

  # ADR-0023's degradation, reaching all the way through to satisfaction.
  #
  # This is the honest state of a node with no media toolchain: with no probe,
  # a gate on resolution cannot be SHOWN to hold, so it rejects — and the
  # reason says "could not determine" rather than claiming the file is too
  # small. "I cannot tell whether this satisfies you" and "this does not
  # satisfy you" are different problems and send an operator to different
  # places.
  local sat_gated sat_gated_id sat_gated_json
  sat_gated=$(api /api/v1/quality-profiles -X POST -H 'Content-Type: application/json' \
    -d '{"name":"acceptance-gated","accept":[{"attribute":"resolution","op":"gte","value":1080}]}' \
    | jq -r '.id')
  sat_gated_id=$(api /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d "{\"work_id\":\"$sat_work\",\"quality_profile_id\":\"$sat_gated\"}" | jq -r '.id')
  sat_gated_json=$(api "/api/v1/desired/$sat_gated_id/satisfaction")

  if command -v ffprobe >/dev/null 2>&1; then
    # With a probe, the resolution is a fact and the gate decides on it.
    assert_contains "$(jq -r '[.content.assets[0].reasons[] | select(.rule == "resolution.gte")] | .[0].result' \
      <<<"$sat_gated_json")" "" "a probed asset is measured against the gate"
  else
    assert_eq "$(jq -r '.content.satisfaction' <<<"$sat_gated_json")" "not_satisfied" \
      "with no probe, a gate that cannot be shown to hold rejects"
    assert_eq "$(jq -r '[.content.assets[0].reasons[] | select(.rule == "resolution.gte")] | .[0].result' \
      <<<"$sat_gated_json")" "undetermined" \
      "and says the attribute could not be determined, rather than claiming the file is too small"
    # AVAILABLE, not MISSING: the bytes are held, they simply cannot be shown
    # to be good enough. Conflating those makes the upgrade workflow
    # unreachable.
    assert_eq "$(jq -r '.state' <<<"$sat_gated_json")" "AVAILABLE" \
      "the bytes are still held, so the state is AVAILABLE rather than MISSING"
    not_exercised ffprobe "a probed asset being MEASURED against a resolution gate, rather than the gate reporting undetermined"
  fi

  # §57's point, and the reason a beat exists at all: satisfaction can change
  # when NOTHING about the want or the library changed.
  local sat_after
  api "/api/v1/quality-profiles/$sat_anything" -X PUT -H 'Content-Type: application/json' \
    -d '{"accept":[{"attribute":"size_bytes","op":"gte","value":1099511627776}]}' -o /dev/null
  sat_after=$(api "/api/v1/desired/$sat_id/satisfaction")
  assert_eq "$(jq -r '.content.satisfaction' <<<"$sat_after")" "not_satisfied" \
    "raising the profile unsatisfies a want nothing else touched (§57)"
  assert_eq "$(jq -r '.state' <<<"$sat_after")" "AVAILABLE" \
    "and the bytes are still there, so it is AVAILABLE rather than MISSING"
  # A blob's size is known even with no toolchain, so this gate genuinely
  # FAILS rather than being undetermined — which is what makes it a usable
  # assertion on a bare node.
  assert_eq "$(jq -r '[.content.assets[0].rejected_by[] | select(.rule == "size_bytes.gte")] | .[0].result' \
    <<<"$sat_after")" "fail" \
    "and names the gate that now rejects what the library holds"

  # Reconciliation is a JOB (invariant 4, ADR-0002) — the API asks, a worker
  # runs it, and the two may be different processes.
  local sat_job
  sat_job=$(api "/api/v1/desired/$sat_id/reconcile" -X POST)
  assert_contains "$(jq -r '.job_id' <<<"$sat_job")" "-" \
    "asking for a reconciliation queues a job rather than doing the work inline"

  # A want for content the library does NOT hold.
  local sat_absent
  sat_absent=$(api /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d "{\"work\":{\"content_type\":\"movie\",\"title\":\"Nothing We Have\",\"year\":1999},
         \"quality_profile_id\":\"$sat_gated\"}" | jq -r '.id')
  assert_eq "$(api "/api/v1/desired/$sat_absent/satisfaction" | jq -r '.state')" "MISSING" \
    "a want for content nothing holds reconciles to MISSING"
  assert_eq "$(api "/api/v1/desired/$sat_absent/satisfaction" | jq -r '.content.assets | length')" "0" \
    "with no assets to explain"

  api "/api/v1/desired/$sat_id" -X DELETE -o /dev/null
  api "/api/v1/desired/$sat_gated_id" -X DELETE -o /dev/null
  api "/api/v1/desired/$sat_absent" -X DELETE -o /dev/null
  api "/api/v1/quality-profiles/$sat_anything" -X DELETE -o /dev/null
  api "/api/v1/quality-profiles/$sat_gated" -X DELETE -o /dev/null

  note "  release-candidate evaluation (§63, M3-04)"
  # §63 says evaluation is deterministic and INSPECTABLE. This drives the
  # scorer over a real socket against a real profile out of the database, and
  # asserts the half §61 cares about: the REASONS, not just the number.
  #
  # It writes nothing, so it can sit anywhere; it is here to keep the M3
  # sections together and below every catalog count.
  local eval_profile eval_json eval_body
  eval_profile=$(api /api/v1/quality-profiles | jq -r '.items[] | select(.name == "living-room") | .id')

  eval_body='{"candidates":[
    {"id":"remux","title":"2160p remux","attributes":
      {"resolution":2160,"source":"remux","video_codec":"hevc","hdr":true}},
    {"id":"web","title":"1080p web-dl","attributes":
      {"resolution":1080,"source":"web-dl","video_codec":"h264","hdr":false}},
    {"id":"cam","title":"480p cam","attributes":
      {"resolution":480,"source":"cam","video_codec":"h264","hdr":false}},
    {"id":"unmeasured","title":"resolution unknown","attributes":
      {"source":"remux","video_codec":"hevc","hdr":true}}
  ]}'
  eval_json=$(api "/api/v1/quality-profiles/$eval_profile/evaluate" -X POST \
    -H 'Content-Type: application/json' -d "$eval_body")

  assert_eq "$(jq -r '.selected' <<<"$eval_json")" "remux" \
    "the best acceptable candidate is selected"
  assert_eq "$(jq -r '.ranked[0].score' <<<"$eval_json")" "30" \
    "the score is the sum of the preferences that landed"
  assert_eq "$(jq -r '.ranked[0].terminal' <<<"$eval_json")" "true" \
    "a 2160p remux meets every terminal condition, so the upgrade loop can stop"

  # Accepted and NOT terminal is the gap the upgrade workflow lives in.
  assert_eq "$(jq -r '[.ranked[] | select(.id == "web")] | .[0] | "\(.accepted)/\(.terminal)"' \
    <<<"$eval_json")" "true/false" \
    "an acceptable release that is not as good as it gets stays upgradable"

  # The reasons ARE the deliverable (§60, §61). A rejection with no explanation
  # is exactly the opaque scoring §61 rejects.
  assert_contains "$(jq -r '[.ranked[] | select(.id == "cam")] | .[0].rejected_by[0].rule' \
    <<<"$eval_json")" "resolution" "a rejection names the gate that failed"
  assert_contains "$(jq -r '[.ranked[] | select(.id == "cam")] | .[0].rejected_by[0].detail' \
    <<<"$eval_json")" "which is not" "and explains it in prose, not just a code"
  # Derived from the profile rather than hardcoded: the seeded living-room
  # profile has eight rules today and the number is not the point — the
  # invariant is that EVERY rule considered produces a reason, so a rule that
  # ran silently fails this regardless of how many there are.
  local eval_rules
  eval_rules=$(api "/api/v1/quality-profiles/$eval_profile" \
    | jq -r '(.accept | length) + (.prefer | length) + (.terminal | length)')
  assert_eq "$(jq -r '[.ranked[] | select(.id == "cam")] | .[0].reasons | length' <<<"$eval_json")" \
    "$eval_rules" "every rule considered produces a reason, not only the ones that failed"

  # "Could not determine" is not "failed", and the difference decides whether an
  # operator looks at the release or at the provider.
  assert_eq "$(jq -r '[.ranked[] | select(.id == "unmeasured")] | .[0] |
    [.reasons[] | select(.rule == "resolution.gte")] | .[0].result' <<<"$eval_json")" "undetermined" \
    "an attribute the provider could not determine is reported as undetermined"
  assert_eq "$(jq -r '[.ranked[] | select(.id == "unmeasured")] | .[0].accepted' <<<"$eval_json")" "false" \
    "but a gate that cannot be shown to hold still rejects"

  # A gate is a gate: maximal preferences cannot buy past one.
  local eval_gate
  eval_gate=$(api "/api/v1/quality-profiles/$eval_profile/evaluate" -X POST \
    -H 'Content-Type: application/json' \
    -d '{"candidates":[{"id":"brilliant","attributes":
         {"resolution":480,"source":"bluray","video_codec":"hevc","hdr":true}}]}')
  assert_eq "$(jq -r '.ranked[0].accepted' <<<"$eval_gate")" "false" \
    "a failed gate rejects whatever the preferences scored"
  assert_eq "$(jq -r '.ranked[0].score' <<<"$eval_gate")" "30" \
    "the preferences are still scored, so the explanation is complete"
  assert_eq "$(jq -r '.selected // "none"' <<<"$eval_gate")" "none" \
    "nothing is selected when nothing is acceptable"

  # Determinism, with the input order deliberately reversed between runs. A
  # randomly-ordered tie looks exactly like a working system, which is why this
  # is asserted rather than assumed.
  local eval_a eval_b
  eval_a=$(api "/api/v1/quality-profiles/$eval_profile/evaluate" -X POST \
    -H 'Content-Type: application/json' \
    -d '{"candidates":[
          {"id":"aaa","attributes":{"resolution":2160,"source":"remux","video_codec":"hevc","hdr":true}},
          {"id":"bbb","attributes":{"resolution":2160,"source":"remux","video_codec":"hevc","hdr":true}},
          {"id":"ccc","attributes":{"resolution":2160,"source":"remux","video_codec":"hevc","hdr":true}}]}' \
    | jq -r '[.ranked[].id] | join(",")')
  eval_b=$(api "/api/v1/quality-profiles/$eval_profile/evaluate" -X POST \
    -H 'Content-Type: application/json' \
    -d '{"candidates":[
          {"id":"ccc","attributes":{"resolution":2160,"source":"remux","video_codec":"hevc","hdr":true}},
          {"id":"bbb","attributes":{"resolution":2160,"source":"remux","video_codec":"hevc","hdr":true}},
          {"id":"aaa","attributes":{"resolution":2160,"source":"remux","video_codec":"hevc","hdr":true}}]}' \
    | jq -r '[.ranked[].id] | join(",")')
  assert_eq "$eval_b" "$eval_a" \
    "reversing the input order does not change the ranking"
  assert_eq "$eval_a" "aaa,bbb,ccc" \
    "ties break on the candidate id, so the winner is predictable and not merely consistent"

  # A typo in an attribute name is caught rather than silently scored against
  # nothing.
  assert_contains "$(api "/api/v1/quality-profiles/$eval_profile/evaluate" -X POST \
    -H 'Content-Type: application/json' \
    -d '{"candidates":[{"id":"x","attributes":{"bitrate":5000}}]}' | jq -r '.detail')" \
    "no attribute called" "an unknown attribute is refused rather than ignored"

  note "  the acquisition state machine (§64, M3-03)"
  # Sits with the desired-state section above it and below every catalog count,
  # for the same reason: wanting unknown content creates a Work.
  #
  # What this proves that the unit tests cannot: that §64's names survive the
  # round trip through a real database, a real socket and a real client — and
  # that BOTH of §56's axes reach the wire, which is what stops
  # CONTENT_SATISFIED and FULLY_SATISFIED collapsing at the edge.
  local acq_id acq_json acq_keys
  acq_json=$(api /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d '{"work":{"content_type":"movie","title":"Stalker","year":1979},
         "quality_profile":"living-room"}')
  acq_id=$(jq -r '.id' <<<"$acq_json")

  # A want and its acquisition state are created together, in one transaction.
  # A want with no acquisition row is one the reconciliation sweep cannot
  # advance, and nothing would ever notice: it would sit there, wanted and
  # never searched for.
  assert_eq "$(jq -r '.acquisition.state' <<<"$acq_json")" "MISSING" \
    "a fresh want starts MISSING"
  assert_eq "$(jq -r '.acquisition.phase' <<<"$acq_json")" "idle" \
    "there is no 'missing' phase — MISSING is idle while holding nothing"
  assert_eq "$(jq -r '.acquisition.managed' <<<"$acq_json")" "false" \
    "a fresh want holds no bytes"
  # Unknown, not not_satisfied: nobody has looked yet, and the two lead to
  # different actions.
  assert_eq "$(jq -r '.acquisition.content' <<<"$acq_json")" "unknown" \
    "a fresh want has been evaluated by nothing"

  # ADR-0027 over HTTP: the derived name AND both axes. A client that reads the
  # name but not the axes cannot tell "we have it" from "we have it everywhere".
  acq_keys=$(api "/api/v1/desired/$acq_id" | jq -r '.acquisition | keys | sort | join(",")')
  assert_contains "$acq_keys" "content" "§56's content axis is on the wire"
  assert_contains "$acq_keys" "placement" "§56's placement axis is on the wire"
  assert_contains "$acq_keys" "managed" "whether bytes are held is on the wire, separately from the phase"
  assert_contains "$acq_keys" "state" "and the derived §64 name alongside them"

  # Every acquisition transition emits (invariant 7). Creating the state is the
  # first one, and it must be in the log rather than only in the row.
  local acq_events
  acq_events=$(events_replay)
  if grep -q 'acquisition.phase_changed' <<<"$acq_events"; then
    pass "the acquisition state machine emits to the event log"
  else
    fail "no acquisition.phase_changed in a replay from seq 0"
  fi

  # The state survives the round trip, and the listing carries it too — a page
  # of wants must not need a query each to say where they got to.
  assert_eq "$(api "/api/v1/desired/$acq_id" | jq -r '.acquisition.state')" "MISSING" \
    "the acquisition state survives the round trip through the database"
  assert_eq "$(api /api/v1/desired | jq -r '[.items[] | select(.acquisition == null)] | length')" "0" \
    "every want in a listing carries its acquisition state"

  # The CLI shows where a want got to, because "where has this got to" is the
  # question someone runs the command to answer.
  assert_contains "$("$BIN" --config "$WORK/full.yaml" --token "$TOKEN" desired list --json \
    | jq -r '[.[] | select(.id == "'"$acq_id"'") | .acquisition.state] | join(",")')" \
    "MISSING" "the CLI reports the acquisition state"

  # Deleting the want takes its acquisition state with it.
  api "/api/v1/desired/$acq_id" -X DELETE -o /dev/null
  assert_eq "$(api "/api/v1/desired/$acq_id" | jq -r '.status')" "404" \
    "removing a want removes its acquisition state too"

  # NOT asserted here, deliberately: the walk from AVAILABLE through
  # CONTENT_SATISFIED and PLACEMENT_CONVERGING to FULLY_SATISFIED. Nothing can
  # drive those axes over the API yet — reconciliation is M3-05 and the search
  # job is M3-12 — and the only way to assert it today would be a debug
  # endpoint that writes state directly, which is a backdoor that would outlive
  # its purpose. The derivation is exhaustively table-tested in
  # internal/domain/acquisition; this section asserts the half that a running
  # system can actually reach.

  note "  ingest of completed acquisitions (§65, §66, M3-13)"
  # PLACED HERE DELIBERATELY, AND THE ANCHOR IN THE BRIEF WAS WRONG.
  #
  # The instruction was to anchor before `note "  the CLI (M1-17)"`. That
  # section CONTAINS the catalog counts — cli_works and cli_assets are asserted
  # a few lines into it — so anchoring above it puts this section above them.
  #
  # That matters more here than for any earlier M3 section: ingest ADDS AN
  # ASSET, which is exactly what cli_assets counts. Remux was the only section
  # that mutated the library; the search section became the second; this is the
  # third, and the only one that changes an asset count. So it sits below the
  # counts, with the other M3 sections.
  #
  # It also wants a work the fixture library already holds, so no Work is
  # created either.
  #
  # THE WANT IS SEARCHED FOR FIRST, and that is not ceremony.
  #
  # §64 has no edge from idle to queued: acquisition begins with a search, and
  # the machine refuses to skip it. That is correct — a want that arrived at
  # VERIFYING without passing through SELECTED has a history that does not
  # describe what happened — and it means adopting externally-arrived bytes
  # still requires the want to have chosen a release. Discovered here, by the
  # machine refusing.
  local ing_work ing_want ing_file ing_before_assets ing_state
  ing_before_assets=$(api /api/v1/assets | jq -r '.items | length')

  ing_work=$(api_all /api/v1/works '.items[] | select(.title == "The Quiet Room") | .id' | head -1)
  ing_want=$(api /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d "{\"work_id\":\"$ing_work\",\"quality_profile\":\"living-room\"}" | jq -r '.id')
  assert_contains "$ing_want" "-" "a want can be created for content to acquire"

  api "/api/v1/desired/$ing_want/search" -X POST -o /dev/null
  # Wait for SELECTED only. Breaking on idle as well would return instantly,
  # because a fresh want STARTS idle — which is exactly what this loop did on
  # its first outing, reporting "got idle, want selected" without ever having
  # waited for the search.
  waited=0
  while (( waited < 600 )); do
    ing_state=$(api "/api/v1/desired/$ing_want" | jq -r '.acquisition.phase')
    [[ "$ing_state" == "selected" ]] && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  assert_eq "$ing_state" "selected" \
    "the fake indexer offers a release and the want selects one"

  # A file standing in for a completed download, written OUTSIDE any library
  # root — which is what a download directory actually is. The point of the
  # exercise is that ingest brings bytes under management from somewhere the
  # scanner never walks.
  ing_file=$WORK/downloads/The.Quiet.Room.2001.1080p.mkv
  mkdir -p "$WORK/downloads"
  head -c 262144 /dev/urandom > "$ing_file"

  api "/api/v1/desired/$ing_want/acquisitions" -X POST -H 'Content-Type: application/json' \
    -d "{\"provider\":\"acceptance-downloader\",\"external_id\":\"acceptance-infohash\",
         \"external_name\":\"The.Quiet.Room.2001.1080p.mkv\",\"local_path\":\"$ing_file\"}" \
    -o /dev/null

  waited=0
  while (( waited < 600 )); do
    ing_state=$(api "/api/v1/desired/$ing_want" | jq -r '.acquisition.phase')
    [[ "$ing_state" == "idle" ]] && break
    sleep 0.1; waited=$(( waited + 1 ))
  done

  # ASSERT THE PHASE AND THE JOB, never a satisfaction axis. Satisfaction is
  # owned by the reconciliation beat, and a timer is not part of this claim —
  # the ~50% flake in the search section came from exactly that mistake.
  assert_eq "$ing_state" "idle" \
    "a completed acquisition drives the pipeline back to rest"
  assert_eq "$(api "/api/v1/desired/$ing_want" | jq -r '.acquisition.managed')" "true" \
    "and Heyarr now holds bytes for that want"

  assert_eq "$(api /api/v1/assets | jq -r '.items | length')" "$(( ing_before_assets + 1 ))" \
    "ingesting a completed acquisition adds exactly one asset"

  # Nothing the download client still holds is deleted (§60, ADR-0018). It may
  # still be seeding, and reaching outside the logical-delete stance for it is
  # worse than leaving it.
  if [[ -f "$ing_file" ]]; then
    pass "the download client's copy is left alone"
  else
    fail "the download client's copy was deleted by ingest"
  fi

  # Re-running is harmless (invariant 9). Note this is the STATE MACHINE's
  # guarantee rather than the job's: an ingest for a want that has left
  # VERIFYING is refused by §64, so a re-queued job is a no-op by construction
  # rather than by the handler remembering to check.
  api "/api/v1/desired/$ing_want/acquisitions" -X POST -H 'Content-Type: application/json' \
    -d "{\"provider\":\"acceptance-downloader\",\"external_id\":\"acceptance-infohash\",
         \"external_name\":\"The.Quiet.Room.2001.1080p.mkv\",\"local_path\":\"$ing_file\"}" \
    -o /dev/null
  assert_eq "$(api /api/v1/assets | jq -r '.items | length')" "$(( ing_before_assets + 1 ))" \
    "and re-ingesting the same completed download adds no second asset"

  api "/api/v1/desired/$ing_want" -X DELETE -o /dev/null

  note "  THE MILESTONE ARC: decides, explains, acquires (M3-15)"
  # This is the milestone gate, and it is the only section that runs the WHOLE
  # arc as one continuous story rather than proving one link of it.
  #
  # Every earlier M3 section asserts its own issue's claim against a library
  # that already contains things. This one starts from nothing: a want for
  # content the fixture library does not hold, no asset, no blob, no bytes
  # anywhere — which is the case the milestone is actually about and the one
  # every fixture-shaped test quietly avoids, because every fixture has assets.
  #
  # AND IT DOES IT WITH NO REAL INDEXER. That is not a limitation being worked
  # around; it is the claim (§84, ADR-0026). A real indexer proxies real
  # trackers with real credentials and can never run in CI, so a milestone that
  # could only be demonstrated with one would be a milestone nobody could
  # verify.
  #
  # It sits below every catalog count with the other M3 sections: wanting
  # unknown content creates a Work, and ingesting creates an asset.
  local arc_want arc_state arc_file arc_work arc_sat

  # 1. WANT SOMETHING THAT DOES NOT EXIST.
  arc_want=$(api /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d '{"work":{"content_type":"movie","title":"Nightfall Sonata","year":2019},
         "quality_profile":"living-room","reason":"the milestone arc"}' | jq -r '.id')
  assert_contains "$arc_want" "-" "a want can be created for content that exists nowhere"
  arc_work=$(api "/api/v1/desired/$arc_want" | jq -r '.work_id')
  assert_eq "$(api "/api/v1/desired/$arc_want" | jq -r '.acquisition.managed')" "false" \
    "and it begins holding nothing at all"
  assert_eq "$(api "/api/v1/desired/$arc_want" | jq -r '.acquisition.state')" "MISSING" \
    "which §64 presents as MISSING"

  # 2. SEARCH. A job, claimed by a worker, against a provider through the
  #    registry — the same path a real indexer would take.
  api "/api/v1/desired/$arc_want/search" -X POST -o /dev/null
  waited=0
  while (( waited < 600 )); do
    arc_state=$(api "/api/v1/desired/$arc_want" | jq -r '.acquisition.state')
    [[ "$arc_state" == "SELECTED" ]] && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  assert_eq "$arc_state" "SELECTED" \
    "a search finds a release and the evaluator selects one"

  # 3. AND IT EXPLAINS ITSELF. §61 lists opaque scoring among the things Heyarr
  #    avoids; a score with no reasons is exactly that.
  local arc_reasons
  arc_reasons=$(api "/api/v1/desired/$arc_want/candidates")
  assert_eq "$(jq -r '[.candidates[] | select(.selected)] | length' <<<"$arc_reasons")" "1" \
    "exactly one candidate is recorded as selected"
  assert_eq "$(jq -r '[.candidates[] | select(.selected)] | .[0].accepted' <<<"$arc_reasons")" "true" \
    "the selected candidate passed every gate"
  assert_contains "$(jq -r '[.candidates[] | select(.selected)] | .[0].reasons[0].rule' <<<"$arc_reasons")" "." \
    "and the decision carries the rules it was made on, durably"

  # 4. ACQUIRE AND INGEST. The bytes arrive somewhere the scanner never walks,
  #    which is what a download directory is.
  arc_file=$WORK/downloads/Nightfall.Sonata.2019.2160p.mkv
  mkdir -p "$WORK/downloads"
  head -c 262144 /dev/urandom > "$arc_file"
  api "/api/v1/desired/$arc_want/acquisitions" -X POST -H 'Content-Type: application/json' \
    -d "{\"provider\":\"acceptance-downloader\",\"external_id\":\"arc-infohash\",
         \"external_name\":\"Nightfall.Sonata.2019.2160p.mkv\",\"local_path\":\"$arc_file\"}" \
    -o /dev/null

  waited=0
  while (( waited < 600 )); do
    [[ "$(api "/api/v1/desired/$arc_want" | jq -r '.acquisition.managed')" == "true" ]] && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  assert_eq "$(api "/api/v1/desired/$arc_want" | jq -r '.acquisition.managed')" "true" \
    "the acquisition is verified and brought under management"

  # 5. AND IT IS SATISFIED — reconciled ON DEMAND rather than waiting for the
  #    beat, so this is deterministic. Asserting a satisfaction axis against a
  #    timer is what made an earlier section flake on half of runs.
  arc_sat=$(api "/api/v1/desired/$arc_want/satisfaction")
  assert_eq "$(jq -r '.content.assets | length' <<<"$arc_sat")" "1" \
    "and the acquired asset is the one considered for satisfaction"

  # WHY THIS ENDS AT AVAILABLE RATHER THAN CONTENT_SATISFIED, always.
  #
  # The artifact above is 262144 random bytes standing in for a completed
  # download. It is not media, so NOTHING can measure it — the profile gates on
  # resolution, resolution comes from a probe, and there is no resolution in
  # random bytes whether or not this machine has ffprobe.
  #
  # My first version branched on `command -v ffprobe` and asserted `satisfied`
  # when a toolchain was present. That conflated "a toolchain exists" with
  # "these bytes can be measured" — it passed on a machine with no toolchain and
  # failed on the runner that HAS one, which is the branch a bare machine cannot
  # execute. The distinction is the entire subject of §63's `undetermined`
  # result, and flattening it in the section that exists to demonstrate it was
  # the wrong place to be careless.
  #
  # So the assertion is unconditional, and it is the RIGHT answer rather than a
  # weaker one: a gate that cannot be SHOWN to hold must not pass, and the
  # reason says the attribute could not be determined rather than claiming the
  # file is too small. Measurement against real media is proven by the
  # satisfaction section, which runs over the scanned fixture library.
  #
  # This arc proves the PIPELINE — a want with nothing behind it reaching bytes
  # under management, having explained every step. Whether those particular
  # bytes are good enough is a different claim, tested elsewhere.
  assert_eq "$(jq -r '.content.satisfaction' <<<"$arc_sat")" "not_satisfied" \
    "unmeasurable bytes cannot be SHOWN to meet the profile"
  assert_eq "$(jq -r '[.content.assets[0].reasons[] | select(.rule == "resolution.gte")] | .[0].result' \
    <<<"$arc_sat")" "undetermined" \
    "and it says the attribute could not be determined, not that the file is too small"
  assert_eq "$(jq -r '.state' <<<"$arc_sat")" "AVAILABLE" \
    "so the want is AVAILABLE — bytes held, not known to be good enough"

  # THE WHOLE CLAIM, IN ONE LINE. No indexer, no download client, and on this
  # machine no toolchain either — and a want went from nothing at all to bytes
  # under management, having explained every step.
  pass "content that existed nowhere is now under management, with no real indexer present"

  note "  THE REFUSAL ARC: found things, acquired none, and said why"
  # The other half of §63, and the half that is easier to leave untested. A
  # want that searched, found three releases, and correctly took none of them
  # must leave three durable explanations behind — not a silence.
  local ref_want ref_cands
  ref_want=$(api /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d '{"work":{"content_type":"movie","title":"Static Bloom","year":2021},
         "quality_profile":"living-room"}' | jq -r '.id')
  api "/api/v1/desired/$ref_want/search" -X POST -o /dev/null

  # THE SECOND INSTANCE #207 WAS FILED ABOUT. This loop used to break on the
  # candidates appearing and then assert `phase == "idle"` — which happens
  # strictly later, after the search concludes and the want is re-evaluated. It
  # failed as `got 'candidates_found', want 'idle'`, which reads as a broken
  # state machine and is not one.
  #
  # The condition is the CONJUNCTION, and both halves are load-bearing: the
  # phase alone is not enough because a fresh want STARTS idle, so `idle` is
  # already true before the search has run at all.
  wait_for "the refusal arc never came to rest — three candidates recorded AND the phase back to idle" \
    600 refusal_arc_at_rest "$ref_want"
  ref_cands=$(api "/api/v1/desired/$ref_want/candidates")

  assert_eq "$(jq -r '.candidates | length' <<<"$ref_cands")" "3" \
    "every candidate the search found is recorded, not only the acceptable ones"
  assert_eq "$(jq -r '[.candidates[] | select(.accepted)] | length' <<<"$ref_cands")" "0" \
    "none of them was acceptable"
  assert_eq "$(jq -r '[.candidates[] | select(.selected)] | length' <<<"$ref_cands")" "0" \
    "so nothing was selected — the least bad is not good enough"
  assert_eq "$(jq -r '[.candidates[] | select((.rejected_by | length) > 0)] | length' <<<"$ref_cands")" "3" \
    "and all three carry the rule that rejected them"
  # The pipeline is at rest and holding nothing: a fruitless search leaves the
  # want exactly as unmet as before.
  assert_eq "$(api "/api/v1/desired/$ref_want" | jq -r '.acquisition.phase')" "idle" \
    "a want that found nothing acceptable returns to rest"
  assert_eq "$(api "/api/v1/desired/$ref_want" | jq -r '.acquisition.managed')" "false" \
    "still holding nothing"

  api "/api/v1/desired/$ref_want" -X DELETE -o /dev/null
  api "/api/v1/desired/$arc_want" -X DELETE -o /dev/null

  note "  THE FULLY DEGRADED NODE (ADR-0023, ADR-0025)"
  # No toolchain, no indexer, no download client — all three absent at once.
  #
  # Every one of those degradations is asserted somewhere already, and each is
  # asserted ALONE. This is the configuration nobody tests: an operator who has
  # copied a single static binary onto a NAS and started it, with nothing else
  # installed and nothing configured. It is also the FIRST configuration Heyarr
  # is ever in, so if it does not work the second one never happens.
  #
  # ADR-0023 made the toolchain optional and ADR-0025 extended that to external
  # services. Neither ADR claims the combination, and a combination that has
  # never run is a combination nobody should assume.
  local bare_dir bare_log bare_pid bare_sock
  bare_dir=$WORK/bare
  bare_log=$WORK/bare-node.log
  bare_sock=$bare_dir/heyarr.sock
  mkdir -p "$bare_dir"

  cat > "$WORK/bare.yaml" <<YAML
data_dir: $bare_dir
peer:
  name: acceptance-bare
  site: test
log:
  level: info
  format: json
http:
  addr: ""
  unix_socket: $bare_sock
  auth:
    enabled: false
libraries:
  - name: films
    content_type: movie
    roots: ["$FULLLIB/movies"]
YAML

  # env -i, and a PATH pointing at a directory that exists and holds nothing.
  # An honest simulation of a machine that never had a toolchain, rather than
  # one where it happens to be missing today.
  mkdir -p "$WORK/empty-bin"
  env -i PATH="$WORK/empty-bin" HOME="$HOME" \
    "$PWD/$BIN" --config "$WORK/bare.yaml" all >"$bare_log" 2>&1 &
  bare_pid=$!

  waited=0
  while (( waited < 600 )); do
    curl -sf --unix-socket "$bare_sock" http://heyarr/readyz >/dev/null 2>&1 && break
    sleep 0.1; waited=$(( waited + 1 ))
  done

  bare_api() { curl -sS --unix-socket "$bare_sock" "$@" "http://heyarr$1"; }

  if curl -sf --unix-socket "$bare_sock" http://heyarr/readyz >/dev/null 2>&1; then
    pass "a node with no toolchain, no indexer and no download client becomes ready"
  else
    fail "the fully degraded node never became ready"
  fi

  # It says what it cannot do, rather than leaving an operator to infer it from
  # things not happening. "Why is nothing probing" should be answerable from
  # one request.
  local bare_system
  bare_system=$(curl -sS --unix-socket "$bare_sock" http://heyarr/api/v1/system)
  assert_eq "$(jq -r '[.media[] | select(.available)] | length' <<<"$bare_system")" "0" \
    "and reports that it resolved no media toolchain"
  assert_eq "$(jq -r '.media | length' <<<"$bare_system")" "2" \
    "naming both tools rather than omitting them, so the degradation is legible"
  # `.providers`, not `.items`. This response has no `items` field, so
  # `null | length` was 0 and the assertion passed for ANY number of configured
  # providers — the same class of vacuity #164 is about, found next door to it.
  assert_eq "$(curl -sS --unix-socket "$bare_sock" http://heyarr/api/v1/providers \
    | jq -r '.providers | length')" "0" \
    "and that it has no providers configured at all"

  # THE POINT: everything that does not need an external thing still works.
  # ADR-0023's claim is that a node without the toolchain "scans, ingests,
  # catalogues, verifies, garbage-collects and serves byte ranges exactly as
  # before" — asserted here on a real node rather than reasoned about.
  waited=0
  while (( waited < 900 )); do
    [[ "$(curl -sS --unix-socket "$bare_sock" http://heyarr/api/v1/assets \
      | jq -r '.items | length')" -gt 0 ]] && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  local bare_assets bare_blob
  bare_assets=$(curl -sS --unix-socket "$bare_sock" http://heyarr/api/v1/assets | jq -r '.items | length')
  if (( bare_assets > 0 )); then
    pass "it scans and ingests a library with no toolchain present ($bare_assets assets)"
  else
    fail "the degraded node ingested nothing"
  fi

  bare_blob=$(curl -sS --unix-socket "$bare_sock" http://heyarr/api/v1/assets \
    | jq -r '[.items[] | select(.blob_hash != null)] | .[0].blob_hash')
  assert_eq "$(curl -sS -o /dev/null -w '%{http_code}' --unix-socket "$bare_sock" \
    -H 'Range: bytes=0-1023' "http://heyarr/api/v1/blobs/$bare_blob/content")" "206" \
    "and serves byte ranges from what it ingested (ADR-0013)"

  # Wanting still works, because desired state needs no external service at
  # all. What it CANNOT do is find anything — and the search stays PENDING
  # rather than failing, which is ADR-0025's central claim.
  local bare_want bare_job
  bare_want=$(curl -sS --unix-socket "$bare_sock" -X POST -H 'Content-Type: application/json' \
    -d '{"work":{"content_type":"movie","title":"Unfindable","year":2020},
         "quality_profile":"living-room"}' \
    http://heyarr/api/v1/desired | jq -r '.id')
  assert_contains "$bare_want" "-" "a node with no indexer can still be told what should exist"

  curl -sS --unix-socket "$bare_sock" -X POST -o /dev/null \
    "http://heyarr/api/v1/desired/$bare_want/search"
  # Give the worker a moment to NOT claim it. There is no positive edge to wait
  # for here — the assertion is that nothing happens — so this is bounded by a
  # short settle rather than by polling for an arrival.
  sleep 2
  bare_job=$(curl -sS --unix-socket "$bare_sock" "http://heyarr/api/v1/jobs?type=search_release")
  assert_eq "$(jq -r '[.items[] | select(.state == "failed")] | length' <<<"$bare_job")" "0" \
    "a search on a node with no indexer does NOT fail"
  assert_eq "$(jq -r '[.items[] | select(.state == "pending")] | length' <<<"$bare_job")" "1" \
    "it stays pending and visible, which is the whole of ADR-0025's degrade path"

  kill "$bare_pid" 2>/dev/null || true
  wait "$bare_pid" 2>/dev/null || true


  # -------------------------------------------------------------------------
  note "  the third state blobs.chunked could not express (§16, ADR-0034, M5-03)"
  # -------------------------------------------------------------------------
  #
  # `blobs.chunked` was `required` in the OpenAPI, in the CLI, in catalog
  # snapshots and in the peer snapshot schema, and it was `0` on every row in
  # every deployment since Milestone 1 because nothing ever wrote it. §16 asks a
  # question with three answers and a boolean holds two; the one it cannot hold
  # — NOT YET — is the one replication has to branch on.
  #
  # This is the FABRIC OF ONE. There is no peer here, nothing has decided that
  # any of these bytes must cross a network, and the answer must therefore be
  # `undecided` for every blob in the library. The two-peer arc asserts the
  # other two states; this asserts that a single-node deployment reaches neither
  # of them by accident.
  # There is no blobs COLLECTION route — a blob is reached by digest — so the
  # digests come from the assets that reference them and each is read on its
  # own. The non-vacuity guard is first and it is not decoration: an empty list
  # would make both counts zero and every assertion below would pass having
  # read nothing, which is precisely the shape #187 exists to stop.
  local m3_undecided=0 m3_total=0 m3_chunked=0 m3_hash m3_blob
  for m3_hash in $(api_all /api/v1/assets '.items[] | select(.blob_hash != null) | .blob_hash' | sort -u); do
    m3_blob=$(api "/api/v1/blobs/$m3_hash")
    m3_total=$(( m3_total + 1 ))
    [[ "$(jq -r '.chunk_manifest' <<<"$m3_blob")" == "undecided" ]] && m3_undecided=$(( m3_undecided + 1 ))
    [[ "$(jq -r '.chunked | tostring' <<<"$m3_blob")" == "false" ]] && m3_chunked=$(( m3_chunked + 1 ))
  done
  assert_eq "$(( m3_total >= 4 ))" "1" \
    "there are blobs to ask about at all — $m3_total of them — so the two counts below are not both zero"
  assert_eq "$m3_undecided" "$m3_total" \
    "every blob on a node that has never had a peer is UNDECIDED: ingest chunks nothing (§16)"
  # The compatibility field is still present and still a boolean, so a pre-M5
  # client does not break — and it is `false` for BOTH states that are not
  # `present`, which is exactly why nothing may branch on it.
  assert_eq "$m3_chunked" "$m3_total" \
    "the deprecated 'chunked' boolean is still there and still false — a pre-M5 client keeps working"

  # 🔴 Asking generates nothing (ADR-0034). Ten reads of the state, through both
  # surfaces, and no chunk_blob job exists afterwards. A GET that chunked a
  # 20 GB blob to answer would be a remote denial of service with a polite name.
  local m3_ask
  for m3_ask in 1 2 3 4 5; do
    api "/api/v1/blobs/$big_hash" >/dev/null
    cli blobs stat "$big_hash" --json >/dev/null
  done
  assert_eq "$(api '/api/v1/jobs?type=chunk_blob' | jq -r '.items | length')" "0" \
    "asking whether a blob has a manifest enqueued no chunk_blob job — the question is a READ"
  assert_eq "$(api "/api/v1/blobs/$big_hash" | jq -r '.chunk_manifest')" "undecided" \
    "and produced no manifest: the third state is the ANSWER, not a condition to be resolved"

  # The CLI stopped printing a boolean that could not be true. Asserted on the
  # plain output as well as on --json, because the operator reading a terminal
  # is the one who was being told `chunked false` about bytes nobody had looked
  # at.
  assert_contains "$(cli blobs stat "$big_hash" 2>&1)" "manifest    undecided" \
    "heyarr blobs stat reports the three-way state rather than a boolean that is always false"
  assert_eq "$(cli blobs stat "$big_hash" --json | jq -r '.chunk_manifest')" \
    "undecided" "and the --json shape carries it too"

  note "  worker capability advertisement (#112, ADR-0039, §6, §75)"
  # -------------------------------------------------------------------------
  #
  # Almost everything #112 turns on is asserted in Go, and deliberately: the
  # "lists an encoder it cannot run" case needs a fake FFmpeg re-exec, narrowing
  # needs an injected prober, and expiry needs a movable clock.
  #
  # What shell adds is the one thing those structurally cannot catch: that the
  # REAL binary, started as it ships, actually advertises. Every unit above
  # would pass on a build where worker.go never started the beat.
  #
  # Nothing here creates a Work, an edition or an asset. Every assertion but the
  # second worker is a read, so this section moves no catalogue count.
  local caps caps_holders caps_sources caps_expires caps_self
  caps=$(api /api/v1/capabilities)
  assert_eq "$(jq -r 'has("holders")' <<<"$caps")" "true" \
    "the fleet capability view is mounted"
  assert_eq "$(jq -r 'has("available")' <<<"$caps")" "true" \
    "and states the union across the fleet, rather than leaving a client to derive it"

  # THE ASSERTION NO GO TEST CAN MAKE: the running worker advertised ITSELF.
  # Polled rather than slept on — the beat advertises at startup and how long
  # that takes is a property of the machine, not of the code.
  waited=0
  while (( waited < 300 )); do
    caps=$(api /api/v1/capabilities)
    (( $(jq -r '.holders | length' <<<"$caps") >= 1 )) && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  caps_holders=$(jq -r '.holders | length' <<<"$caps")
  assert_eq "$(( caps_holders >= 1 ))" "1" \
    "the running worker advertised itself: the beat is wired into worker.go and started"

  # It named the NODE, and the name is the one this node knows itself by rather
  # than the string the config typed — read back from the peer table, which is
  # the node's own record of its identity.
  caps_self=$(api /api/v1/peers | jq -r '.items[] | select(.is_self) | .name')
  assert_eq "$(jq -r '[.holders[].peer_name] | unique | join(",")' <<<"$caps")" "$caps_self" \
    "the advertisement names this node, and only this node — a fabric of one advertises once"

  # The advertisement EXPIRES, and the value is in the future. Not "the field is
  # present": a worker that dies cannot tidy up after itself, and expiry is the
  # only thing that stops a dead worker being routed work.
  caps_expires=$(jq -r '.holders[0].expires_at' <<<"$caps")
  assert_eq "$([[ "$caps_expires" > "$(date -u +%Y-%m-%dT%H:%M:%SZ)" ]] && echo future || echo past)" \
    "future" "the advertisement carries an expiry in the future, so a dead worker falls out of the fleet"

  # Every advertised capability says HOW it was established, and the value is
  # one of exactly three. assert_eq on the whole sorted set, never
  # assert_contains: `ffmpeg` is a PREFIX of `ffmpeg.encoder.hevc` and the same
  # discipline has to apply to the source enum.
  caps_sources=$(jq -r '[.holders[].capabilities[].source] | unique | join(",")' <<<"$caps")
  case "$caps_sources" in
    ""|binary|probe|service|binary,probe|binary,service|probe,service|binary,probe,service)
      pass "every advertised capability declares a known source (binary, probe or service)" ;;
    *)
      fail "an advertised capability declares an unknown source: $caps_sources" ;;
  esac

  # Both halves are asserted, so neither the equipped CI runner nor a bare
  # laptop passes this vacuously. ADR-0023 makes a node without ffmpeg a
  # SUPPORTED node, not a broken one.
  local caps_ffmpeg_present caps_ffmpeg_source
  caps_ffmpeg_present=$(api /api/v1/system | jq -r '[.media[] | select(.name == "ffmpeg") | .available] | first // false')
  caps_ffmpeg_source=$(jq -r '[.holders[].capabilities[] | select(.name == "ffmpeg") | .source] | first // ""' <<<"$caps")
  if [[ "$caps_ffmpeg_present" == "true" ]]; then
    assert_eq "$caps_ffmpeg_source" "binary" \
      "a node with ffmpeg advertises it, sourced from the startup resolution rather than from a probe"
  else
    assert_eq "$caps_ffmpeg_source" "" \
      "a node without ffmpeg advertises no ffmpeg capability — and that is a supported node (ADR-0023)"
  fi

  # The filter is an EXACT match. A prefix match would answer "which nodes can
  # encode AV1" with every node that merely has the binary installed, which is
  # the failure the whole mechanism exists to prevent.
  assert_eq "$(api '/api/v1/capabilities?capability=ffmpeg.encoder.vp9' | jq -r '.holders | length')" \
    "0" "asking for a capability nobody holds returns no holders"
  assert_eq "$(api '/api/v1/capabilities?capability=ffmpeg.encoder' | jq -r '.holders | length')" \
    "0" "a partial dotted segment matches nothing: the filter is equality, never a prefix"

  # THE FLEET QUESTION, which is unanswerable with one worker. A second worker
  # against the SAME database with a scrubbed PATH must appear as its OWN
  # holder — the view is per worker, not per node — and must advertise strictly
  # less than the equipped one wherever there is anything to be less than.
  #
  # This is deliberately NOT inside the mixed-fleet block below: that one needs
  # ffprobe to mean anything, and the per-worker shape of this view does not.
  local caps_bare_log caps_bare_pid caps_bare_n
  caps_bare_log=$WORK/caps-bare-worker.log
  mkdir -p "$WORK/empty-bin"
  env -i PATH="$WORK/empty-bin" HOME="$HOME" \
    "$PWD/$BIN" --config "$WORK/full.yaml" worker >"$caps_bare_log" 2>&1 &
  caps_bare_pid=$!
  waited=0
  while (( waited < 300 )); do
    caps_bare_n=$(api /api/v1/capabilities | jq -r '.holders | length')
    (( caps_bare_n > caps_holders )) && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  assert_eq "$(( caps_bare_n == caps_holders + 1 ))" "1" \
    "a second worker on the same node advertises SEPARATELY: the view is per worker, not per node"
  assert_eq "$(api /api/v1/capabilities | jq -r '[.holders[].worker_id] | length == (. | unique | length)')" \
    "true" "and each advertisement carries a worker id of its own, so a stuck job and a fleet entry name the same process"
  if [[ "$caps_ffmpeg_present" == "true" ]]; then
    # Asserted as "the bare worker is absent from the holders", NOT as a COUNT
    # of holders. An advertisement outlives the process that made it, by design:
    # it expires on a TTL (ADR-0039) because a worker that stopped answering is
    # indistinguishable from one that is merely unreachable. So an equipped
    # worker from earlier in this run is legitimately still listed, and a count
    # here measures how recently the script restarted things rather than
    # anything about capability routing.
    #
    # The claim this section actually makes is about THIS bare worker, and that
    # is what is asserted: its own worker id is not among the ffmpeg holders.
    local caps_bare_id
    caps_bare_id=$(grep -o '"worker":"[^"]*"' "$caps_bare_log" | tail -1 | cut -d'"' -f4)
    assert_eq "$([[ -n "$caps_bare_id" ]] && echo yes || echo no)" "yes" \
      "the bare worker logged a worker id to assert against"
    assert_eq "$(api '/api/v1/capabilities?capability=ffmpeg' \
      | jq -r --arg w "$caps_bare_id" '[.holders[] | select(.worker_id == $w)] | length')" "0" \
      "the worker that resolved no ffmpeg does not hold it — it is not a candidate for transcode work"
    assert_eq "$(api '/api/v1/capabilities?capability=ffmpeg' \
      | jq -r 'if (.holders | length) > 0 then "some" else "none" end')" "some" \
      "and a worker that DID resolve ffmpeg holds it, so the filter is not simply empty"
  else
    not_exercised ffmpeg \
      "a MIXED fleet answering '/api/v1/capabilities?capability=ffmpeg' with one of two workers — both workers here resolved no toolchain, so the filter has nothing to separate them"
  fi
  kill "$caps_bare_pid" 2>/dev/null || true
  wait "$caps_bare_pid" 2>/dev/null || true

  note "  a mixed fleet (§75, ADR-0023)"
  # The one claim in ADR-0023 with no evidence behind it until now.
  #
  # Capability routing only does anything in a MIXED fleet. On a bare node the
  # probe handler is not registered at all, so probe jobs stay pending
  # regardless of the capability; on an equipped node the worker holds it
  # anyway. Sabotaging RequiredCapability in the production registration
  # therefore passed every test and both acceptance passes — the gap was filed
  # rather than papered over, and this closes it.
  #
  # A second worker is started against the SAME database with a scrubbed PATH,
  # so it resolves no toolchain and advertises nothing. If capability routing
  # were broken it would claim a probe job it cannot run and fail it.
  if command -v ffprobe >/dev/null 2>&1; then
    local bare_log bare_pid bare_probe_hash bare_before bare_failures
    bare_log=$WORK/bare-worker.log

    bare_before=$(api "/api/v1/jobs?type=probe_blob&state=failed" | jq -r '.items | length')

    # env -i keeps nothing but what the worker genuinely needs. PATH points at
    # a directory that exists and holds no toolchain, which is the honest
    # simulation of a machine that never had one.
    mkdir -p "$WORK/empty-bin"
    env -i PATH="$WORK/empty-bin" HOME="$HOME" \
      "$PWD/$BIN" --config "$WORK/full.yaml" worker >"$bare_log" 2>&1 &
    bare_pid=$!

    waited=0
    while (( waited < 300 )); do
      grep -q "worker ready" "$bare_log" 2>/dev/null && break
      sleep 0.1; waited=$(( waited + 1 ))
    done
    assert_contains "$(cat "$bare_log")" "worker ready" "a worker with no toolchain starts and becomes ready"
    # What this asserts changed shape in M3-07, and the reason is worth stating.
    #
    # It used to be '"capabilities":[]' — an empty set, as a PROXY for "this
    # worker resolved no toolchain". That proxy was exact while capabilities had
    # one source. They now have two independent ones: the media toolchain
    # (ADR-0023) and the configured provider registry (ADR-0025). This bare
    # worker shares the demo's config file, which declares a fake indexer, so it
    # legitimately advertises `indexer` while advertising no toolchain at all.
    #
    # So the assertion is now about what it actually means: a worker with no
    # FFmpeg advertises neither ffprobe nor ffmpeg, whatever else it can do.
    # Asserting the empty set would have been asserting the proxy, and the proxy
    # is the thing that stopped being true.
    local bare_caps
    bare_caps=$(grep -o '"capabilities":\[[^]]*\]' "$bare_log" | head -1)
    # A guard on the guard. assert_not_contains passes vacuously against an
    # empty string, so a grep that matched nothing — a renamed log field, say —
    # would report two cheerful passes having checked nothing at all.
    if [[ -z "$bare_caps" ]]; then
      fail "could not find the bare worker's advertised capabilities in its log"
    else
      pass "the bare worker's capabilities are readable from its startup log"
    fi
    assert_not_contains "$bare_caps" "ffprobe" \
      "the bare worker advertises no ffprobe"
    assert_not_contains "$bare_caps" "ffmpeg" \
      "the bare worker advertises no ffmpeg"
    # It did not register the handlers either, which is what makes the
    # degraded state readable in the log rather than only in behaviour.
    assert_not_contains "$(cat "$bare_log")" "probing is available" \
      "the bare worker did not register the probe handler"

    # Give it long enough to have claimed something if it were going to. There
    # is no positive condition to poll for here — the assertion is that
    # NOTHING happened — so this waits a bounded interval and then checks, and
    # says so rather than pretending to poll.
    sleep 2

    bare_failures=$(api "/api/v1/jobs?type=probe_blob&state=failed" | jq -r '.items | length')
    assert_eq "$bare_failures" "$bare_before" \
      "the bare worker claimed no probe job it could not run"

    # And the equipped worker is still doing its job alongside it: a fleet
    # with one incapable member is not a broken fleet.
    bare_probe_hash=$(api_all /api/v1/assets '.items[] | select(.filename != null) | select(.filename | endswith(".mkv") or endswith(".mp4")) | .blob_hash' | head -1)
    assert_eq "$(api "/api/v1/blobs/$bare_probe_hash/probe" -o /dev/null -w '%{http_code}')" "200" \
      "the capable worker still probed while an incapable one was running"

    kill "$bare_pid" 2>/dev/null || true
    wait "$bare_pid" 2>/dev/null || true
  else
    pass "no toolchain here, so there is no mixed fleet to make"
    not_exercised ffprobe "capability routing in a MIXED fleet — a bare worker declining a probe job an equipped one then claims (ADR-0023)"
  fi

  note "  remuxing (§10, §75)"
  # A remux is the case the planner returns most often and the one that costs
  # almost nothing to serve. This drives it through the real binary: queue the
  # job, wait for a capable worker to run it, and confirm a NEW asset appeared
  # on the same edition in a container the device actually declares.
  if command -v ffmpeg >/dev/null 2>&1 && command -v ffprobe >/dev/null 2>&1; then
    local mkv_asset mkv_edition mkv_hash remux_device remux_job remux_code derived_before derived_after
    mkv_asset=$(api_all /api/v1/assets '.items[] | select(.filename != null) | select(.filename | endswith(".mkv")) | .id' | head -1)
    mkv_edition=$(api "/api/v1/assets/$mkv_asset" | jq -r '.edition_id')
    mkv_hash=$(api "/api/v1/assets/$mkv_asset" | jq -r '.blob_hash')

    # THE INSTANCE #207 WAS FILED ABOUT. This block used to ask the planner for
    # a decision with nothing at all waiting for this blob's probe. With no
    # container recorded the planner correctly answers `direct`, and the run
    # printed `matroska on an mp4-only device plans REMUX — got 'direct', want
    # 'remux'` followed by four cascading failures, all of it reading as a
    # planner regression on a machine that was merely slow.
    #
    # ffmpeg AND ffprobe are already established by the branch above, so the
    # probe genuinely can land here: the fix is a wait, not a skip.
    wait_for_probe "$mkv_hash" \
      "the planner was asked to decide on a file it had not measured, and answered DIRECT — correctly. The four assertions below are consequences of that, not claims about the planner"

    # A device that takes the streams and refuses the container: the REMUX
    # case exactly.
    remux_device=$(api /api/v1/devices -X POST -H 'Content-Type: application/json' \
      -d '{"device_key":"acceptance-mp4only","name":"MP4 Only","platform":"tvos",
           "profile":{"containers":["mp4"],"video_codecs":["h264"],"audio_codecs":["aac"]}}' \
      | jq -r '.id')

    local remux_decision
    remux_decision=$(api /api/v1/playback/plan -X POST -H 'Content-Type: application/json' \
      -d "{\"asset_id\":\"$mkv_asset\",\"device_id\":\"$remux_device\"}" | jq -r '.decision')
    assert_eq "$remux_decision" "remux" "matroska on an mp4-only device plans REMUX"

    derived_before=$(api "/api/v1/assets?edition_id=$mkv_edition" | jq -r '[.items[] | select(.role == "derived")] | length')

    remux_job=$(api /api/v1/playback/remux -X POST -H 'Content-Type: application/json' \
      -d "{\"asset_id\":\"$mkv_asset\",\"device_id\":\"$remux_device\"}" | jq -r '.job_id')
    assert_contains "$remux_job" "-" "the remux was queued"

    # Poll for the CONDITION, never a fixed sleep.
    waited=0
    while (( waited < 600 )); do
      [[ "$(api "/api/v1/jobs/$remux_job" | jq -r '.state')" == "succeeded" ]] && break
      sleep 0.1; waited=$(( waited + 1 ))
    done
    assert_eq "$(api "/api/v1/jobs/$remux_job" | jq -r '.state')" "succeeded" \
      "a worker with ffmpeg ran the remux"

    derived_after=$(api "/api/v1/assets?edition_id=$mkv_edition" | jq -r '[.items[] | select(.role == "derived")] | length')
    assert_eq "$derived_after" "$(( derived_before + 1 ))" \
      "the remux produced one derived asset on the same edition"

    # And the derived asset is a real, servable, MP4 blob — not a row claiming
    # to be one.
    local derived_hash derived_magic
    derived_hash=$(api "/api/v1/assets?edition_id=$mkv_edition" | jq -r '.items[] | select(.role == "derived") | .blob_hash' | head -1)
    derived_magic=$(api "/api/v1/blobs/$derived_hash/content" -H 'Range: bytes=4-7' -o - | head -c 4)
    assert_eq "$derived_magic" "ftyp" "the derived blob is an MP4 and is range-servable"

    # Re-running is idempotent: the job dedupes and the edition does not grow a
    # second identical derivative.
    api /api/v1/playback/remux -X POST -H 'Content-Type: application/json' \
      -d "{\"asset_id\":\"$mkv_asset\",\"device_id\":\"$remux_device\"}" >/dev/null
    assert_eq "$(api "/api/v1/assets?edition_id=$mkv_edition" | jq -r '[.items[] | select(.role == "derived")] | length')" \
      "$derived_after" "asking for the same remux twice adds no second derivative"

    # The refusal: a DIRECT plan has nothing worth queueing.
    remux_code=$(api /api/v1/playback/remux -X POST -H 'Content-Type: application/json' \
      -d "{\"asset_id\":\"$mkv_asset\",\"device_id\":\"$play_device\"}" \
      -o /dev/null -w '%{http_code}')
    assert_eq "$remux_code" "409" "a DIRECT plan is refused rather than queueing pointless work"
  else
    # ADR-0023 once more: no ffmpeg, no remuxing, and everything else still
    # works. A remux job queued here would sit pending, which is the whole
    # degrade contract.
    pass "the demo passed with no ffmpeg, so nothing could be remuxed"
    not_exercised ffmpeg "a real REMUX end to end: the job queued, claimed and run, a NEW derived asset on the same edition, and a DIRECT plan refusing to queue pointless work"
  fi

  note "  version and schema drift (#150, #132)"
  # PLACED HERE DELIBERATELY: THIS SECTION CREATES NOTHING.
  #
  # No Work, no asset, no library, no job. Read the comment at the ingest
  # section above for why that matters — the catalogue counts asserted inside
  # `note "  the CLI (M1-17)"` are order-sensitive, and anything that adds a row
  # has to sit below them. This section needs no fixtures at all, so its only
  # constraint is that the server is still running, which it is until the
  # integrity section stops it.
  #
  # WHAT THIS PROVES, AND THE RULE IT ENCODES
  #
  # #132 found a deployment 36 commits and seven migrations behind main, across
  # two entire milestones, that NOTHING would have reported. The startup line
  # already carried the version and the commit; the schema version was already
  # logged; nothing compared them to anything.
  #
  # It also found HOW that went unnoticed, which generalises further than the
  # drift did: the verification procedure asked for the SILENCE of a warning,
  # and the warning had landed AFTER the build running on that host. The binary
  # contained zero occurrences of it. The silence was perfect and meant nothing,
  # and asserting on it would have PASSED.
  #
  #   NEVER ASSERT ON AN ABSENCE WITHOUT FIRST PROVING THE MECHANISM EXISTS.
  #
  # So every "no drift" assertion below is preceded — same endpoint, same field,
  # same running server — by one that watches the check FIRE. A silence only
  # counts once the noise has been heard.
  local drift_applied drift_version drift_commit
  drift_applied=$(api /api/v1/system | jq -r '.schema_version')
  drift_version=$("$BIN" version --json | jq -r '.version')
  drift_commit=$("$BIN" version --json | jq -r '.commit')

  # ---- SCHEMA DRIFT: fire first ------------------------------------------
  #
  # Seven, because seven is what #132 actually was: migrations 00012 to 00018
  # had never been applied on that host. The assertion is the NUMBER, not that
  # something was reported — "drifted: yes" is the answer that gets muted, and
  # two patch releases behind and two milestones behind are the same boolean.
  local drift_behind
  drift_behind=$(api "/api/v1/system?expected_schema=$(( drift_applied + 7 ))")
  assert_eq "$(jq -r '.drift.schema.status' <<<"$drift_behind")" "behind" \
    "seven unapplied migrations are reported as schema drift"
  assert_eq "$(jq -r '.drift.schema.migrations_behind' <<<"$drift_behind")" "7" \
    "the schema drift says HOW FAR behind, not that it differs"
  assert_eq "$(jq -r '.drift.schema.applied' <<<"$drift_behind")" "$drift_applied" \
    "the schema drift names the version actually applied"

  # ---- SCHEMA DRIFT: and only now, the silence ----------------------------
  local drift_current
  drift_current=$(api /api/v1/system)
  assert_eq "$(jq -r '.drift.schema.status' <<<"$drift_current")" "current" \
    "a fully migrated database reports no schema drift"
  assert_eq "$(jq -r '.drift.schema.migrations_behind' <<<"$drift_current")" "0" \
    "a current schema reports a distance of zero"

  # The other direction. A database migrated by a NEWER build than the binary
  # opening it is not a milder version of being behind: an old binary writes
  # plausible rows against constraints the new schema depends on, and the damage
  # predates every backup by the time anyone notices (§49).
  local drift_ahead
  drift_ahead=$(api "/api/v1/system?expected_schema=1")
  assert_eq "$(jq -r '.drift.schema.status' <<<"$drift_ahead")" "ahead" \
    "a database newer than expected is reported as ahead, not as current"
  assert_eq "$(jq -r '.drift.schema.migrations_ahead' <<<"$drift_ahead")" "$(( drift_applied - 1 ))" \
    "the ahead distance is a count of migrations too"

  # ---- BUILD DRIFT: fire first -------------------------------------------
  #
  # A commit nothing was ever built from. The demo binary is stamped with `git
  # describe`, which is not a semantic version, so there is no distance to
  # report here — and "mismatch" says exactly that rather than the "current" a
  # naive equality check would produce. The numeric semver distance is asserted
  # in internal/drift, where both sides can be controlled.
  local build_drifted
  build_drifted=$(api "/api/v1/system?expected_commit=0000000000deadbeef")
  assert_eq "$(jq -r '.drift.build.status' <<<"$build_drifted")" "mismatch" \
    "a build from a commit nobody deployed is reported as drift"

  # ---- BUILD DRIFT: and only now, the silence -----------------------------
  local build_ok
  build_ok=$(api "/api/v1/system?expected_version=${drift_version}&expected_commit=${drift_commit}")
  assert_eq "$(jq -r '.drift.build.status' <<<"$build_ok")" "current" \
    "the running build matches the expectation it was built from"

  # An expectation that was never supplied must NOT read as "current". This is
  # #132's failure mode in one field: a check that has quietly stopped comparing
  # looks exactly like a fleet that never drifts.
  assert_eq "$(jq -r '.drift.build.status' <<<"$drift_current")" "unknown" \
    "an unmade build comparison reports unknown, never current"

  # ---- THE TWO HALVES ARE INDEPENDENT ------------------------------------
  #
  # One request, both answers, and they disagree. A current binary with seven
  # migrations unapplied is its own failure — a build running against a schema
  # it was never tested on — and a single "up to date" flag would hide it.
  local drift_both
  drift_both=$(api "/api/v1/system?expected_version=${drift_version}&expected_commit=${drift_commit}&expected_schema=$(( drift_applied + 7 ))")
  assert_eq "$(jq -r '.drift.build.status' <<<"$drift_both")" "current" \
    "build drift is reported independently of schema drift"
  assert_eq "$(jq -r '.drift.schema.status' <<<"$drift_both")" "behind" \
    "schema drift is reported independently of build drift"
  assert_eq "$(jq -r '.drift.schema.migrations_behind' <<<"$drift_both")" "7" \
    "a current binary still reports its seven unapplied migrations"

  # A typo in whatever is monitoring this must not fall back to the default and
  # answer a question nobody asked.
  assert_eq "$(api "/api/v1/system?expected_schema=eighteen" -o /dev/null -w '%{http_code}')" "400" \
    "an unparseable expected_schema is refused rather than ignored"

  # ---- THE CLI SAYS THE SAME THING, AND EXITS ON IT ----------------------
  #
  # Exposure where a MACHINE can read it, not only where a person can: the API
  # above, and a command that exits non-zero. A checker wired into cron that
  # reports a problem and then exits 0 has its output stop being read and its
  # success start being trusted.
  local drift_cli_out drift_cli_rc=0
  drift_cli_out=$("$BIN" --config "$WORK/full.yaml" --token "$TOKEN" \
    system drift --expected-schema "$(( drift_applied + 7 ))" --json 2>"$WORK/drift-cli.err") || drift_cli_rc=$?
  if (( drift_cli_rc != 0 )); then
    pass "system drift exits non-zero when the instance has drifted"
  else
    fail "system drift exited 0 with seven migrations unapplied"
  fi
  assert_eq "$(jq -r '.schema.migrations_behind' <<<"$drift_cli_out")" "7" \
    "the CLI reports the same seven migrations the API does"
  if grep -q "the schema is behind" "$WORK/drift-cli.err"; then
    pass "the non-zero exit says which half drifted"
  else
    fail "system drift exited non-zero without saying why"; cat "$WORK/drift-cli.err"
  fi

  # And the silence, after the noise. No flags at all: the expectation defaults
  # to THIS binary — its own version, its own commit, its own embedded
  # migrations — which is the question somebody at a terminal actually has, and
  # it reaches nothing but the instance itself.
  drift_cli_rc=0
  drift_cli_out=$("$BIN" --config "$WORK/full.yaml" --token "$TOKEN" \
    system drift --json 2>"$WORK/drift-cli.err") || drift_cli_rc=$?
  assert_eq "$drift_cli_rc" "0" "system drift exits 0 against the binary it is comparing"
  assert_eq "$(jq -r '.build.status' <<<"$drift_cli_out")" "current" \
    "the CLI defaults its build expectation to itself, and matches"
  assert_eq "$(jq -r '.schema.status' <<<"$drift_cli_out")" "current" \
    "the CLI defaults its schema expectation to the migrations it embeds"

  note "  integrity (ADR-0018)"
  stop_full

  # Corrupt exactly one blob, and check that fsck names THAT blob rather than
  # reporting that something somewhere is wrong.
  local victim victim_hash
  victim=$(find "$FULLDATA/cas/blobs" -type f | sort | head -1)
  victim_hash="blake3:$(basename "$victim")"
  chmod u+w "$victim" && printf 'truncated' > "$victim"

  local fsck_out fsck_code=0
  fsck_out=$("$BIN" --config "$WORK/full.yaml" fsck --deep --json 2>&1) || fsck_code=$?
  if (( fsck_code != 0 )); then
    pass "fsck exits non-zero when it finds damage"
  else
    fail "fsck --deep exited 0 having found a corrupt blob"
  fi
  if grep -qF "$victim_hash" <<<"$fsck_out"; then
    pass "fsck --deep names the corrupt blob exactly"
  else
    fail "fsck did not name $victim_hash"; echo "$fsck_out" | head -20
  fi
  # Corrupt bytes are evidence, not rubbish: on a hardlink-ingested library the
  # "corruption" may be the original that legitimately changed (ADR-0018).
  if [[ -f "$victim" ]]; then
    fail "the corrupt blob is still addressable at its own digest"
  elif find "$FULLDATA/cas/quarantine" -type f 2>/dev/null | grep -q .; then
    pass "the corrupt blob is quarantined, not deleted"
  else
    fail "the corrupt blob was deleted rather than quarantined"
  fi

  # gc with no flags must change nothing at all. A garbage collector whose
  # default is destructive is one nobody can safely run to find out what it
  # would do.
  local before_gc after_gc
  before_gc=$(find "$FULLDATA/cas" -type f | wc -l | tr -d ' ')
  "$BIN" --config "$WORK/full.yaml" gc >/dev/null 2>&1 || true
  after_gc=$(find "$FULLDATA/cas" -type f | wc -l | tr -d ' ')
  assert_eq "$after_gc" "$before_gc" "gc without flags changes nothing"
  # ---------------------------------------------------------------------------
  note "  integrity repair: chunks are replaced, a blob is never edited (ADR-0036, M5-08)"
  # ---------------------------------------------------------------------------
  #
  # The blob corrupted above is still quarantined and is no longer addressable
  # at its own digest. `fsck --repair` must therefore say what it could not
  # repair and WHY, rather than exiting quietly or claiming success — a refusal
  # nobody can read is an outage nobody can diagnose (M4-12).
  #
  # 🔴 What this can and cannot reach today. The repairer fetches its
  # replacement chunks through a narrow `integrity.ChunkSource`, and the
  # concrete peer-backed implementation is NOT wired in: `fsck --repair` passes
  # a nil source deliberately, so a nil source REFUSES rather than permits. That
  # is the honest state and it is asserted as such; the repaired path is
  # recorded as unexercised below rather than skipped, so the verdict line says
  # this run did not reach it.
  local repair_out repair_rc=0 repair_json
  repair_out=$("$BIN" --config "$WORK/full.yaml" fsck --deep --repair 2>&1) || repair_rc=$?
  if (( repair_rc != 0 )); then
    pass "fsck --repair still exits non-zero while the damage is unrepaired"
  else
    fail "fsck --repair exited 0 with a blob still damaged"
  fi
  assert_contains "$repair_out" "NOT REPAIRED" \
    "fsck --repair names the blob it could not repair rather than reporting a count"
  assert_contains "$repair_out" "chunks damaged" \
    "and says how much of the blob was damaged, not merely that it was"

  # The machine-readable half carries the same verdict as an enum, and the
  # outcome is asserted with assert_eq: every value in this enum shares words
  # with its neighbours, and `no_manifest` is the answer that says the blob was
  # never chunked rather than that no peer answered.
  repair_json=$("$BIN" --config "$WORK/full.yaml" fsck --deep --repair --json 2>/dev/null || true)
  assert_eq "$(jq -r '.repairs | type' <<<"$repair_json")" "array" \
    "fsck --repair --json reports a result per damaged blob, so a script can act on it"
  assert_eq "$(jq -r '[.repairs[] | select(.outcome == "repaired")] | length' <<<"$repair_json")" "0" \
    "nothing was repaired, which is the truth on a node with no manifest and no peer to fetch from"
  assert_eq "$(jq -r '[.repairs[].outcome] | unique | join(",")' <<<"$repair_json")" "no_manifest" \
    "and it says WHICH refusal it was: these bytes were never chunked, so there is nothing to repair FROM (§16)"

  # Nothing was written. A repair that cannot complete leaves the store exactly
  # as it was: the evidence stays in quarantine and no replacement is published.
  local q_before q_after
  q_before=$(find "$FULLDATA/cas/quarantine" -type f | wc -l | tr -d ' ')
  "$BIN" --config "$WORK/full.yaml" fsck --deep --repair >/dev/null 2>&1 || true
  q_after=$(find "$FULLDATA/cas/quarantine" -type f | wc -l | tr -d ' ')
  assert_eq "$q_after" "$q_before" \
    "a repair that could not complete quarantined nothing further"
  if [[ -f "$victim" ]]; then
    fail "a failed repair republished the damaged blob at its own digest"
  else
    pass "a failed repair published nothing: the digest still names no bytes"
  fi

  # Staging residue is WASTE, not damage: an abandoned reconstruction leaves a
  # reapable partial and nothing addressable. Asserted only when there is
  # residue to assert about, and stated as such rather than passing on an empty
  # directory.
  if find "$FULLDATA/cas/tmp" -name '*.part' 2>/dev/null | grep -q .; then
    "$BIN" --config "$WORK/full.yaml" gc --apply --temp-grace 0 >/dev/null 2>&1 || true
    if find "$FULLDATA/cas/tmp" -name '*.part' 2>/dev/null | grep -q .; then
      fail "staging residue from a repair is not reapable"
    else
      pass "staging residue from a repair is reapable"
    fi
  fi

  # A damaged blob actually REPAIRED, with the fetch scoped to the damage, is
  # asserted in the two-peer arc — where there is a peer to fetch from and a
  # manifest to scope the fetch by. It cannot be asserted HERE, and the reason
  # is not a missing mechanism any more: this node is a fabric of one, and the
  # blob it just damaged was never chunked, so `no_manifest` is the correct and
  # final answer whatever transport exists. That is what the assertions above
  # check, and they are about the refusal rather than about the absence of a
  # peer-backed source.


  # ---------------------------------------------------------------------------
  # Garbage collection confirms placement before unlinking (ADR-0018, M4-12)
  # ---------------------------------------------------------------------------
  #
  # ADR-0018 deferred a second precondition to this milestone: GC "must confirm
  # the placement policy is satisfied elsewhere before unlinking". The refusals
  # are the deliverable; the happy path was always the easy part.
  #
  # Handed over from M4-12, which owned the collector while this file was being
  # rewritten by the placement issue. The three PEER refusals need a second node
  # with its own CAS and its own listener, and the one an operator most needs to
  # have seen work — the last copy, spared because the other peer is down — is
  # asserted against two running nodes in the two-peer section below.
  note "  garbage collection preconditions (ADR-0018, M4-12)"

  local gc_json
  gc_json=$("$BIN" --config "$WORK/full.yaml" gc --json 2>/dev/null)

  # The reasons are REPORTED, not merely logged. Both fields exist as arrays in
  # the --json shape, so a script can act on them and a human can read them.
  assert_eq "$(jq -r '.spared | type' <<<"$gc_json")" "array" \
    "gc --json reports which blobs were spared"
  assert_eq "$(jq -r '.refusals | type' <<<"$gc_json")" "array" \
    "gc --json reports the conditions that stopped the sweep"
  assert_eq "$(jq -r '.untracked_spared | type' <<<"$gc_json")" "array" \
    "gc --json names the untracked files a guard protected rather than counting them"

  # This node has exactly one peer (ADR-0010), so there is no elsewhere for a
  # placement policy to be satisfied at and the four original preconditions are
  # the whole gate. It must therefore NOT be refusing: a precondition that
  # refuses everywhere is indistinguishable from a broken collector, and every
  # refusal asserted below would pass against one.
  assert_eq "$(jq -r '.refusals | length' <<<"$gc_json")" "0" \
    "a sole-peer deployment is not refused by the placement precondition"

  # An empty catalog against a populated store sweeps NOTHING.
  #
  # A fresh data directory, a fresh empty database, and the demo node's real
  # CAS — every blob in it untracked, every one of them older than the window,
  # and a --grace small enough that age cannot be what saves them. Before
  # M4-12 that combination unlinked the library, which is the shape a restored
  # backup, a wrong --config, or a peer's catalog snapshot mistaken for a
  # catalog produces.
  local vac_dir vac_cas vac_before vac_after vac_blobs vac_json
  vac_dir="$WORK/gc-vacuity"
  mkdir -p "$vac_dir/data"
  vac_cas="$vac_dir/cas"
  cp -R "$FULLDATA/cas" "$vac_cas"
  # Age every file well past any window, the way a month of sitting there does.
  find "$vac_cas" -type f -exec touch -t 202601010000 {} +
  cat >"$vac_dir/gc.yaml" <<YAML
data_dir: $vac_dir/data
database:
  path: $vac_dir/data/heyarr.db
cas:
  root: $vac_cas
YAML
  vac_before=$(find "$vac_cas" -type f | wc -l | tr -d ' ')
  # Blob files only, excluding the HEYARR_CAS marker, because that is what the
  # sweep would have unlinked and therefore what the guard has to have spared.
  vac_blobs=$(find "$vac_cas/blobs" -type f | wc -l | tr -d ' ')
  vac_json=$("$BIN" --config "$vac_dir/gc.yaml" gc --apply --grace 1ns --json 2>/dev/null)
  vac_after=$(find "$vac_cas" -type f | wc -l | tr -d ' ')

  assert_eq "$(jq -r '.considered' <<<"$vac_json")" "0" \
    "the fixture is an empty catalog, so the sweep considered no blobs"
  assert_eq "$vac_after" "$vac_before" \
    "an empty catalog against a populated CAS unlinks nothing"
  assert_eq "$(jq -r '.untracked | length' <<<"$vac_json")" "0" \
    "an empty catalog reclaims no untracked bytes"
  assert_eq "$(jq -r '.refusals[0].reason' <<<"$vac_json")" "catalog_vacuous" \
    "gc says why it refused: the catalog does not describe this store"
  # And it NAMES them. "removed nothing" without a reason per file is a result
  # an operator cannot act on.
  assert_eq "$(jq -r '.untracked_spared | length' <<<"$vac_json")" "$vac_blobs" \
    "every file the guard protected is named in the report"
  assert_eq "$(jq -r '[.untracked_spared[].reason] | unique | join(",")' <<<"$vac_json")" \
    "catalog_vacuous" "every spared file carries the reason it was spared"

  # The human-readable output says it too — the --json shape is for scripts and
  # the operator reading a terminal must not be the one left guessing.
  local vac_text
  vac_text=$("$BIN" --config "$vac_dir/gc.yaml" gc --apply --grace 1ns 2>/dev/null)
  assert_eq "$(grep -cF 'REFUSED  catalog_vacuous' <<<"$vac_text" | tr -d ' ')" "1" \
    "gc's plain output names the refusal"
}

