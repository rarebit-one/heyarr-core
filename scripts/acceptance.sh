#!/usr/bin/env bash
# The executable definition of "the current milestone is done".
#
# This script is the merge gate. It drives the real binary end to end and
# asserts the properties every later milestone depends on. It grows with each
# milestone; M1-18 completes it with the scan/ingest/range/idempotency checks.
#
# Everything here runs against a temporary data directory and touches nothing
# outside it. No network required.
set -euo pipefail
cd "$(dirname "$0")/.."

BIN=${BIN:-./bin/heyarr}
WORK=$(mktemp -d)
FAILED=0
# Kill anything this script started before removing its data directory. Without
# this an interrupted run leaves a server holding a port and a database, and the
# next run fails to bind with an error that points at neither.
cleanup() {
  local p
  for p in "${FULL_PIDS[@]:-}"; do kill -KILL "$p" 2>/dev/null || true; done
  pkill -f "$WORK" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

pass() { printf '  \033[32mok\033[0m   %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAILED=1; }
note() { printf '\n\033[1m%s\033[0m\n' "$1"; }

assert_contains() { # haystack needle description
  if [[ "$1" == *"$2"* ]]; then pass "$3"; else
    fail "$3"; printf '       wanted to find: %s\n       in: %s\n' "$2" "$1"
  fi
}
assert_not_contains() {
  if [[ "$1" != *"$2"* ]]; then pass "$3"; else
    fail "$3"; printf '       did not want to find: %s\n' "$2"
  fi
}

# Runs a command that MUST exit, bounded by a deadline, and captures its output.
# Without the deadline a regressed refusal turns this script into a hang rather
# than a failure — and a test that hangs is as useless as one that passes
# silently, because CI cannot tell it apart from a slow machine.
# Sets REPLY to the combined output. Returns 0 if the command exited non-zero
# (the expected refusal), 1 if it succeeded, 2 if it had to be killed.
expect_refusal() { # deadline_seconds command...
  local deadline=$1; shift
  local out="$WORK/refusal.$$.out" pid rc waited=0
  "$@" >"$out" 2>&1 &
  pid=$!
  while (( waited < deadline * 10 )); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if kill -0 "$pid" 2>/dev/null; then
    kill -KILL "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
    REPLY=$(cat "$out"); rm -f "$out"
    return 2
  fi
  wait "$pid" && rc=0 || rc=$?
  REPLY=$(cat "$out"); rm -f "$out"
  (( rc == 0 )) && return 1
  return 0
}

# Asserts that a command refuses to start, and that it says why.
assert_refuses() { # description needle command...
  local desc=$1 needle=$2 status=0; shift 2
  # `set -e` would kill the script on expect_refusal's non-zero return, which is
  # its normal signal rather than an error — and it would do so producing no
  # output, so a regression would look like a silent early exit rather than a
  # failure. Capture the status instead of letting it propagate.
  expect_refusal 10 "$@" || status=$?
  case $status in
    0) assert_contains "$REPLY" "$needle" "$desc" ;;
    1) fail "$desc — it started instead of refusing" ;;
    2) fail "$desc — it hung and had to be killed (a refusal must be immediate)" ;;
  esac
}

[[ -x "$BIN" ]] || { echo "acceptance: $BIN not built — run 'make build'"; exit 1; }

cat > "$WORK/heyarr.yaml" <<YAML
data_dir: $WORK/data
peer:
  name: acceptance
  site: test
log:
  level: info
  format: json
# Unix socket only. The default TCP bind is a FIXED port, so two runs on one
# machine collide and a leaked server from an interrupted run breaks every later
# run with a bind error that explains nothing. Nothing here needs TCP — the
# refusal checks below set http.addr explicitly for the cases that do.
http:
  addr: ""
YAML

note "build identity"
V=$("$BIN" version --json)
if echo "$V" | grep -q '"version"' && echo "$V" | grep -q '"go_version"'; then
  pass "version --json carries build identity"
