#!/usr/bin/env bash
# The daemon-in-the-loop acceptance for the download clients (#379, ADR-0052).
#
# It is the honest proof behind the claim `acquires-over-daemon-clients` — "a
# real qBittorrent transfer completed" — that the merge-path demo cannot make,
# because a real download-client daemon is out of the demo budget by design
# (ADR-0026). It stands up a DISPOSABLE qBittorrent and a web seed, runs the Go
# harness test against them, and tears them down.
#
# It is NOT on the merge path. It runs from `make daemon-acceptance`, and from
# the scheduled `daemon-acceptance` workflow — a lane that reports, never one a
# pull request waits on, so a container pull can never gate a merge.
#
# Requires: docker (with the compose plugin) and the Go toolchain.
set -euo pipefail
cd "$(dirname "$0")/.."

HARNESS=test/harness/qbittorrent
COMPOSE=(docker compose -f "$HARNESS/docker-compose.yml")

# A run touches nothing outside these two directories, both under the harness.
WEBSEED_DIR="$(pwd)/$HARNESS/_webseed"
DOWNLOAD_DIR="$(pwd)/$HARNESS/_downloads"

FAILED=0
pass() { printf '  \033[32mok\033[0m   %s\n' "$1"; }
fail() { FAILED=1; printf '  \033[31mFAIL\033[0m %s\n' "$1"; }

cleanup() {
  "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$WEBSEED_DIR" "$DOWNLOAD_DIR"
}
trap cleanup EXIT INT TERM

echo "== daemon-in-the-loop acceptance: qBittorrent =="

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is not available; this lane needs it (ADR-0052)" >&2
  exit 1
fi

rm -rf "$WEBSEED_DIR" "$DOWNLOAD_DIR"
mkdir -p "$WEBSEED_DIR" "$DOWNLOAD_DIR"
# The container writes as uid 1000; make sure it can, and that the test (this
# user) can read back what it writes.
chmod 0777 "$WEBSEED_DIR" "$DOWNLOAD_DIR"

export HEYARR_HARNESS_DOWNLOAD_DIR="$DOWNLOAD_DIR"
export HEYARR_HARNESS_WEBSEED_DIR="$WEBSEED_DIR"

echo "-- bringing up the daemons"
"${COMPOSE[@]}" up -d

# Wait for the qBittorrent Web API to answer. A bounded wait, because a daemon
# that never comes up must fail this script rather than hang it (the reasoning
# scripts/acceptance.sh applies to every command it runs).
echo -n "-- waiting for the qBittorrent Web API "
UP=0
for _ in $(seq 1 60); do
  if curl -fsS "http://127.0.0.1:8080/api/v2/app/version" >/dev/null 2>&1; then
    UP=1
    break
  fi
  echo -n "."
  sleep 2
done
echo
if [ "$UP" -ne 1 ]; then
  fail "the qBittorrent Web API never became reachable"
  echo "-- qbittorrent logs (tail):"
  "${COMPOSE[@]}" logs --tail=50 qbittorrent || true
  exit 1
fi
pass "the qBittorrent Web API is reachable"

# The harness test reaches the daemon over the host-published port and the web
# seed over the compose network's DNS name.
export HEYARR_HARNESS_QBITTORRENT_URL="http://127.0.0.1:8080"
export HEYARR_HARNESS_WEBSEED_BASEURL="http://webseed"
export HEYARR_HARNESS_REMOTE_SAVE="/downloads"

echo "-- running the harness test"
if go test -race -count=1 -run TestHarnessQBittorrentTransfer -v ./internal/downloads/; then
  pass "a real qBittorrent transfer completed and the bytes matched"
else
  fail "the qBittorrent daemon-in-the-loop test failed"
fi

echo
if [ "$FAILED" -ne 0 ]; then
  echo "== FAILED =="
  exit 1
fi
echo "== PASSED: acquires-over-daemon-clients (qBittorrent) proven against a real daemon =="
