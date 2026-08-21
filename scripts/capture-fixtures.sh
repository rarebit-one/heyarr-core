#!/usr/bin/env bash
# Capture provider fixtures from a REAL instance (ADR-0026).
#
# A real indexer proxies real trackers with real credentials. It is not
# reproducible, it is not ours, and it can never run in CI. Recorded fixtures
# are therefore the PRIMARY test strategy for it rather than a stand-in for
# one — which means a hand-written fixture does not approximate reality, it
# replaces it permanently.
#
# So this script exists to make a real capture cheap enough that nobody is
# tempted to invent one.
#
# ---------------------------------------------------------------------------
# THIS WRITES TO A PUBLIC REPOSITORY, AND GIT HISTORY IS PERMANENT
# ---------------------------------------------------------------------------
#
# Every capture is redacted HERE, before it is written. The scanner in
# internal/providers/fixtures runs over the committed corpus in CI as a second
# line — it is there to catch the time this one is wrong, not to replace it.
#
# Redaction is not a tidiness pass. A tracker passkey identifies a person to a
# private tracker, and rotating it afterwards does not remove it from history.
#
# Usage:
#   scripts/capture-fixtures.sh prowlarr     http://host:9696 <api-key>
#   scripts/capture-fixtures.sh transmission http://host:9091 <user> <pass>
#
# Nothing here is committed automatically. Read what it wrote, then commit it.

set -euo pipefail

CORPUS=${CORPUS:-internal/providers/fixtures/testdata}
REDACTED=REDACTED

die() { printf '\033[31m%s\033[0m\n' "$*" >&2; exit 1; }
note() { printf '\033[1m%s\033[0m\n' "$*"; }
ok()   { printf '  \033[32mcaptured\033[0m %s\n' "$*"; }

command -v curl >/dev/null || die "curl is required"
command -v jq   >/dev/null || die "jq is required"

# redact removes every credential shape this corpus can contain.
#
# Applied to the whole recorded exchange — path, headers and body alike —
# because a key travels in all three and a redactor that only cleaned the body
# would be the most dangerous kind of half-working.
redact() {
  sed -E \
    -e "s/(api[_-]?key=)[0-9a-fA-F]{8,}/\1$REDACTED/gI" \
    -e "s/(\"X-Api-Key\"[[:space:]]*:[[:space:]]*\")[^\"]+/\1$REDACTED/gI" \
    -e "s/(\"X-Transmission-Session-Id\"[[:space:]]*:[[:space:]]*\")[^\"]+/\1$REDACTED/gI" \
    -e "s|([a-zA-Z][a-zA-Z0-9+.-]*://)[^/[:space:]\"']*:[^/[:space:]\"'@]+@|\1$REDACTED:$REDACTED@|g" \
    -e "s|(/announce[^\"' ]*[?\&](passkey\|pid\|torrent_pass\|authkey)=)[0-9a-zA-Z]+|\1$REDACTED|gI" \
    -e "s|(/announce/)[0-9a-zA-Z]{16,}|\1$REDACTED|gI" \
    -e "s/(\"Authorization\"[[:space:]]*:[[:space:]]*\")[^\"]+/\1$REDACTED/gI"
}

# write_exchange records one request/response pair with its provenance.
write_exchange() {
  local service=$1 name=$2 method=$3 path=$4 status=$5 body=$6 headers=$7 version=$8
  local dir="$CORPUS/$service"
  mkdir -p "$dir"

  jq -n \
    --arg name "$name" \
    --arg service "$service" \
    --arg version "$version" \
    --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg proc "scripts/capture-fixtures.sh $service <endpoint> <credentials>" \
    --arg method "$method" \
    --arg path "$path" \
    --argjson status "$status" \
    --arg body "$body" \
    --argjson headers "$headers" \
    '{
      name: $name,
      provenance: {
        origin: "captured",
        service: $service,
        version: $version,
        captured_at: $at,
        procedure: $proc
      },
      request:  { method: $method, path: $path },
      response: { status: $status, headers: $headers, body: $body }
    }' | redact > "$dir/$name.json"

  ok "$service/$name.json"
}