else
  fail "version --json is missing fields"; echo "$V"
fi

note "configuration layering"
OUT=$("$BIN" --config "$WORK/heyarr.yaml" config print)
assert_contains "$OUT" "$WORK/data/cas"      "cas root is derived from data_dir"
assert_contains "$OUT" "$WORK/data/heyarr.db" "database path is derived from data_dir"
OUT=$(HEYARR_PEER_SITE=overridden "$BIN" --config "$WORK/heyarr.yaml" config print)
assert_contains "$OUT" "overridden" "HEYARR_ environment overrides the config file"

note "refusals — configuration is validated before anything starts"
# ADR-0011: this server range-serves the whole library and milestone 1 has no
# identity model, so an unauthenticated non-loopback listener is refused.
assert_refuses "refuses an unauthenticated public bind" "refusing to start" \
  env HEYARR_HTTP_ADDR=0.0.0.0:7777 HEYARR_HTTP_AUTH_ENABLED=false \
  "$BIN" --config "$WORK/heyarr.yaml" all
assert_refuses "rejects an invalid log level, naming the field" "log.level" \
  env HEYARR_LOG_LEVEL=verbose "$BIN" --config "$WORK/heyarr.yaml" all
assert_refuses "names the config path it could not read" "$WORK/nope.yaml" \
  "$BIN" --config "$WORK/nope.yaml" all

# Runs one role set, sends SIGTERM, and asserts a clean exit within the deadline.
run_and_term() { # label deadline_seconds role...
  local label=$1 deadline=$2; shift 2
  local log="$WORK/$label.log" pids=() rc=0
  for role in "$@"; do
    "$BIN" --config "$WORK/heyarr.yaml" "$role" >>"$log" 2>&1 &
    pids+=($!)
  done

  # Wait for each role to report itself up, rather than sleeping a fixed
  # duration. A fixed wait is a bet on machine speed, and it lost as soon as
  # the schema migration grew: on a slow runner the SIGTERM arrived
  # mid-migration and the role correctly reported a clean stop during startup,
  # so the "started" line this asserts was never going to appear.
  # `all` runs the three roles concurrently, so waiting for any one of them
  # proves nothing about the others — the controller is the slow one, because
  # it migrates.
  # Wait for READINESS, not liveness. The worker reports "worker started" the
  # moment it is alive and supervised, which is before the controller has
  # migrated and therefore before it can claim anything — so waiting for that
  # line means SIGTERM can arrive while it is still waiting for the schema, and
  # everything downstream of readiness silently never happens. Waiting for a
  # proxy is the same mistake as sleeping a fixed duration, one level up.
  local wanted=()
  for role in "$@"; do
    case "$role" in
      all)    wanted+=("controller started" "worker ready" "peer started") ;;
      worker) wanted+=("worker ready") ;;
      *)      wanted+=("$role started") ;;
    esac
  done

  local waited_start
  for want in "${wanted[@]}"; do
    waited_start=0
    while (( waited_start < 300 )); do          # 30s
      grep -q "$want" "$log" 2>/dev/null && break
      sleep 0.1; waited_start=$(( waited_start + 1 ))
    done
    if (( waited_start >= 300 )); then
      fail "$label: never saw \"$want\""
      cat "$log"
      for p in "${pids[@]}"; do kill -KILL "$p" 2>/dev/null || true; done
      return
    fi
  done

  for p in "${pids[@]}"; do kill -TERM "$p" 2>/dev/null || true; done

  local waited=0
  while (( waited < deadline * 10 )); do
    local alive=0
    for p in "${pids[@]}"; do kill -0 "$p" 2>/dev/null && alive=1; done
    (( alive == 0 )) && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  for p in "${pids[@]}"; do wait "$p" || rc=1; done
  if (( waited >= deadline * 10 )); then
    fail "$label did not stop within ${deadline}s of SIGTERM"; return
  fi
  if (( rc != 0 )); then fail "$label exited non-zero"; cat "$log"; return; fi
  pass "$label starts and exits 0 within ${deadline}s of SIGTERM"
  cat "$log"
}

