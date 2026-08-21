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
V1=$(grep -aom1 '"schema_version":[0-9]*' "$WORK/all.log" | cut -d: -f2)
run_and_term restart 5 all >/dev/null
V2=$(grep -aom1 '"schema_version":[0-9]*' "$WORK/restart.log" | cut -d: -f2)
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

# What the WORKER advertises has to come from what it actually resolved
# (ADR-0023), and that join is one line in worker.go that no unit test can
# reach: the runtime's capability filtering is tested, the toolchain's
# capability list is tested, and deleting the line that connects them left
# every one of those tests passing. This is the assertion that noticed.
#
# It asserts on the RUNTIME's own startup line — the value it will actually
# claim with — and not on anything the worker re-derives for logging. The first
# version of this check did the latter, and the sabotage passed: the log agreed
# with itself while the wiring between it and the runtime was cut.
#
# A log line rather than an API because Milestone 1's peer model has no
# capability advertisement; there is nowhere else to observe what a worker will
# claim.
if command -v ffprobe >/dev/null 2>&1; then
  assert_contains "$ALLLOG" '"capabilities":["ffprobe","ffmpeg"]' \
    "a worker with a toolchain claims with it"
else
  # [] and not null. "Advertises nothing" is a deliberate state; null reads as
  # "never asked", and the two need to be tellable apart in a log someone is
  # reading because probing is not happening.
  assert_contains "$ALLLOG" '"capabilities":[]' \
    "a worker with no toolchain claims with nothing rather than defaulting"
fi

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
  fi

  note "  the playback planner (§68)"
  # The planner is a pure function, exhaustively table-tested. What this adds
  # is the join: real probe rows, real device profiles and real replicas, over
  # a real socket — because each of those three being right in isolation is
  # how a wiring bug survives.
  local plan_device limited_device plan_json
  plan_asset=$(api_all /api/v1/assets '.items[] | select(.filename != null) | select(.filename | endswith(".mkv") or endswith(".mp4")) | .id' | head -1)

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
  fi

  # §31: with exactly one peer (ADR-0010) every replica is local, which is the
  # only locality case that can be asserted against reality today.
  assert_eq "$(jq -r '.remote' <<<"$plan_json")" "false" \
    "the single-peer deployment plans from a local replica"
  assert_contains "$(jq -r '.peer_id' <<<"$plan_json")" "-" \
    "the plan names the peer it would serve from"

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
  acq_events=$(api "/api/v1/events?after=0" --max-time 5 --no-buffer 2>/dev/null || true)
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
    assert_contains "$(cat "$bare_log")" '"capabilities":[]' \
      "the bare worker advertises nothing"
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
  fi

  note "  remuxing (§10, §75)"
  # A remux is the case the planner returns most often and the one that costs
  # almost nothing to serve. This drives it through the real binary: queue the
  # job, wait for a capable worker to run it, and confirm a NEW asset appeared
  # on the same edition in a container the device actually declares.
  if command -v ffmpeg >/dev/null 2>&1 && command -v ffprobe >/dev/null 2>&1; then
    local mkv_asset mkv_edition remux_device remux_job remux_code derived_before derived_after
    mkv_asset=$(api_all /api/v1/assets '.items[] | select(.filename != null) | select(.filename | endswith(".mkv")) | .id' | head -1)
    mkv_edition=$(api "/api/v1/assets/$mkv_asset" | jq -r '.edition_id')

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

# The gate is only a gate if people run it, and people stop running a gate
# they have to wait for. Milestone 1 finished at about fifteen seconds and 100
# assertions; Milestone 2 adds probing, remuxing and a second worker process,
# all of which cost real time.
#
# The budget is generous rather than tight — a loaded CI runner is slower than
# a laptop and this must not be flaky — but it exists so that a change which
# doubles the runtime is noticed by CI rather than by whoever stops running
# `make demo` six weeks later.
DEMO_BUDGET_SECONDS=${DEMO_BUDGET_SECONDS:-240}
DEMO_STARTED=$SECONDS

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

DEMO_ELAPSED=$(( SECONDS - DEMO_STARTED ))
if (( DEMO_ELAPSED > DEMO_BUDGET_SECONDS )); then
  fail "the demo took ${DEMO_ELAPSED}s, past its ${DEMO_BUDGET_SECONDS}s budget"
  printf '       A gate nobody waits for is a gate people stop running. Either make it\n'
  printf '       faster or raise DEMO_BUDGET_SECONDS deliberately, in a commit that says why.\n'
else
  pass "the demo finished in ${DEMO_ELAPSED}s, within its ${DEMO_BUDGET_SECONDS}s budget"
fi

if (( FAILED )); then
  printf '\n\033[31macceptance: FAILED\033[0m\n'; exit 1
fi
printf '\n\033[32macceptance: all checks passed\033[0m\n'
