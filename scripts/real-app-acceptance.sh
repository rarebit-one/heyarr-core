#!/usr/bin/env bash
# The app-in-the-loop scene for the compatibility adapters (#373, #376, #30).
#
# `make demo` drives the OpenSubsonic and OPDS adapters the way a real client
# would and proves byte identity against the blob route — protocol conformance
# on a real byte path. What it cannot do is the honest far end of the bar: a
# REAL third-party app, code we did not write, browsing and playing. That is
# app-in-the-loop, out of the 300s demo budget, and it needs a human and a
# device (the #202 deferral pattern).
#
# This script is the other half: it stands up a disposable node with the
# ordinary fixture library, binds it to TCP so an app can reach it, and prints
# exactly what to type into that app. The manual pass is then a repeatable
# procedure against a known scene rather than an anecdote about somebody's
# library — which is the difference between evidence and a recollection.
#
# It is NOT on the merge path and asserts nothing by itself. The evidence a run
# produces is recorded in docs/acceptance/real-app-in-the-loop.md.
#
# Requires: the Go toolchain (or prebuilt ./bin/heyarr and ./bin/genlibrary).
#
#   scripts/real-app-acceptance.sh                  # bind 127.0.0.1:7788
#   PORT=9999 scripts/real-app-acceptance.sh        # another port
#   BIND=0.0.0.0 scripts/real-app-acceptance.sh     # reachable from the LAN
#
# For a USB-attached Android device, leave the bind on loopback and tunnel it —
# the phone reaches the scene without the scene being exposed to any network:
#
#   adb reverse tcp:7788 tcp:7788
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=${PORT:-7788}
BIND=${BIND:-127.0.0.1}
USER_NAME=${USER_NAME:-realapp}

WORK=$(mktemp -d "${TMPDIR:-/tmp}/heyarr-real-app.XXXXXX")
NODE_PID=""

cleanup() {
  [[ -n "$NODE_PID" ]] && kill "$NODE_PID" 2>/dev/null || true
  wait "$NODE_PID" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

BIN=${BIN:-./bin/heyarr}
GEN=${GEN:-./bin/genlibrary}
for pair in "BIN:./cmd/heyarr" "GEN:./internal/testutil/fixtures/cmd/genlibrary"; do
  var=${pair%%:*} pkg=${pair#*:}
  if [[ ! -x "${!var}" ]]; then
    command -v go >/dev/null 2>&1 || { echo "no ${!var} and no Go toolchain to build it" >&2; exit 1; }
    go build -o "$WORK/$(basename "${!var}")" "$pkg"
    printf -v "$var" '%s' "$WORK/$(basename "${!var}")"
  fi
done

echo "== app-in-the-loop scene =="
echo "-- generating the fixture library"
# The streaming fixture is the demo's 200MB one by default and nothing here
# needs it large; 8MB keeps the scene quick to stand up.
"$GEN" -out "$WORK/library" -manifest "$WORK/manifest.json" -large-size $((8 * 1024 * 1024)) >/dev/null

cat > "$WORK/node.yaml" <<YAML
data_dir: $WORK/data
peer:
  name: real-app-loop
  site: test
log:
  level: info
  format: json
# TCP, deliberately: the merge-path demo speaks over a unix socket because a
# fixed port collides between runs, but no phone can open a unix socket.
http:
  addr: $BIND:$PORT
libraries:
  - name: albums
    content_type: music
    roots: ["$WORK/library/music"]
  - name: shelf
    content_type: book
    roots: ["$WORK/library/books"]
YAML

echo "-- starting the node"
"$BIN" --config "$WORK/node.yaml" all > "$WORK/node.log" 2>&1 &
NODE_PID=$!

READY=0
for _ in $(seq 1 100); do
  if curl -fsS "http://$BIND:$PORT/readyz" >/dev/null 2>&1; then READY=1; break; fi
  sleep 0.1
done
(( READY )) || { echo "the node never became ready" >&2; cat "$WORK/node.log" >&2; exit 1; }

# Four ingestable files: two tracks of one album, an epub and a cbz. A bounded
# wait, because a scene that never ingested must say so rather than hand an
# empty library to a person who will read the empty screen as an app bug.
INGESTED=0
for _ in $(seq 1 300); do
  INGESTED=$(grep -c '"msg":"ingested"' "$WORK/node.log" 2>/dev/null || true)
  (( INGESTED >= 4 )) && break
  sleep 0.1
done
(( INGESTED >= 4 )) || { echo "only $INGESTED of 4 files ingested" >&2; tail -20 "$WORK/node.log" >&2; exit 1; }

TOKEN=$("$BIN" --config "$WORK/node.yaml" token create "$USER_NAME" --scopes admin --json | sed -n 's/.*"token": *"\([^"]*\)".*/\1/p')
[[ -n "$TOKEN" ]] || { echo "no token was minted" >&2; exit 1; }

cat <<TXT

  the scene is up — $INGESTED files ingested, log at $WORK/node.log

  OpenSubsonic (#373)          http://$BIND:$PORT
  OPDS          (#376)         http://$BIND:$PORT/opds
  username                     $USER_NAME
  password                     $TOKEN

  The password is a Heyarr bearer token. The adapter REFUSES the Subsonic
  salted-token scheme (error 40) by design, and every stock Subsonic client
  sends it by default — so the client must be told to send the password
  directly. In Ultrasonic that switch is "Force plain password authentication".

  What a pass has to show, per #373 and #376:
    - the app browsed the catalogue it did not know in advance, and
    - played (or downloaded) a real file from it.

  Ctrl-C tears the scene down; the data directory is disposable.

TXT

wait "$NODE_PID"