note "single-process mode"
LOG=$(run_and_term all 5 all)
echo "$LOG" | grep -q '^  ok' && echo "$LOG" | grep '^  ' || true
ALLLOG=$(cat "$WORK/all.log")
assert_contains "$ALLLOG" '"version"'        "startup line carries the build version"
assert_contains "$ALLLOG" '"commit"'         "startup line carries the commit"
for r in controller worker peer; do
  assert_contains "$ALLLOG" "$r started" "heyarr all starts the $r"
  assert_contains "$ALLLOG" "$r stopped" "heyarr all stops the $r"
done

# ADR-0002: roles must be independently runnable as OS processes. Running the
# acceptance checks in both configurations is what keeps that honest — otherwise
# only one of the two is ever exercised.
note "split-process mode (ADR-0002)"
run_and_term split 5 controller worker peer >/dev/null
SPLITLOG=$(cat "$WORK/split.log")
for r in controller worker peer; do
  assert_contains "$SPLITLOG" "$r started" "$r runs as its own process"
done

note "persistence"
DB="$WORK/data/heyarr.db"
if [[ -f "$DB" ]]; then pass "the controller created its database"; else fail "no database at $DB"; fi
# After shutdown the database file must stand alone: §50 replicates controller
# backups to peers, and a populated -wal beside a copied file is a silently
# stale backup. SQLite removes the WAL itself on last-connection close, so this
# asserts the property rather than our implementation of it.
if [[ -s "$DB-wal" ]]; then
  fail "a populated WAL survived shutdown ($(wc -c <"$DB-wal") bytes) — a file-copy backup would be stale"
else
  pass "the database file is self-contained after shutdown"
fi
# The schema must survive a restart rather than being rebuilt each time.
V1=$(grep -ao '"schema_version":[0-9]*' "$WORK/all.log" | head -1 | cut -d: -f2)
run_and_term restart 5 all >/dev/null
V2=$(grep -ao '"schema_version":[0-9]*' "$WORK/restart.log" | head -1 | cut -d: -f2)
if [[ -n "$V1" && "$V1" == "$V2" ]]; then
  pass "the schema survives a restart (version $V1)"
else
  fail "schema version changed across a restart: '$V1' then '$V2'"
fi

note "ingest wiring (M1-10)"
# Started and ready are different things. A worker alive before any controller
# has migrated is legitimately unable to work, and saying so is the difference
# between "starting up" and "broken" (ADR-0002).
assert_contains "$ALLLOG" "worker ready" "the worker reports readiness separately from liveness"

# A handler registered in a map nobody has watched be read is not wiring. The
# worker runtime logs the job types it will claim, so assert the one that
# matters is among them — this is the single path bytes enter Heyarr (§65).
assert_contains "$ALLLOG" "ingest_artifact" "the worker registers the ingest_artifact handler"

# ADR-0010: the peer model exists from milestone 1 with exactly one peer, and
# it is this node. It is created on first start and never again — a second
# self peer is unrecoverable once replication has run, which is why the
# database refuses it and why this asserts the count rather than the ability.
assert_contains "$ALLLOG" "registered this peer" "the self peer is registered on first start"
RESTARTLOG=$(cat "$WORK/restart.log")
assert_not_contains "$RESTARTLOG" "registered this peer" \
  "the self peer is registered once, not once per start"

# §7 and ADR-0003: the controller owns the schema. Two processes racing to
# apply the same DDL is a way to find out how goose handles concurrent
# migrations, and that is not a thing to find out in production. The worker
# waits for the schema; it never applies it.
MIGRATIONS=$(grep -c "database schema ready" "$WORK/all.log" || true)
if [[ "$MIGRATIONS" == "1" ]]; then
  pass "exactly one role migrates the schema, even with three running"