capture_prowlarr() {
  local base=$1 key=$2
  [[ -n "$key" ]] || die "prowlarr needs an api key"

  note "prowlarr — $base"

  local version
  version=$(curl -sS -H "X-Api-Key: $key" "$base/api/v1/system/status" \
    | jq -r '.version // "unknown"') || die "could not reach prowlarr"
  [[ "$version" != "unknown" ]] || die "prowlarr did not report a version; is the key right?"
  note "  version $version"

  # A search WITH results. The query is deliberately something an indexer will
  # answer for — pass QUERY=... to choose your own.
  local q=${QUERY:-ubuntu}
  local path="/api/v1/search?query=$q&type=search"
  local body status
  body=$(curl -sS -H "X-Api-Key: $key" "$base$path")
  status=$(curl -sS -o /dev/null -w '%{http_code}' -H "X-Api-Key: $key" "$base$path")
  write_exchange prowlarr search-with-results GET "$path" "$status" "$body" \
    '{"Content-Type":"application/json"}' "$version"

  # A search with ZERO results. §63 can only report an empty candidate set if
  # something ever returns one.
  local empty="/api/v1/search?query=zzzzzzzz-no-such-release-zzzzzzzz&type=search"
  body=$(curl -sS -H "X-Api-Key: $key" "$base$empty")
  write_exchange prowlarr search-empty GET "$empty" 200 "$body" \
    '{"Content-Type":"application/json"}' "$version"

  # A 401. Provoked deliberately rather than waited for: this is the response
  # that decides whether a bad key is reported as a configuration problem or
  # retried forever.
  body=$(curl -sS -H "X-Api-Key: definitely-not-a-valid-key" "$base$path" || true)
  status=$(curl -sS -o /dev/null -w '%{http_code}' \
    -H "X-Api-Key: definitely-not-a-valid-key" "$base$path" || true)
  write_exchange prowlarr unauthorised GET "$path" "${status:-401}" "$body" \
    '{"Content-Type":"application/json"}' "$version"

  cat <<'MSG'

  Still missing, and worth adding by hand if your instance will produce them:
    rate-limited   a 429 with Retry-After
    malformed      a truncated or non-JSON body
    proxy-error    an HTML error page from a reverse proxy

  Those are the responses that actually occur at 03:00. A fixture written by
  hand for one of these is legitimate — set origin to "synthesised" and say in
  the note why a real one could not be obtained.

  ALSO WORTH CAPTURING: a search whose results OMIT a field the client reads
  — no resolution, no source. Attribute extraction happens inside the provider,
  so §63 can only report "undetermined" for something it never received, and
  that path is unreachable without a fixture that leaves the field out.
MSG
}

