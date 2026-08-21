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
#   scripts/capture-fixtures.sh torznab      http://host:9696/1/api <api-key>
#   scripts/capture-fixtures.sh transmission http://host:9091 <user> <pass>
#
# Nothing here is committed automatically. Read what it wrote, then commit it.

set -euo pipefail

CORPUS=${CORPUS:-internal/providers/fixtures/testdata}
REDACTED=REDACTED
# The placeholder a redacted endpoint becomes. A resolvable-looking but
# reserved name (RFC 2606), so a fixture reads naturally and nothing in it can
# ever be dialled by accident.
REDACTED_HOST=indexer.invalid

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
#
# ---------------------------------------------------------------------------
# THE API-KEY CHARSET IS NOT HEX, AND ASSUMING IT WAS LET A LIVE KEY THROUGH
# ---------------------------------------------------------------------------
#
# The first version of this matched [0-9a-fA-F]{8,} — a 32-character lower-case
# hex key, which is what the *arr stack issues. Jackett issues a 32-character
# lower-case ALPHANUMERIC key, and `w2dgeuyey1vf7dg9d1kjkicwrfnmzyht` is not
# hex. Measured against a real instance: the key survived redaction intact, and
# the scanner's matching rule missed it for the same reason.
#
# That is ADR-0028's argument arriving in the safety layer rather than the
# client. The redactor had been shaped, accidentally, to the one server it was
# written against — which is the precise failure the ADR says binding to a
# protocol rather than a product is supposed to prevent. Two servers speak
# Torznab; only one of them has hex keys.
#
# So the charset is now permissive. A false redaction costs a fixture that
# says REDACTED where it could have said something harmless; a false miss
# costs a live credential in a permanent public record. Those are not
# comparable, and the pattern is chosen accordingly.
#
# ---------------------------------------------------------------------------
# THE ENDPOINT ITSELF IS SENSITIVE HERE, AND IT IS NOT A CREDENTIAL
# ---------------------------------------------------------------------------
#
# This repository is public. A Torznab feed echoes the server it was served
# from — in <atom:link href>, in every coverurl — so a capture carries a host
# name, a port, and by implication a network topology into a permanent public
# record. None of that is secret in the credential sense and all of it is
# exactly what this repo must not contain.
#
# CAPTURE_HOST is set from the endpoint by each capture function, so the
# substitution names one host rather than guessing at what a host looks like.
redact() {
  local host=${CAPTURE_HOST:-}
  sed -E \
    -e "s/(api[_-]?key=)[0-9a-zA-Z._-]{8,}/\1$REDACTED/gI" \
    -e "s/(\"X-Api-Key\"[[:space:]]*:[[:space:]]*\")[^\"]+/\1$REDACTED/gI" \
    -e "s/(\"X-Transmission-Session-Id\"[[:space:]]*:[[:space:]]*\")[^\"]+/\1$REDACTED/gI" \
    -e "s|([a-zA-Z][a-zA-Z0-9+.-]*://)[^/[:space:]\"']*:[^/[:space:]\"'@]+@|\1$REDACTED:$REDACTED@|g" \
    -e "s|(/announce[^\"' ]*[?\&](passkey\|pid\|torrent_pass\|authkey)=)[0-9a-zA-Z]+|\1$REDACTED|gI" \
    -e "s|(/announce/)[0-9a-zA-Z]{16,}|\1$REDACTED|gI" \
    -e "s/(\"Authorization\"[[:space:]]*:[[:space:]]*\")[^\"]+/\1$REDACTED/gI" \
  | redact_host "$host"
}

# redact_host replaces the captured endpoint's host with a placeholder.
#
# Separate from the credential pass because it is a different KIND of rule: it
# is driven by what the operator typed rather than by a shape, and it is a
# no-op when nothing was passed. Folding it into the sed above would mean
# building that pipeline conditionally, which is how one of seven expressions
# ends up quietly dropped.
redact_host() {
  local host=$1
  if [[ -z "$host" ]]; then
    cat
    return 0
  fi
  # The host is escaped before it reaches a regex: it is user input containing
  # dots at minimum, and an unescaped dot matching any character is how a
  # redactor removes more than it was asked to and nobody notices.
  local escaped
  escaped=$(printf '%s' "$host" | sed -E 's/[][\.^$*+?(){}|/\\]/\\&/g')
  sed -E -e "s/$escaped/$REDACTED_HOST/gI"
}