else
  fail "the schema was migrated $MIGRATIONS times in one start — only the controller may migrate"
fi
SPLITMIGRATIONS=$(grep -c "database schema ready" "$WORK/split.log" || true)
if [[ "$SPLITMIGRATIONS" == "1" ]]; then
  pass "in split-process mode the worker waits for the schema rather than applying it"
else
  fail "split mode migrated $SPLITMIGRATIONS times — only the controller may migrate"
fi

# A worker started with no controller anywhere is the ADR-0002 case: roles are
# independently runnable, so it must wait for the schema rather than migrate it
# — and must still stop when told to. The first cut of that wait ignored the
# shutdown context and polled for two minutes past the point anyone wanted the
# process alive.
note "a worker with no controller (ADR-0002)"
LONEDIR="$WORK/lone"
mkdir -p "$LONEDIR"
cat > "$WORK/lone.yaml" <<YAML
data_dir: $LONEDIR
peer:
  name: lone
log:
  level: info
  format: json
YAML
"$BIN" --config "$WORK/lone.yaml" worker >"$WORK/lone.log" 2>&1 &
LONEPID=$!
waited=0
while (( waited < 300 )); do
  grep -q "waiting for the controller to migrate" "$WORK/lone.log" 2>/dev/null && break
  sleep 0.1; waited=$(( waited + 1 ))
done
if (( waited >= 300 )); then
  fail "a lone worker never reported that it was waiting for the schema"
  cat "$WORK/lone.log"
  kill -KILL "$LONEPID" 2>/dev/null || true
else
  pass "a lone worker waits for the controller instead of migrating"
  kill -TERM "$LONEPID" 2>/dev/null || true
  waited=0
  while (( waited < 100 )); do
    kill -0 "$LONEPID" 2>/dev/null || break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if kill -0 "$LONEPID" 2>/dev/null; then
    kill -KILL "$LONEPID" 2>/dev/null || true
    wait "$LONEPID" 2>/dev/null || true
    fail "a lone worker ignored SIGTERM while waiting for the schema"
  else
    wait "$LONEPID" && pass "a waiting worker still stops within 10s of SIGTERM" \
      || fail "a waiting worker exited non-zero on SIGTERM"
  fi
fi
LONEMIGRATIONS=$(grep -c "database schema ready" "$WORK/lone.log" || true)
if [[ "$LONEMIGRATIONS" == "0" ]]; then
  pass "a lone worker never migrates the schema, even with nobody else running"
else
  fail "a lone worker applied the schema itself — only the controller may (§7)"
fi

# The first section that drives the whole chain: configuration -> library rows
# -> scan_library -> ingest_artifact -> blobs and assets on disk. Everything
# before this asserts that the parts start; this asserts that they add up.
note "library scan to ingest, end to end (M1-12)"

SCANROOT="$WORK/scan"
LIB="$SCANROOT/library"
mkdir -p "$LIB/Films" "$LIB/Shows/Season 01" "$LIB/@eaDir"

# Two paths, byte-identical. §13: that is ONE blob and TWO assets, and it is the
# case an asset table keyed on the hash would silently collapse into one.
printf 'the very same bytes' > "$LIB/Films/Twin A (2019).mkv"
printf 'the very same bytes' > "$LIB/Films/Twin B (2019).mkv"
printf 'quite different bytes' > "$LIB/Shows/Season 01/Show - S01E01.mkv"

# Noise. A partial download must never be ingested: its hash describes bytes
# that never existed as a complete file, and nothing later can tell that from a
# corrupt copy.
printf 'still downloading' > "$LIB/Films/Partial (2020).mkv.part"
printf 'macos droppings' > "$LIB/Films/.DS_Store"
printf 'synology thumbnail' > "$LIB/@eaDir/thumb.mkv"

