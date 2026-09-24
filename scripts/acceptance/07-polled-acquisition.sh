# shellcheck shell=bash
# A section of scripts/acceptance.sh: sourced by it, in order, never run alone.
# ---------------------------------------------------------------------------
# THE POLLED ACQUISITION ARC: a grab that SUCCEEDS, and the poll that notices
# (§58, §61, §65, #247)
# ---------------------------------------------------------------------------
#
# Every acquisition arc above this point ends at one of two places: a want that
# reaches SELECTED and stops, or bytes adopted through
# `POST /desired/{id}/acquisitions` — the endpoint documented as the path for
# "a transfer that finished before Heyarr knew about it, something fetched by
# hand". §65 keeps several ways for bytes to arrive and the ORDINARY one was
# not among them.
#
# # What was actually missing, which was not the fake client
#
# #247 read as "the demo cannot construct a working download client", and #259
# built one. That was necessary and it was not sufficient: `poll_downloads` was
# declared, its handler was registered, its dedupe key was declared beside it,
# and NOTHING ENQUEUED IT. A transfer handed to a client was never asked about
# again, so no arc could get past QUEUED on a running node however the client
# was configured.
#
# That is the same defect #164 closed for `provider_health`, and finding it
# twice is why scripts/claims.list exists. The beat is in
# internal/controller/downloadbeat.go; this section is what proves it runs.
#
# # Why its own node
#
# The full demo node configures a download client at port 9 that refuses
# connections ON PURPOSE — ADR-0025's claim that a client which is down is a
# degraded state rather than a failure, and the assertion that a want keeps its
# selection to retry. Giving that node a working client would delete the
# section that proves the opposite. So this is a second node with its own
# library, its own catalogue and its own downloads directory, which also keeps
# it out of every catalogue count asserted elsewhere.
polled_acquisition_demo() {
  local root="$WORK/polled" lib
  lib="$root/library"
  mkdir -p "$lib/movies" "$root/downloads" "$root/data" "$root/fixture"

  # A REAL HTTP server serving KNOWN bytes, so the plain-HTTP download client
  # (§58, M11) can be driven end to end with no external daemon — Heyarr IS the
  # client. It binds 127.0.0.1:0 and reports the port it got, the same "never
  # guess a port" discipline the peer surface uses, and the bytes are
  # deterministic so a digest can be asserted.
  python3 -c "import sys; sys.stdout.buffer.write(b'beacon-hill-http-payload-'*700)" > "$root/fixture/beacon.mkv"
  cat > "$WORK/httpfixture.py" <<'PY'
import http.server, socketserver, sys, os
os.chdir(sys.argv[1])
with socketserver.TCPServer(("127.0.0.1", 0), http.server.SimpleHTTPRequestHandler) as httpd:
    with open(sys.argv[2], "w") as portfile:
        portfile.write(str(httpd.server_address[1]))
    httpd.serve_forever()
PY
  python3 "$WORK/httpfixture.py" "$root/fixture" "$WORK/httpfixture.port" >/dev/null 2>&1 &
  PEER_PIDS+=($!)
  local http_port pw=0
  while (( pw < 200 )); do [[ -s "$WORK/httpfixture.port" ]] && break; sleep 0.05; pw=$(( pw + 1 )); done
  http_port=$(cat "$WORK/httpfixture.port" 2>/dev/null || true)
  if [[ -z "$http_port" ]]; then fail "polled: the http fixture server never reported a port"; return 1; fi

  # An EMPTY library. The want below is for content nothing holds, which is the
  # case the ordinary arc is about: if the library already had it there would be
  # nothing to acquire and the whole section would assert on a shortcut.
  cat > "$WORK/polled.yaml" <<YAML
data_dir: $root/data
log:
  level: info
  format: json
http:
  addr: ""
libraries:
  - name: films
    content_type: movie
    roots: ["$lib/movies"]
providers:
  - name: polled-indexer
    type: fake
    capabilities: [indexer]
    offers:
      - title: Harbour Lights
        candidates:
          - id: hl-1080-web
            fetch: magnet:?xt=urn:btih:hl-1080-web
            title: Harbour Lights 1080p web-dl
            attributes:
              resolution: 1080
              source: web-dl
              video_codec: h264
      # A release whose source is a direct http(s) URL — the §58 plain-HTTP
      # case. The fetch points at the fixture server above, so the plain-HTTP
      # client fetches REAL bytes over a real connection.
      - title: Beacon Hill
        candidates:
          - id: bh-1080-web
            fetch: http://127.0.0.1:$http_port/beacon.mkv
            title: Beacon Hill 1080p web-dl
            attributes:
              resolution: 1080
              source: web-dl
              video_codec: h264
  # The plain-HTTP download client (§58, M11). Listed BEFORE the fake so a grab
  # tries it first: it takes the http(s) source and REFUSES a magnet, so the
  # magnet want above still falls through to the fake — the two clients compose
  # rather than compete (registry.Grab).
  - name: polled-http
    type: http
    path_map:
      - remote: /downloads/complete
        local: $root/downloads
  # THE POINT OF THIS SECTION. A fake declaring the DOWNLOAD capability, which
  # downloads.Constructor turns into a client that writes real bytes into the
  # local side of its path map. The content is derived from the source, so the
  # same release always produces the same blob.
  - name: polled-downloads
    type: fake
    capabilities: [download]
    path_map:
      - remote: /downloads/complete
        local: $root/downloads
YAML

  local sock="$root/data/heyarr.sock" log="$root/node.log" token
  token=$("$BIN" --config "$WORK/polled.yaml" token create acceptance --scopes admin --json | jq -r .token)
  "$BIN" --config "$WORK/polled.yaml" all >>"$log" 2>&1 &
  PEER_PIDS+=($!)

  pa_api() { curl -sS --unix-socket "$sock" -H "Authorization: Bearer $token" "${@:2}" "http://heyarr$1"; }

  local waited=0
  while (( waited < 900 )); do
    curl -sf --unix-socket "$sock" http://heyarr/readyz >/dev/null 2>&1 && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if (( waited >= 900 )); then fail "polled: the node never became ready"; tail -20 "$log"; return 1; fi

  # The beat says so on startup, and it says so BEFORE anything is asserted
  # about its effects. A node that decided it had no download client would
  # produce every failure below with no explanation of why.
  assert_eq "$(grep -c '"msg":"download poll beat started"' "$log" || true)" "1" \
    "the node started a download poll beat, because a download client is configured (#247)"

  # -------------------------------------------------------------------------
  note "  a want for content nothing holds, acquired the ORDINARY way (§65)"
  # -------------------------------------------------------------------------
  local pa_profile pa_want pa_state
  pa_profile=$(pa_api /api/v1/quality-profiles -X POST -H 'Content-Type: application/json' \
    -d '{"name":"polled-anything","description":"accepts whatever exists"}' | jq -r '.name')
  assert_eq "$pa_profile" "polled-anything" "a profile that gates on nothing, so random bytes are acceptable"

  pa_want=$(pa_api /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d '{"work":{"content_type":"movie","title":"Harbour Lights","year":2016},
         "quality_profile":"polled-anything","reason":"the polled arc"}' | jq -r '.id')
  assert_contains "$pa_want" "-" "a want for content this node holds nothing of"
  assert_eq "$(pa_api "/api/v1/desired/$pa_want" | jq -r '.acquisition.managed')" "false" \
    "and it begins holding nothing at all"

  # Searched ON DEMAND rather than waiting for the beat, which is the same
  # reason the satisfaction section reconciles on demand: asserting against a
  # timer is what made an earlier section flake on half of runs.
  #
  # It is also thirty seconds of the demo's budget. The search beat's cadence
  # is #130's business and is asserted where that issue lives; this section is
  # about what happens AFTER a selection, and waiting out somebody else's
  # cadence to get there proves nothing it does not already prove.
  pa_api "/api/v1/desired/$pa_want/search" -X POST -o /dev/null

  waited=0
  while (( waited < 900 )); do
    pa_state=$(pa_api "/api/v1/desired/$pa_want" | jq -r '.acquisition.state')
    [[ "$pa_state" == "SELECTED" || "$pa_state" == "QUEUED" || "$pa_state" == "VERIFYING" ]] && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  assert_eq "$(( waited < 900 ))" "1" \
    "the want reaches a selection with no real indexer anywhere (§64, ADR-0026)"

  # -------------------------------------------------------------------------
  note "  🔴 the grab SUCCEEDS, and the poll notices (#247, #225)"
  # -------------------------------------------------------------------------
  #
  # THE ASSERTION THIS SECTION EXISTS FOR. Everywhere else the demo asserts a
  # grab was QUEUED, because the only client configured refuses connections. A
  # queued job is the edge existing; a SUCCEEDED one is the edge working.
  pa_grab_succeeded() {
    [[ "$(pa_api "/api/v1/jobs?type=grab_release" |
      jq -r '[.items[] | select(.state == "succeeded")] | length > 0')" == "true" ]]
  }
  wait_for "no grab_release job ever succeeded — SELECTED never became QUEUED against a client that answers" \
    900 pa_grab_succeeded
  pass "a grab_release job succeeded — the release was handed to a download client that took it"

  # And the poll pass ran. Asserted separately from its effect, because a
  # transfer that completed for some other reason would satisfy the ingest
  # assertion below while saying nothing about the beat.
  pa_poll_succeeded() {
    [[ "$(pa_api "/api/v1/jobs?type=poll_downloads" |
      jq -r '[.items[] | select(.state == "succeeded")] | length > 0')" == "true" ]]
  }
  wait_for "no poll_downloads job ever succeeded — nothing observed the transfer, which is #247 exactly" \
    900 pa_poll_succeeded
  pass "a poll_downloads job succeeded — something asked the client what it had finished"

  # -------------------------------------------------------------------------
  note "  and the bytes arrive under management, with nobody adopting them"
  # -------------------------------------------------------------------------
  pa_managed() {
    [[ "$(pa_api "/api/v1/desired/$pa_want" | jq -r '.acquisition.managed')" == "true" ]]
  }
  wait_for "the want never came under management — the polled arc stopped somewhere after the transfer completed" \
    1200 pa_managed
  assert_eq "$(pa_api "/api/v1/desired/$pa_want" | jq -r '.acquisition.managed')" "true" \
    "the acquisition is verified and brought under management, reached WITHOUT the by-hand endpoint (§65)"

  # The adoption endpoint was never called on this node, which is what makes
  # the claim above about the ORDINARY route rather than about ingest in
  # general. Asserted from the access log, positively: this node served no POST
  # to /acquisitions at all.
  assert_eq "$(grep -c '"path":"/api/v1/desired/[^"]*/acquisitions"' "$log" || true)" "0" \
    "and nothing adopted anything by hand — the by-hand endpoint was never called on this node"

  local pa_sat
  pa_sat=$(pa_api "/api/v1/desired/$pa_want/satisfaction")
  assert_eq "$(jq -r '.content.assets | length' <<<"$pa_sat")" "1" \
    "the acquired asset is the one considered for satisfaction"

  # -------------------------------------------------------------------------
  note "  a second want fetched over PLAIN HTTP (§58, M11)"
  # -------------------------------------------------------------------------
  # The other §58 client: the release's source is a direct URL and HEYARR is the
  # client. The grab tries the http client first; it takes the http source and
  # fetches the fixture bytes, while the magnet want above went to the fake — the
  # two compose (registry.Grab). What is proven is a REAL caller (the grab)
  # driving a REAL fetch, asserted by byte-identity with what the server served —
  # the same honest bar the OpenSubsonic and OPDS scenes hold to.
  local pa_http_want
  pa_http_want=$(pa_api /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d '{"work":{"content_type":"movie","title":"Beacon Hill","year":2016},
         "quality_profile":"polled-anything","reason":"the plain-http arc"}' | jq -r '.id')
  assert_contains "$pa_http_want" "-" "a want whose release is a direct http link"
  pa_api "/api/v1/desired/$pa_http_want/search" -X POST -o /dev/null

  # The http client renames its .part to beacon.mkv only when the fetch is
  # complete, so the file appearing IS the download finishing — and it appears
  # only if the grab routed to the http client, because nothing else writes this
  # name into the download directory.
  pa_http_fetched() { [[ -f "$root/downloads/beacon.mkv" ]]; }
  wait_for "beacon.mkv never appeared — the grab did not reach the http client, or the fetch failed" \
    1200 pa_http_fetched
  pass "a grab routed to the plain-HTTP client, which fetched the release over a real connection (§58)"

  local http_got http_want_sha
  http_got=$(shasum -a 256 "$root/downloads/beacon.mkv" | cut -d" " -f1)
  http_want_sha=$(shasum -a 256 "$root/fixture/beacon.mkv" | cut -d" " -f1)
  assert_eq "$http_got" "$http_want_sha" \
    "the bytes Heyarr fetched over plain HTTP are byte-identical to what the server served"

  not_exercised daemon_download_clients \
    "qBittorrent/SABnzbd/NZBGet download clients — each needs a LIVE daemon, so a conformance test against a mock would be a mechanism with no real caller; deferred to a daemon-in-the-loop harness (tracked pending: acquires-over-daemon-clients)"

  local p
  for p in "${PEER_PIDS[@]:-}"; do kill -TERM "$p" 2>/dev/null || true; done
  for p in "${PEER_PIDS[@]:-}"; do wait "$p" 2>/dev/null || true; done
  PEER_PIDS=()
}