# write_exchange records one request/response pair with its provenance.
write_exchange() {
  local service=$1 name=$2 method=$3 path=$4 status=$5 body=$6 headers=$7 version=$8
  # SERVER is the product that answered, and it decides the subdirectory.
  #
  # Two servers speaking one protocol produce two corpora whose exchange names
  # collide — both have a caps and an unauthorised — so a flat directory would
  # mean the second capture silently overwriting the first.
  local server=${SERVER:-}
  local slug
  slug=$(printf '%s' "$server" | tr '[:upper:]' '[:lower:]')
  local dir="$CORPUS/$service"
  # A server whose name IS the protocol gets no subdirectory.
  #
  # Transmission RPC is one protocol with one implementation, so
  # transmission/transmission would be a directory saying nothing twice.
  # Torznab is one protocol with several, and there the subdirectory is the
  # only thing keeping two captures of "caps" from overwriting each other.
  if [[ -n "$slug" && "$slug" != "$service" ]]; then
    dir="$CORPUS/$service/$slug"
  fi
  mkdir -p "$dir"

  jq -n \
    --arg name "$name" \
    --arg service "$service" \
    --arg server "$server" \
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
        server: $server,
        version: $version,
        captured_at: $at,
        procedure: $proc
      },
      request:  { method: $method, path: $path },
      response: { status: $status, headers: $headers, body: $body }
    }' | redact > "$dir/$name.json"

  # The path actually written, not a reconstruction of it — the two diverged
  # the moment captures gained a per-server subdirectory, and a capture script
  # that reports a path nothing is at is how the wrong file gets committed.
  ok "${dir#"$CORPUS"/}/$name.json"
}