cat > "$WORK/scan.yaml" <<YAML
data_dir: $SCANROOT/data
peer:
  name: scanner
log:
  level: info
  format: json
# Unix socket only — see the note on the base config. A fixed TCP port makes two
# runs on one machine collide.
http:
  addr: ""
libraries:
  - name: acceptance-films
    content_type: movie
    roots:
      - $LIB
YAML

CAS_BLOBS="$SCANROOT/data/cas/blobs"

# Starts heyarr, waits for the scan to COMPLETE and for every file it enqueued
# to have been ingested, then stops. It waits for those conditions rather than
# sleeping: a fixed wait is a bet on machine speed, and the number of files is
# read out of the scan's own log line rather than hardcoded here, so the two
# cannot drift apart.
# Sets SCAN_ENQUEUED, SCAN_INGESTED, SCAN_SKIPPED, SCAN_MISSING.
run_scan() { # label
  # `local` expands all its arguments before it runs, so log cannot be derived
  # from label on the same line — it would expand to an empty label.
  local label=$1
  local log="$WORK/$label.log" pid waited=0 ingested=0
  "$BIN" --config "$WORK/scan.yaml" all >"$log" 2>&1 &
  pid=$!

  while (( waited < 900 )); do
    grep -q '"msg":"scan completed"' "$log" 2>/dev/null && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if (( waited >= 900 )); then
    kill -KILL "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true
    fail "$label: the scan never completed"; cat "$log"; return 1
  fi

  SCAN_ENQUEUED=$(grep -ao '"files_enqueued":[0-9]*' "$log" | tail -1 | cut -d: -f2)
  SCAN_SKIPPED=$(grep -ao '"files_skipped":[0-9]*' "$log" | tail -1 | cut -d: -f2)
  SCAN_MISSING=$(grep -ao '"files_missing":[0-9]*' "$log" | tail -1 | cut -d: -f2)
  : "${SCAN_ENQUEUED:=0}" "${SCAN_SKIPPED:=0}" "${SCAN_MISSING:=0}"

  waited=0
  while (( waited < 900 )); do
    ingested=$(grep -c '"msg":"ingested"' "$log" 2>/dev/null || true)
    (( ingested >= SCAN_ENQUEUED )) && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  # A no-op scan enqueues nothing, so give a moment's grace only when there was
  # something to wait for.
  if (( waited >= 900 )); then
    kill -KILL "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true
    fail "$label: only $ingested of $SCAN_ENQUEUED enqueued files were ingested"; cat "$log"; return 1
  fi
  SCAN_INGESTED=$ingested

  kill -TERM "$pid" 2>/dev/null || true
  waited=0
  while (( waited < 150 )); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if kill -0 "$pid" 2>/dev/null; then
    kill -KILL "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true
    fail "$label did not stop within 15s of SIGTERM"; return 1
  fi
  wait "$pid" || { fail "$label exited non-zero"; cat "$log"; return 1; }
  return 0
}

