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
trap 'rm -rf "$WORK"' EXIT

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

note "not yet implemented (milestone 1)"
echo "  --   library scan, range serving, end-to-end idempotency, integrity: M1-18"

if (( FAILED )); then
  printf '\n\033[31macceptance: FAILED\033[0m\n'; exit 1
fi
printf '\n\033[32macceptance: all checks passed\033[0m\n'
