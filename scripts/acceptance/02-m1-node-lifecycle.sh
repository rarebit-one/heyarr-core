# shellcheck shell=bash
# A section of scripts/acceptance.sh: sourced by it, in order, never run alone.
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