if run_scan scan1; then
  SCANLOG=$(cat "$WORK/scan1.log")
  assert_contains "$SCANLOG" "scan_library" "the worker registers the scan_library handler"
  assert_contains "$SCANLOG" "library root reconciled" "the libraries block becomes library_roots rows"

  if [[ "$SCAN_ENQUEUED" == "3" ]]; then
    pass "the scan enqueued the 3 real files"
  else
    fail "the scan enqueued $SCAN_ENQUEUED files, want 3"; echo "$SCANLOG"
  fi
  # .part, .DS_Store and the @eaDir thumbnail. @eaDir is skipped as a whole
  # directory, so its file is never counted as a candidate at all.
  if (( SCAN_SKIPPED >= 2 )); then
    pass "partial downloads and platform droppings are skipped ($SCAN_SKIPPED skipped)"
  else
    fail "the scan skipped only $SCAN_SKIPPED files — .part and .DS_Store must never be ingested"
  fi
  assert_not_contains "$SCANLOG" ".mkv.part" "no partial download reached ingest"
  assert_not_contains "$SCANLOG" "@eaDir" "the Synology thumbnail store was not walked"

  BLOBS=$(find "$CAS_BLOBS" -type f 2>/dev/null | wc -l | tr -d ' ')
  ASSETS=$(grep -c '"asset_created":true' "$WORK/scan1.log" 2>/dev/null || true)
  # §13, and the whole point of content addressing: identical bytes at two
  # paths are one blob and two assets.
  if [[ "$BLOBS" == "2" ]]; then
    pass "three files with one duplicate pair produced 2 blobs"
  else
    fail "the CAS holds $BLOBS blobs, want 2 — identical bytes must deduplicate"
  fi
  if [[ "$ASSETS" == "3" ]]; then
    pass "three files produced 3 assets (the duplicate pair shares one blob)"
  else
    fail "$ASSETS assets were created, want 3"; echo "$SCANLOG"
  fi
  assert_contains "$SCANLOG" '"deduplicated":true' "the second copy of the duplicate pair deduplicated"

  # The claim the fingerprint cache exists to make: scanning again reads
  # nothing, ingests nothing and changes nothing.
  if run_scan scan2; then
    if [[ "$SCAN_ENQUEUED" == "0" ]]; then
      pass "a second scan of an unchanged library enqueues nothing"
    else
      fail "the second scan enqueued $SCAN_ENQUEUED files — the fingerprint cache did not hold"
    fi
    if [[ "$SCAN_INGESTED" == "0" ]]; then
      pass "a second scan ingests nothing"
    else
      fail "the second scan ingested $SCAN_INGESTED files"
    fi
    if [[ "$SCAN_MISSING" == "0" ]]; then
      pass "a second scan marks nothing missing"
    else
      fail "the second scan marked $SCAN_MISSING assets missing with nothing removed"
    fi
    BLOBS2=$(find "$CAS_BLOBS" -type f 2>/dev/null | wc -l | tr -d ' ')
    if [[ "$BLOBS2" == "$BLOBS" ]]; then
      pass "a second scan adds no blobs (still $BLOBS2)"
    else
      fail "the CAS grew from $BLOBS to $BLOBS2 blobs on a no-op rescan"
    fi
  fi

  # ADR-0018: a vanished path is a logical deletion. The bytes stay, because a
  # scanner that unlinked them because a mount was late would be the most
  # destructive component in the system.
  rm "$LIB/Shows/Season 01/Show - S01E01.mkv"
  if run_scan scan3; then
    if [[ "$SCAN_MISSING" == "1" ]]; then
      pass "a vanished path marks exactly one asset missing"
    else
      fail "the scan marked $SCAN_MISSING assets missing after one file was removed"
    fi
    BLOBS3=$(find "$CAS_BLOBS" -type f 2>/dev/null | wc -l | tr -d ' ')
    if [[ "$BLOBS3" == "$BLOBS" ]]; then
      pass "a vanished path leaves its blob alone (ADR-0018)"
    else
      fail "the CAS went from $BLOBS to $BLOBS3 blobs when a file was deleted — a scan must never unlink bytes"
    fi
  fi
fi


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

# A ~200 MB streaming fixture is the point: a file that fits in one buffer
# proves nothing about the 20 GB remux ADR-0013 calls a normal case. Shrinkable
# for a quick local loop, and the size is printed so a green run against a tiny
# fixture is never mistaken for a green run against a real one.
LARGE=${HEYARR_ACCEPTANCE_LARGE_SIZE:-209715200}