# Torznab, not Prowlarr's own API (ADR-0028).
#
# Heyarr binds to the PROTOCOL, so this captures what any Torznab endpoint
# serves — Prowlarr's, Jackett's, or a tracker exposing one natively. The
# corpus then stays valid across product versions, which matters more here than
# anywhere else: for an indexer these fixtures are the only test that will ever
# run, and a corpus pinned to one product's current JSON API needs recapturing
# from an instance nobody may still have.
#
# Prowlarr exposes a Torznab endpoint per indexer at
#   /<n>/api?apikey=…            (n is the indexer id)
# and an all-indexers endpoint at
#   /api/v1/indexer/…            (product-specific — deliberately NOT used)
#
# Jackett's is /api/v2.0/indexers/<id>/results/torznab/api.
capture_torznab() {
  local base=$1 key=$2
  [[ -n "$key" ]] || die "torznab needs an api key"

  # The endpoint's host, so redact() can take it out of every recorded body.
  #
  # A Torznab feed ECHOES the server it came from — <atom:link href>, every
  # coverurl — so this is not belt-and-braces. Without it a capture writes a
  # host name and a port into a public repository.
  CAPTURE_HOST=$(printf '%s' "$base" | sed -E 's|^[a-zA-Z][a-zA-Z0-9+.-]*://||; s|[/?].*$||')
  export CAPTURE_HOST

  # Deliberately NOT echoing $base — this runs in terminals that get pasted
  # into issues. The host is what we are about to redact out of the corpus.
  note "torznab — capturing from the configured endpoint"

  # t=caps FIRST, and it is not a formality.
  #
  # It is the capability handshake ADR-0025's pattern asks for, one level up
  # from a version string: the indexer states which content types it can search
  # and which parameters it accepts. An indexer that cannot serve what is being
  # wanted should be reported as such rather than queried and found wanting.
  local caps status
  caps=$(curl -sS "$base?t=caps&apikey=$key") || die "could not reach the torznab endpoint"
  status=$(curl -sS -o /dev/null -w '%{http_code}' "$base?t=caps&apikey=$key")
  [[ "$status" == "200" ]] || die "t=caps answered $status; is the key right?"

  # Torznab is XML, so the version comes out of the caps document rather than a
  # JSON field. Missing is not fatal — some servers omit it — but it is
  # recorded when present, because provenance without a version is a capture
  # nobody can regenerate.
  local version
  version=$(printf '%s' "$caps" | sed -n 's/.*<server[^>]*version="\([^"]*\)".*/\1/p' | head -1)
  [[ -n "$version" ]] || version="unreported"

  # WHICH server this is, taken from the caps document rather than asked for.
  #
  # <server title="Jackett"/> and <server title="Prowlarr"/> — the one field
  # that says which implementation of the protocol answered. Reading it here
  # rather than making the operator type it means the corpus cannot be filed
  # under the wrong server by a typo, and ADR-0028's claim rests on that
  # filing being right.
  SERVER=${SERVER:-$(printf '%s' "$caps" \
    | sed -n 's/.*<server[^>]*title="\([^"]*\)".*/\1/p' | head -1)}
  [[ -n "$SERVER" ]] || SERVER=unknown
  export SERVER
  note "  $SERVER, version $version"

  write_exchange torznab caps GET "?t=caps" "$status" "$caps" \
    '{"Content-Type":"application/xml"}' "$version"

  # A search WITH results. Pass QUERY=… to choose your own.
  local q=${QUERY:-ubuntu}
  local path="?t=search&q=$q&apikey=$key"
  local body
  body=$(curl -sS "$base$path")
  status=$(curl -sS -o /dev/null -w '%{http_code}' "$base$path")
  # The apikey is in the path and redact() strips it before writing.
  write_exchange torznab search-with-results GET "$path" "$status" "$body" \
    '{"Content-Type":"application/xml"}' "$version"

  # A search with ZERO results. §63 can only report an empty candidate set if
  # something ever returns one — and Torznab answers this with a valid feed
  # containing no items rather than an error, which is a shape a client must
  # not mistake for a failure.
  local empty="?t=search&q=zzzzzzzz-no-such-release-zzzzzzzz&apikey=$key"
  body=$(curl -sS "$base$empty")
  write_exchange torznab search-empty GET "$empty" 200 "$body" \
    '{"Content-Type":"application/xml"}' "$version"

  # A bad key. Provoked deliberately rather than waited for: this response
  # decides whether a wrong credential is reported as a configuration problem
  # or retried forever.
  #
  # Torznab signals it as an <error code="100"> DOCUMENT, usually with HTTP
  # 200 — so a client checking only the status code will read an error as a
  # successful empty search and report "no releases found" forever.
  local badkey="?t=search&q=$q&apikey=definitely-not-a-valid-key"
  body=$(curl -sS "$base$badkey" || true)
  status=$(curl -sS -o /dev/null -w '%{http_code}' "$base$badkey" || true)
  write_exchange torznab unauthorised GET "$badkey" "${status:-200}" "$body" \
    '{"Content-Type":"application/xml"}' "$version"

  # An <error> DOCUMENT WITH A NON-200 STATUS.
  #
  # This is the trap's other half, and it is why the client must not gate
  # parsing on the status code in EITHER direction. A bad key is an error
  # document with HTTP 200; an unsupported function is an error document with
  # HTTP 400. A client that parses the body only on 200 misses this one, and a
  # client that trusts the status only misses the other.
  local badfn="?t=nosuchfunction&apikey=$key"
  body=$(curl -sS "$base$badfn" || true)
  status=$(curl -sS -o /dev/null -w '%{http_code}' "$base$badfn" || true)
  write_exchange torznab unsupported-function GET "$badfn" "${status:-400}" "$body" \
    '{"Content-Type":"application/xml"}' "$version"

  # A body that is NOT XML at all, from an endpoint that is supposed to serve
  # it.
  #
  # Both servers answer a request for an indexer that does not exist with
  # JSON — a 404 from one, a 500 with a stack trace from the other — on the
  # same path shape that otherwise serves Torznab. This is the misconfiguration
  # an operator actually produces: an indexer id that was right until the
  # indexer was removed. The client has to name what failed to parse rather
  # than reporting "no releases found".
  if [[ -n "${MISSING_INDEXER_URL:-}" ]]; then
    body=$(curl -sS "$MISSING_INDEXER_URL" || true)
    status=$(curl -sS -o /dev/null -w '%{http_code}' "$MISSING_INDEXER_URL" || true)
    write_exchange torznab indexer-not-found GET "<an indexer id that does not exist>" \
      "${status:-404}" "$body" '{"Content-Type":"application/json"}' "$version"
  fi

  cat <<'MSG'

  Still missing, and worth adding by hand if your endpoint will produce them:
    rate-limited   a 429, or a torznab <error> saying so
    malformed      a truncated or non-XML body
    proxy-error    an HTML error page from a reverse proxy

  Those are the responses that actually occur at 03:00. A fixture written by
  hand for one of these is legitimate — set origin to "synthesised" and say in
  the note why a real one could not be obtained.

  ALSO WORTH CAPTURING: a search whose results OMIT attributes the client reads
  — no <torznab:attr name="resolution">, no seeders, no size. Attribute
  extraction happens inside the provider, so §63 can only report "undetermined"
  for something it never received, and that path is unreachable without a
  fixture that genuinely leaves the attribute out.
MSG
}

capture_transmission() {
  local base=$1 user=${2:-} pass=${3:-}
  SERVER=${SERVER:-Transmission}
  export SERVER
  CAPTURE_HOST=$(printf '%s' "$base" | sed -E 's|^[a-zA-Z][a-zA-Z0-9+.-]*://||; s|[/?].*$||')
  export CAPTURE_HOST
  note "transmission — capturing from the configured endpoint"

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
  torznab)      shift; capture_torznab "$@" ;;
  transmission) shift; capture_transmission "$@" ;;
  *) die "usage: $0 {torznab|transmission} <endpoint> <credentials...>" ;;
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