capture_transmission() {
  local base=$1 user=${2:-} pass=${3:-}
  note "transmission — $base"

  # An empty array under `set -u` is an ERROR in bash 3.2, which is what macOS
  # ships — and the no-auth case is not an edge case here: Transmission's RPC
  # is commonly left unauthenticated behind a whitelist, so this is the FIRST
  # configuration the script meets. Guarding every expansion with
  # ${auth[@]+"${auth[@]}"} would work and would have to be right in five
  # places; one variable that is either empty or a complete flag pair cannot be
  # got wrong in only four of them.
  local auth=()
  if [[ -n "$user" ]]; then
    auth=(-u "$user:$pass")
  fi
  # curl_rpc wraps the repetition, so the auth expansion exists once.
  curl_rpc() { curl -sS ${auth[@]+"${auth[@]}"} "$@"; }

  # THE handshake. Transmission answers the first RPC call with 409 and a
  # session id that must be replayed. A client that treats 409 as an error
  # works against every hand-written fixture and fails against every real
  # instance — so this is the single most important exchange in the corpus.
  local headers status
  headers=$(curl_rpc -D - -o /dev/null -X POST \
    -H 'Content-Type: application/json' \
    -d '{"method":"session-get"}' "$base/transmission/rpc" || true)
  status=$(printf '%s' "$headers" | head -1 | awk '{print $2}')
  local sid
  sid=$(printf '%s' "$headers" | grep -i '^X-Transmission-Session-Id:' \
    | tr -d '\r' | awk '{print $2}')
  [[ -n "$sid" ]] || die "transmission did not issue a session id; is it reachable?"

  # Held back until the version is known. This exchange is the response that
  # refuses to tell you anything until the session id is replayed, so it cannot
  # carry a version of its own — and writing "unknown" into provenance would
  # make the one required fact a placeholder.
  local handshake_status=$status handshake_sid=$sid

  local version body
  body=$(curl_rpc -X POST -H "X-Transmission-Session-Id: $sid" \
    -H 'Content-Type: application/json' \
    -d '{"method":"session-get"}' "$base/transmission/rpc")
  version=$(printf '%s' "$body" | jq -r '.arguments.version // "unknown"')
  note "  version $version"

  # session-get carries download-dir AND incomplete-dir, which is what stops a
  # client resolving a mid-transfer path that does not exist yet.
  write_exchange transmission session-get POST "/transmission/rpc" 200 "$body" \
    '{"Content-Type":"application/json"}' "$version"

  # Now that the version is known, the handshake can be written with it.
  write_exchange transmission session-handshake-409 POST "/transmission/rpc" \
    "${handshake_status:-409}" "" \
    "$(jq -n --arg s "$handshake_sid" '{"X-Transmission-Session-Id":$s}')" "$version"

  body=$(curl_rpc -X POST -H "X-Transmission-Session-Id: $sid" \
    -H 'Content-Type: application/json' \
    -d '{"method":"torrent-get","arguments":{"fields":["id","name","hashString","status","percentDone","downloadDir","labels","error","errorString","trackerStats","isFinished","eta","totalSize"]}}' \
    "$base/transmission/rpc")
  write_exchange transmission torrent-get POST "/transmission/rpc" 200 "$body" \
    '{"Content-Type":"application/json"}' "$version"

  # WHY trackerStats IS IN THAT FIELD LIST, measured on a real instance:
  #
  # A torrent whose only tracker does not resolve reports
  #     error = 0, errorString = ""
  # at the TOP level, while trackerStats[].lastAnnounceResult says
  #     "Could not connect to tracker".
  #
  # So a client that watches errorString — the obvious field, and the one its
  # name promises — will never see a tracker failure. The transfer simply sits
  # at 0% looking perfectly healthy, forever. Anything deciding "is this stuck"
  # has to read trackerStats.

  cat <<'MSG'

  Still missing, and worth adding by hand if your instance will produce them:
    torrent-get-completed   a transfer at percentDone 1
    torrent-get-errored     a transfer with a non-empty errorString
    unauthorised            a 401 from the RPC endpoint

  NOTE ON LABELS: if your Transmission is older than RPC 16 it has no labels,
  and the client falls back to a download subdirectory. Capture whichever your
  instance actually does and record the rpc-version — the fallback is a real
  path and needs a fixture too.
MSG
}

# Sourced rather than executed? Then stop here and expose the functions.
#
# This exists so redact() can be TESTED rather than trusted. It is the single
# most important function in this file — it is what stands between a real
# tracker passkey and a permanent public record — and a redactor nobody has
# watched redact anything is exactly the kind of thing that has a typo in one
# of seven sed expressions.
#
# internal/providers/fixtures drives it through this seam.
if [[ "${BASH_SOURCE[0]}" != "${0}" ]]; then
  return 0
fi

service=${1:-}
case "$service" in
  prowlarr)     shift; capture_prowlarr "$@" ;;
  transmission) shift; capture_transmission "$@" ;;
  *) die "usage: $0 {prowlarr|transmission} <endpoint> <credentials...>" ;;
esac

note ""
note "Now READ what was written before committing it:"
note "  git diff --stat $CORPUS"
note ""
note "Then check it with the scanner, which is what CI will do:"
note "  go test ./internal/providers/fixtures/ -run TestTheCommittedCorpusIsClean"
note ""
note "The scanner is a second line, not the first. If it fires, redaction here"
note "missed a shape — fix redact() as well, or the next capture repeats it."