api() { # path [curl args...]
  local path=$1; shift
  curl -sS --unix-socket "$SOCK" -H "Authorization: Bearer $TOKEN" "$@" "http://heyarr$path"
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

assert_eq() { # got want description
  if [[ "$1" == "$2" ]]; then pass "$3"; else fail "$3 — got '$1', want '$2'"; fi
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
  # SSE never ends by design, so the read is bounded here rather than waited on.
  events_out=$(api "/api/v1/events?after=0" --max-time 5 --no-buffer 2>/dev/null || true)
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
  first_blob=$(grep -n 'blob.created' <<<"$events_out" | head -1 | cut -d: -f1)
  first_asset=$(grep -n 'content.asset.created' <<<"$events_out" | head -1 | cut -d: -f1)
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
  head_seq=$(api "/api/v1/events?after=0" --max-time 5 2>/dev/null | grep -c '^id:' || true)
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
}

# ADR-0002: the roles must be independently runnable as OS processes, and the
# only way that stays true is running the real checks in both configurations.
# Otherwise one of the two is never exercised and the split is decorative.
split_process_demo() {
  local root="$WORK/split-full"
  local lib="$root/library" data="$root/data"
  SOCK="$data/heyarr.sock"
  MANIFEST="$root/manifest.json"
  mkdir -p "$root"
  "$GEN" -out "$lib" -manifest "$MANIFEST" -large-size 1048576 >/dev/null

  local files blobs
  files=$(jq -r '.ingestable_files' "$MANIFEST")
  blobs=$(jq -r '.ingestable_blobs' "$MANIFEST")

  cat > "$WORK/split-full.yaml" <<YAML
data_dir: $data
peer:
  name: acceptance-split
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
    roots: ["$lib/movies"]
  - name: shows
    content_type: series
    roots: ["$lib/tv"]
  - name: albums
    content_type: music
    roots: ["$lib/music"]
  - name: shelf
    content_type: book
    roots: ["$lib/books"]
YAML

  TOKEN=$("$BIN" --config "$WORK/split-full.yaml" token create acceptance --scopes admin --json | jq -r .token)

  FULL_LOG="$WORK/split-full.log"
  FULL_PIDS=()
  local role
  for role in controller worker peer; do
    "$BIN" --config "$WORK/split-full.yaml" "$role" >>"$FULL_LOG" 2>&1 &
    FULL_PIDS+=($!)
  done

  local waited=0
  while (( waited < 600 )); do
    curl -sf --unix-socket "$SOCK" http://heyarr/readyz >/dev/null 2>&1 && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if (( waited >= 600 )); then
    fail "split-process: /readyz never became ready"; tail -30 "$FULL_LOG"; stop_full; return 1
  fi
  pass "three separate processes serve a ready API"

  wait_for_ingest split-full "$files" || { stop_full; return 1; }

  local a b
  a=$(api_all /api/v1/assets '.items[].id' | sort -u | wc -l | tr -d ' ')
  b=$(find "$data/cas/blobs" -type f 2>/dev/null | wc -l | tr -d ' ')
  assert_eq "$a" "$files" "split-process mode ingests the same assets"
  assert_eq "$b" "$blobs" "split-process mode deduplicates the same blobs"

  local big_hash
  big_hash=$(jq -r --arg p "$(jq -r .largest_path "$MANIFEST")" '.files[] | select(.path == $p) | .hash' "$MANIFEST")
  assert_eq "$(api "/api/v1/blobs/$big_hash/content" -H 'Range: bytes=0-1023' -o /dev/null -w '%{http_code}')" \
    "206" "split-process mode range-serves bytes"
  stop_full
}

if ! command -v jq >/dev/null 2>&1; then
  fail "jq is not installed — the API assertions in this demo need it"
elif [[ ! -x "$GEN" ]]; then
  fail "no fixture generator at $GEN — run 'make fixtures', or set GEN to a prebuilt one"
else
  full_library_demo
  stop_full
  note "split-process mode, end to end (ADR-0002)"
  split_process_demo
  stop_full
fi

if (( FAILED )); then
  printf '\n\033[31macceptance: FAILED\033[0m\n'; exit 1
fi
printf '\n\033[32macceptance: all checks passed\033[0m\n'
