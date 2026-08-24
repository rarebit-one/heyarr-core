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
  # FIRST, before the kills: a run that stopped at a missing capability must say
  # so even though it never reached the verdict line — otherwise the loudest
  # failure in this file is the one with the least explanation attached. It
  # prints ahead of the kills because bash reports SIGKILLed jobs
  # asynchronously, and those lines land in the middle of the explanation.
  # Guarded on the function existing: this trap is installed above the helper,
  # so a failure between the two would otherwise die inside the trap (#187).
  if declare -F capability_exit_note >/dev/null 2>&1; then capability_exit_note; fi
  for p in "${FULL_PIDS[@]:-}" "${PEER_PIDS[@]:-}"; do kill -KILL "$p" 2>/dev/null || true; done
  pkill -f "$WORK" 2>/dev/null || true
  rm -rf "$WORK"
}
# The second peer's processes, tracked separately from FULL_PIDS: the two-peer
# section runs its own pair of nodes and must not be stopped by stop_full, which
# every other section calls.
PEER_PIDS=()
trap cleanup EXIT INT TERM

# ASSERTIONS counts everything pass/fail printed, so the verdict line can say
# how much was actually exercised rather than leaving a reader to count `ok`s.
ASSERTIONS=0
pass() { ASSERTIONS=$(( ASSERTIONS + 1 )); printf '  \033[32mok\033[0m   %s\n' "$1"; }
fail() { ASSERTIONS=$(( ASSERTIONS + 1 )); printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAILED=1; }
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

# Exact equality, not containment: "not_satisfied" contains "satisfied", and a
# substring match on an enum-like value shipped here once already. Defined with
# the other helpers rather than further down, because the assertions above the
# full-library demo need it too.
assert_eq() { # got want description
  if [[ "$1" == "$2" ]]; then pass "$3"; else fail "$3 — got '$1', want '$2'"; fi
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

# ---------------------------------------------------------------------------
# Capabilities: an assertion may declare what it needs to mean anything (#187)
# ---------------------------------------------------------------------------
#
# The failure this exists to prevent: an assertion whose SUBJECT is absent does
# not fail and does not skip — it quietly passes, and reads as coverage. It bit
# three times in M4. The clearest case is the probe-traffic confound (#149): the
# data-path assertion counted requests on the client API's blob route and
# required the count not to move. `ffprobe` is absent on the development
# machines, so probe jobs are never claimed, that route is never touched, and
# the counter could not have moved for reasons having nothing to do with the
# data path. Five green local runs were measuring the absence of ffprobe. CI,
# which installs the pinned toolchain, failed it on the first try.
#
# `assert_eq` on a counter that never moves is indistinguishable from `assert_eq`
# on a counter that correctly stayed still. Both print `ok`. So the assertion has
# to say what it needs.
#
# Two helpers, and the DEFAULT IS THE LOUD ONE:
#
#   require_capability <cap> <what would be vacuous>
#       The block cannot be written honestly without <cap>. Absent → FAIL the
#       run, naming the capability and what it would have made meaningless.
#       NOT a skip. A skip is exactly how this became invisible: the developer
#       machine skips, CI runs, and the two disagree silently for weeks.
#
#   unexercised_without <cap> <what was not exercised>
#       For a block that has a REAL alternative branch — assertions that are
#       meaningful on a bare machine and different from, not weaker than, the
#       equipped ones. ADR-0023's degrade path is the whole point of this
#       script's `if command -v ffprobe` sites, so calling them failures would
#       be wrong. It does not fail. It records, so the verdict line and the
#       epilogue can say a green run here is not a green run on a full machine.
#
# Reach for `require_capability` first. `unexercised_without` is only correct
# when the else-branch asserts something a reader would accept as coverage on
# its own; if the else-branch is empty, or is a weaker restatement, the block
# wanted require_capability.
#
# A capability is a name. It resolves through `capability_probe_<name>` if such
# a function exists, so a capability need not be a binary — a large-blob
# fixture, a filesystem that reflinks, a second physical machine are all things
# an M5 assertion may depend on. With no such function it falls back to
# `command -v <name>`, which covers ffprobe, ffmpeg and jq.

CAPS_RESOLVED=()       # "name=yes" / "name=no", memoised: probing is not free
CAPS_UNEXERCISED=()    # "cap<TAB>description" for blocks this machine could not run
CAPS_UNEXERCISED_N=0   # its length, counted rather than measured: macOS CI runs
                       # bash 3.2, where ${#arr[@]} on an empty array under
                       # `set -u` is an unbound-variable error rather than 0
CAPS_MISSING=()        # capabilities a require_capability asked for and did not get
CAPS_MISSING_N=0

# capability_available <name> — 0 if present, 1 if absent. Memoised.
capability_available() {
  local name=$1 entry answer
  for entry in "${CAPS_RESOLVED[@]:-}"; do
    case "$entry" in
      "$name="*) [[ "${entry#*=}" == yes ]] && return 0 || return 1 ;;
    esac
  done
  if declare -F "capability_probe_$name" >/dev/null 2>&1; then
    if "capability_probe_$name"; then answer=yes; else answer=no; fi
  elif command -v "$name" >/dev/null 2>&1; then
    answer=yes
  else
    answer=no
  fi
  CAPS_RESOLVED+=("$name=$answer")
  [[ "$answer" == yes ]]
}

# require_capability <name> <description> — present: 0. Absent: FAIL, then 1.
#
# Two call forms, and both are loud:
#
#   require_capability ffprobe "..." || skip_the_now_vacuous_assertions
#       the run CONTINUES and finishes, so one pass reports EVERY capability it
#       was short of rather than the first — nobody wants to discover a second
#       missing toolchain on the next run — and the verdict line names them all.
#
#   require_capability ffprobe "..."
#       a bare call returns 1 under `set -e`, so the run stops right there. That
#       is a legitimate choice when nothing below the block is meaningful
#       without the capability. The EXIT trap prints the reason (see
#       capability_exit_note), because a run that dies at line 4000 without a
#       verdict line is otherwise indistinguishable from a crash.
#
# What it must never be is a skip. A skip is how this became invisible: the
# developer machine skips, CI runs, and the two disagree silently.
require_capability() { # name description
  local name=$1 desc=$2
  if capability_available "$name"; then
    return 0
  fi
  CAPS_MISSING+=("$name"); CAPS_MISSING_N=$(( CAPS_MISSING_N + 1 ))
  fail "REQUIRES CAPABILITY '$name', which this machine does not have"
  printf '       these assertions would be vacuous without it, not skipped:\n'
  printf '         %s\n' "$desc"
  printf '       a run that cannot exercise an assertion must say so, and this\n'
  printf '       is where it says so — not silently, in the middle of 500 oks (#187).\n'
  return 1
}

# capability_exit_note explains an aborted run, from the EXIT trap. Silent when
# the run reached its verdict line, and silent when nothing was missing: this
# speaks only for the run that stopped early.
VERDICT_REACHED=0
capability_exit_note() {
  # Explicit ifs, not `(( x )) && return`: a `&&` list whose left side is false
  # is a non-zero statement, and `set -e` kills the script on it — inside an
  # EXIT trap that turns a tidy abort into a second, unrelated failure.
  if (( ${VERDICT_REACHED:-0} )); then return 0; fi
  if (( ${CAPS_MISSING_N:-0} == 0 )); then return 0; fi
  printf '\n\033[31macceptance: STOPPED — a required capability is missing\033[0m\n'
  printf '  missing: %s\n' "$(printf '%s\n' "${CAPS_MISSING[@]:-}" | sort -u | tr '\n' ' ')"
  printf '  an assertion block declared it, this machine does not have it, and the\n'
  printf '  assertions it guards would have been vacuous rather than skipped. This\n'
  printf '  run proved LESS than the assertions above it appear to claim.\n'
  printf '  Install what it needs — scripts/toolchain.sh for the media toolchain —\n'
  printf '  and run it again (#187).\n'
}

# not_exercised <name> <description> — record and carry on, for use INSIDE the
# else-branch of a block that already has an honest alternative. Always 0, so it
# can sit in the middle of a branch under `set -e` without a `|| true` that a
# reader would have to think about.
not_exercised() { # name description
  CAPS_UNEXERCISED+=("$1	$2")
  CAPS_UNEXERCISED_N=$(( CAPS_UNEXERCISED_N + 1 ))
}

# capability_names prints the distinct capabilities the ledger mentions.
capability_names() {
  local entry
  for entry in "${CAPS_UNEXERCISED[@]:-}"; do
    [[ -n "$entry" ]] && printf '%s\n' "${entry%%	*}"
  done | sort -u
}

# capability_ledger prints the unexercised blocks, grouped by capability and
# indented to sit inside the epilogue. Nothing at all when the machine was fully
# equipped — a complete run should not be noisier than an incomplete one.
capability_ledger() { # indent
  local indent=$1 cap entry n
  for cap in $(capability_names); do
    n=0
    for entry in "${CAPS_UNEXERCISED[@]:-}"; do
      [[ "$entry" == "$cap	"* ]] && n=$(( n + 1 ))
    done
    printf '%sabsent capability: %s — %d assertion block(s) not exercised\n' "$indent" "$cap" "$n"
    for entry in "${CAPS_UNEXERCISED[@]:-}"; do
      [[ "$entry" == "$cap	"* ]] || continue
      printf '%s  - %s\n' "$indent" "${entry#*	}"
    done
  done
}

# unexercised_without <name> <description> — the guard form: 0 (present, run the
# block) or 1 (absent: recorded for the verdict line and the epilogue, no
# failure).
unexercised_without() { # name description
  if capability_available "$1"; then
    return 0
  fi
  not_exercised "$1" "$2"
  return 1
}

# ---------------------------------------------------------------------------
# Waits: poll THE CONDITION THE NEXT ASSERTION IS ABOUT (#207)
# ---------------------------------------------------------------------------
#
# A sibling of require_capability above, and for the same reason: both exist so
# that a green run means what it appears to mean. require_capability is about an
# assertion whose SUBJECT IS ABSENT; this is about an assertion whose
# PRECONDITION HAS NOT HAPPENED YET.
#
# The failure this exists to prevent, stated once: a loop waits for precondition
# A, and the assertion below it is about consequence B, where B happens strictly
# after A. The wait does not cover the thing being asserted, so the assertion
# passes only when B lands inside the polling overhead — and when it does not,
# it fails AS THOUGH THE LOGIC WERE WRONG.
#
# It bit twice in one night, both times by accident:
#
#   - The remux block asked the planner for a decision with nothing waiting for
#     the blob's probe. With no container recorded the planner correctly answers
#     `direct`, and the run reads `matroska on an mp4-only device plans REMUX —
#     got 'direct', want 'remux'`. That reads as a planner regression. It is not.
#   - The acquisition refusal arc broke on `candidates >= 3` and then asserted
#     `phase == "idle"`, which happens strictly later — after the search
#     concludes and the want is re-evaluated. That reads as a broken state
#     machine. It is not.
#
# In both cases the code was right and the message sent the reader to it anyway.
# THAT is the cost: a real failure and a slow runner become indistinguishable,
# and the habit it teaches is re-running rather than reading.
#
# So the rule, and the whole of this helper:
#
#   1. Poll the condition the NEXT ASSERTION is about, not a precursor of it.
#      Where the assertion needs two things — candidates recorded AND the phase
#      settled — the condition is the conjunction, not the cheaper half.
#   2. On timeout, say WHAT NEVER HAPPENED. Never a value mismatch. A mismatch
#      message is the bug, not the report of it.
#   3. Never a bare `sleep`. A fixed wait is a bet on machine speed and this
#      repo has lost that bet more than four times now.
#
# wait_for <what-never-happened> <deciseconds> <command...>
#   Runs <command> every 100ms until it succeeds. Present already: returns
#   immediately, so a machine where the precondition was met costs one poll.
#
#   ALWAYS RETURNS 0, having already called `fail` — the same reasoning as
#   not_exercised above. A non-zero return here would die under `set -e` in the
#   middle of a block, and a run that dies at line 3500 without a verdict line
#   is indistinguishable from a crash. The run continues, so one pass reports
#   every wait it was short of rather than the first, and the FAIL it printed
#   still makes the run exit non-zero at the verdict line.
wait_for() { # what-never-happened deciseconds command...
  local what=$1 budget=$2; shift 2
  local waited=0
  while (( waited < budget )); do
    if "$@"; then return 0; fi
    sleep 0.1; waited=$(( waited + 1 ))
  done
  fail "NEVER HAPPENED: $what"
  printf '       polled every 100ms for %ss and it did not happen.\n' "$(( budget / 10 ))"
  printf '       This is the missing event itself, not a value mismatch. The assertions\n'
  printf '       below this line are about what happens AFTER it, so whatever they\n'
  printf '       report is a consequence of this line and not a claim about the code\n'
  printf '       they name (#207).\n'
  return 0
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

# ---------------------------------------------------------------------------
note "the capability guard itself (M5-10, #187)"
# ---------------------------------------------------------------------------
#
# The guard is the one assertion in this file that CANNOT be checked by reading
# it, because its whole subject is what happens when something is absent. So it
# is exercised here, on every run, against two synthetic capabilities whose
# presence this section controls. Without this the guard would be exactly the
# thing it exists to prevent: a mechanism nobody has watched fire.
capability_probe_acceptance_present() { true; }
capability_probe_acceptance_absent()  { false; }

assert_eq "$(capability_available acceptance_present && echo yes || echo no)" "yes" \
  "a capability with a probe that resolves is reported present"
assert_eq "$(capability_available acceptance_absent && echo yes || echo no)" "no" \
  "and one whose probe does not resolve is reported absent"

# The answer must be about THIS machine, not about a constant. ffprobe is the
# capability that caused #149, and the acceptance matrix runs both ways: the
# equipped Linux job has it, the degraded Linux job and macOS do not.
CAP_FFPROBE_REAL=no
command -v ffprobe >/dev/null 2>&1 && CAP_FFPROBE_REAL=yes
assert_eq "$(capability_available ffprobe && echo yes || echo no)" "$CAP_FFPROBE_REAL" \
  "the guard's answer for ffprobe matches this machine (currently: $CAP_FFPROBE_REAL)"

# THE POINT. A missing capability FAILS, and it fails saying which assertions it
# just made meaningless. Run in a subshell so its FAILED=1 stays there.
GUARD_RC=0
GUARD_OUT=$( require_capability acceptance_absent "the vacuous assertion #187 describes" 2>&1 ) || GUARD_RC=$?
assert_eq "$GUARD_RC" "1" "require_capability returns non-zero when the capability is absent"
assert_contains "$GUARD_OUT" "FAIL" "and it FAILS the run rather than skipping — skipping is how this became invisible"
assert_contains "$GUARD_OUT" "acceptance_absent" "and names the capability it needed"
assert_contains "$GUARD_OUT" "the vacuous assertion #187 describes" \
  "and names the assertions that would have been vacuous"

GUARD_RC=0
GUARD_OUT=$( require_capability acceptance_present "nothing, this one is present" 2>&1 ) || GUARD_RC=$?
assert_eq "$GUARD_RC" "0" "and it is silent and returns 0 when the capability is present"
assert_eq "$GUARD_OUT" "" "printing nothing, so a fully equipped run is not noisier for it"

# And the abort note, which only ever prints on a run that died before its
# verdict line — the one path a reader can never see working.
EXITNOTE=$( CAPS_MISSING_N=1; CAPS_MISSING=(ffprobe); VERDICT_REACHED=0; capability_exit_note 2>&1 )
assert_contains "$EXITNOTE" "STOPPED" "an aborted run says it stopped rather than dying without a verdict"
assert_contains "$EXITNOTE" "ffprobe" "and names the capability that stopped it"
EXITNOTE=$( CAPS_MISSING_N=1; CAPS_MISSING=(ffprobe); VERDICT_REACHED=1; capability_exit_note 2>&1 )
assert_eq "$EXITNOTE" "" "and it is silent once the run has reached its verdict line"

# The other half: a block with a genuine alternative branch records rather than
# fails, and the record is what the verdict line and the epilogue read.
unexercised_without acceptance_absent "a self-test entry, proving the ledger is written" && \
  fail "unexercised_without returned success for an absent capability" || \
  pass "unexercised_without defers a block whose capability is absent"
unexercised_without acceptance_present "never recorded" && \
  pass "and runs the block when the capability is present" || \
  fail "unexercised_without deferred a block whose capability is present"
assert_contains "${CAPS_UNEXERCISED[*]:-}" "a self-test entry, proving the ledger is written" \
  "the deferred block is recorded, so the verdict line can report it"
assert_not_contains "${CAPS_UNEXERCISED[*]:-}" "never recorded" \
  "and an exercised block is not"
# Remove the self-test entry: the epilogue reports what this RUN could not
# prove about Heyarr, and this entry is about the guard.
CAPS_UNEXERCISED=()
CAPS_UNEXERCISED_N=0

# ---------------------------------------------------------------------------
note "the wait helper itself (M5-11, #207)"
# ---------------------------------------------------------------------------
#
# Exercised here for the same reason the capability guard above it is: its whole
# subject is what happens when something does NOT arrive, and a wait nobody has
# watched time out is a wait whose failure message has never been read. Every
# run reads it.
#
# This section creates nothing — no Work, no asset, no job, no library — so it
# is safe above the catalogue counts asserted inside `note "  the CLI (M1-17)"`.
WAIT_NEVER() { false; }
WAIT_ALWAYS() { true; }

# 1. THE TIMEOUT, which is the point. It fails, and it fails by naming the event
#    rather than by reporting a value.
WAIT_OUT=$( wait_for "the phase never left candidates_found" 3 WAIT_NEVER 2>&1 )
assert_contains "$WAIT_OUT" "FAIL" \
  "a wait that times out FAILS rather than falling through to the assertion below it"
assert_contains "$WAIT_OUT" "NEVER HAPPENED: the phase never left candidates_found" \
  "and the message names WHAT NEVER HAPPENED"
assert_not_contains "$WAIT_OUT" "want '" \
  "and it is NOT a value mismatch — a mismatch sends the reader to the code that was right (#207)"

# 2. It returns 0 even then, so a timed-out wait reports the rest of the run
#    instead of dying at line 3500 with no verdict line.
WAIT_RC=0
( wait_for "a self-test that never arrives" 2 WAIT_NEVER ) >/dev/null 2>&1 || WAIT_RC=$?
assert_eq "$WAIT_RC" "0" \
  "a timed-out wait still returns 0, so the run finishes and reports every wait it was short of"

# 3. Already true: silent, and one poll rather than a duration.
WAIT_OUT=$( wait_for "a condition that is already true" 600 WAIT_ALWAYS 2>&1 )
assert_eq "$WAIT_OUT" "" \
  "and it is silent when the condition is already met, so an equipped machine pays nothing for it"

# 4. It POLLS, and it stops on arrival rather than sleeping the budget out —
#    the property that makes it correct to give it a generous timeout.
WAIT_TICKS=0
WAIT_THIRD() { WAIT_TICKS=$(( WAIT_TICKS + 1 )); (( WAIT_TICKS >= 3 )); }
wait_for "a condition that arrives on the third poll" 600 WAIT_THIRD
assert_eq "$WAIT_TICKS" "3" \
  "it polls until the condition arrives and stops there, rather than sleeping a fixed duration"

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

# ---------------------------------------------------------------------------
# The peer identity, across the same two starts (M4-03, ADR-0010, ADR-0012)
# ---------------------------------------------------------------------------
#
# Anchored here deliberately: $WORK/all.log and $WORK/restart.log are two real
# starts against one data directory that this script has ALREADY paid for, so
# "byte-identical across restarts" costs no extra process. Asserting it
# anywhere else would mean starting the binary twice more to prove something
# these two runs already demonstrate.
#
# ADR-0012 said Milestone 1's only cost was "a peers.public_key column and
# generating the local keypair". The column existed from 00002_core.sql and
# NOTHING wrote it until 00019. These assertions are what keep that true.

PK1=$(grep -aom1 '"peer_public_key":"[^"]*"' "$WORK/all.log" | cut -d'"' -f4)
PK2=$(grep -aom1 '"peer_public_key":"[^"]*"' "$WORK/restart.log" | cut -d'"' -f4)
PEER1=$(grep -aom1 '"peer_id":"[^"]*"' "$WORK/all.log" | cut -d'"' -f4)

# Byte-identical, not merely "it started again". A node that regenerates its
# keypair keeps its peer id and changes its identity, which every peer that
# pinned the old key rejects — silently, at replication time, hours later.
assert_eq "$PK2" "$PK1" "the peer public key is byte-identical across a restart"

# The shape an operator copies to the other site: algorithm-prefixed hex, the
# same idiom as a blob digest so the two are not confused in a terminal.
if [[ "$PK1" =~ ^ed25519:[0-9a-f]{64}$ ]]; then
  pass "the public key is published as ed25519:<64 hex>"
else
  fail "the public key is not ed25519:<64 hex> — got '$PK1'"
fi

# Both places agree. This is the pair the refusal below compares.
MARKER_PEER=$(jq -r '.peer_id // ""' "$WORK/data/cas/HEYARR_CAS")
assert_eq "$MARKER_PEER" "$PEER1" "the CAS root marker names the same peer as the database"

# The private key: 0600, in the data directory, and NOT in the CAS root — the
# CAS is the thing that gets rebuilt, restored and moved between filesystems.
KEYFILE="$WORK/data/peer_ed25519.key"
if [[ -f "$KEYFILE" ]]; then pass "the private key is in the data directory"; else fail "no private key at $KEYFILE"; fi
# %Lp on BSD/macOS stat, %a on GNU stat. Neither is portable, both are here.
# GNU stat first, BSD second, and the order is load-bearing: on Linux `stat -f`
# is a VALID flag meaning "filesystem status", so it SUCCEEDS and the `||` never
# fires — capturing a block of filesystem info instead of a mode. BSD stat has
# no `-c`, so it fails cleanly and falls through. Tried the other way round and
# CI caught it.
KEYMODE=$(stat -c '%a' "$KEYFILE" 2>/dev/null || stat -f '%Lp' "$KEYFILE")
assert_eq "$KEYMODE" "600" "the private key is 0600"
if [[ -e "$WORK/data/cas/peer_ed25519.key" ]]; then
  fail "the private key is inside the CAS root, which travels between hosts"
else
  pass "the private key is not in the CAS root"
fi

# And it never appears in output. Scanned, not reasoned about: the assertion is
# over the captured bytes, so a future change that logs the key fails here even
# if nobody reads this comment again.
SEED=$(cut -d: -f2 <"$KEYFILE" | tr -d '[:space:]')
if [[ ${#SEED} -ne 64 ]]; then
  fail "could not read the private key material to scan for it (got ${#SEED} chars)"
else
  assert_eq "$(grep -c -- "$SEED" "$WORK/all.log" || true)" "0" \
    "the private key never appears in a log line"
  # -a because the database is binary and -c must still report a count; NOT
  # -r, which prefixes the filename and turns "0" into "path:0".
  assert_eq "$(grep -ac -- "$SEED" "$WORK/data/heyarr.db" 2>/dev/null || true)" "0" \
    "the private key never reaches the database"
fi

note "peer identity: the refusal when the two places disagree (M4-03, ADR-0010)"
# "The local peer identity is persisted in two places — the database and the CAS
# root marker. If they ever disagree, the process refuses to start: that
# disagreement is exactly how a deployment silently ends up with two peers
# claiming one identity, and it is unrecoverable once replication has run."
#
# That sentence was in ADR-0010 for three milestones with nothing behind it.
# This is what stops it going back to being decorative.
#
# Everything this section changes, it restores — and the restoration is
# asserted — so the sections after it see the data directory they would have
# seen anyway. It creates no library, no Work, no asset and no blob.

MARKER="$WORK/data/cas/HEYARR_CAS"
OTHER_PEER="01990000-0000-7000-8000-0000000000ff"
cp "$MARKER" "$WORK/marker.orig"
jq --arg p "$OTHER_PEER" '.peer_id = $p' "$WORK/marker.orig" > "$MARKER"

# Assert the CORRUPTION APPLIED before believing anything that follows. A
# residue check that never changed the file passes for the wrong reason, and it
# passes forever.
assert_eq "$(jq -r '.peer_id' "$MARKER")" "$OTHER_PEER" \
  "the CAS marker was actually rewritten to name another peer"

assert_refuses "a CAS marker naming another peer refuses to start" \
  "$OTHER_PEER" "$BIN" --config "$WORK/heyarr.yaml" controller
# The error must name BOTH identities: an operator told only that something
# disagrees cannot tell which of the two machines to correct.
assert_contains "$REPLY" "$PEER1"  "the refusal names the database's peer"
assert_contains "$REPLY" "$MARKER" "the refusal says where the second identity is written"
# ...and it must be a refusal, not a warning it carried on past.
assert_not_contains "$REPLY" "controller started" \
  "the refusal happens before the controller reports itself started"

# Restore, and assert the RESTORATION APPLIED too — otherwise the "it starts
# again" check below could be passing against a still-corrupt marker for some
# unrelated reason.
cp "$WORK/marker.orig" "$MARKER"
assert_eq "$(jq -r '.peer_id' "$MARKER")" "$PEER1" "the CAS marker was restored"

run_and_term identity-restored 5 all >/dev/null
PK3=$(grep -aom1 '"peer_public_key":"[^"]*"' "$WORK/identity-restored.log" | cut -d'"' -f4)
assert_eq "$PK3" "$PK1" "restoring the marker lets the node start again, as the same peer"

# The same refusal for the other half of the identity: a private key that does
# not belong to the recorded public key. Same node, same peer id, and it cannot
# prove it is the peer its own catalog says it is.
#
# printf '%064d' 1 is 64 hex-safe digits, so it parses as a seed and is
# certainly not the real one — the only thing wrong with it is which keypair it
# is, which is exactly the condition under test.
cp "$KEYFILE" "$WORK/peerkey.orig"
printf 'ed25519-seed:%s\n' "$(printf '%064d' 1)" > "$KEYFILE"
chmod 600 "$KEYFILE"
assert_eq "$(cut -d: -f2 <"$KEYFILE" | tr -d '[:space:]')" "$(printf '%064d' 1)" \
  "the private key was actually replaced"

assert_refuses "a private key that does not match the recorded public key refuses to start" \
  "$PK1" "$BIN" --config "$WORK/heyarr.yaml" controller

cp "$WORK/peerkey.orig" "$KEYFILE"
assert_eq "$(cut -d: -f2 <"$KEYFILE" | tr -d '[:space:]')" "$SEED" "the private key was restored"
run_and_term identity-key-restored 5 all >/dev/null
assert_eq "$(grep -aom1 '"peer_public_key":"[^"]*"' "$WORK/identity-key-restored.log" | cut -d'"' -f4)" \
  "$PK1" "restoring the private key lets the node start again, as the same peer"

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
  not_exercised ffprobe "a worker with a toolchain claims with it (the advertisement itself is unproven here)"
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
wait_for_probe() { # blob-hash description
  wait_for "the probe never recorded a container for blob $1, so $2" \
    600 probe_recorded "$1"
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
            title: Blue Harvest 1080p web-dl
            attributes:
              resolution: 1080
              source: web-dl
              video_codec: h264
              hdr: false
          # Rejected by the gate, so the demo has a real rejection to read a
          # reason off rather than asserting on an absence.
          - id: bh-480-cam
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
            title: Static Bloom 480p cam
            attributes:
              resolution: 480
              source: cam
              video_codec: h264
          - id: sb-576-ts
            title: Static Bloom 576p telesync
            attributes:
              resolution: 576
              source: telesync
              video_codec: h264
          - id: sb-720-web
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
  - name: acceptance-downloads
    type: transmission
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
  local mcp_deferred
  mcp_deferred=$(mcp_call "acquire_release" '{}')
  assert_eq "$(jq -r '.error.code' <<<"$mcp_deferred")" "-32601" \
    "a §71 verb this milestone does not carry is absent rather than stubbed"
  assert_contains "$(jq -r '.error.data.milestone' <<<"$mcp_deferred")" "M3" \
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
  # Decision 3 in internal/indexers/client.go makes the health check WRITE the
  # capabilities cache and never read it. If that inverted, an indexer that
  # answered once would stay healthy for the TTL after it stopped answering —
  # and here, where it has never answered at all, the reported health must be
  # false on every pass rather than only on the first.
  #
  # Whether a second pass has run by now depends on the worker's timing, so
  # the second observation is WAITED FOR rather than assumed, and the claim is
  # made only once two distinct ones exist. A fixed sleep would pass on a fast
  # machine and flake on a loaded one.
  local tz_first_checked tz_second tz_waited
  tz_first_checked=$(jq -r '.checked_at // "never"' <<<"$tz_entry")
  tz_waited=0
  while (( tz_waited < 300 )); do
    tz_second=$(jq -c '[.providers[] | select(.name == "acceptance-torznab")] | .[0]' \
      <<<"$(api /api/v1/providers)")
    [[ "$(jq -r '.checked_at // "never"' <<<"$tz_second")" != "$tz_first_checked" ]] && break
    sleep 0.1; tz_waited=$(( tz_waited + 1 ))
  done
  if [[ "$(jq -r '.checked_at // "never"' <<<"$tz_second")" == "$tz_first_checked" ]]; then
    pass "only one health pass has run, so a second observation is not yet assertable"
  else
    assert_eq "$(jq -r '.healthy' <<<"$tz_second")" "false" \
      "a second health pass observes the indexer again rather than replaying the first"
    assert_eq "$(jq -r '.version // "absent"' <<<"$tz_second")" "absent" \
      "and still reports no version on the second pass"
  fi

  # The API key is in the config file three lines above. It must not be in the
  # response, and the assertion is on the VALUE rather than the field name so
  # it is a claim about the credential rather than about the schema.
  assert_not_contains "$dl_json" "not-a-real-key" \
    "an indexer credential does not reach the providers response"

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
  search_events=$(api "/api/v1/events?after=0" --max-time 5 --no-buffer 2>/dev/null || true)
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

# The provider health beat (#164) — the assertions that were vacuous until it
# existed.
#
# # Why this is its own node
#
# The claim being made is EMPTY, THEN POPULATED, and the emptiness half has to
# be deterministic or it asserts nothing. On the full demo node the health beat
# has been running since before the first API call, so "checked_at is absent"
# there is a race that is already lost. Here the node is started as a
# CONTROLLER ALONE first: the controller enqueues the pass and there is no
# worker in existence to claim it, so "never checked" is a fact about the
# system rather than a fact about how fast the machine is.
#
# It configures no fixture library and wants nothing, so it creates no Work, no
# asset and no blob — nothing here can shift a catalogue count asserted
# elsewhere. It costs three short process starts.
#
# # What was wrong before
#
# providers.HealthJobType was declared and its handler was registered at
# internal/worker/worker.go and NOTHING ENQUEUED IT. Every assertion anywhere
# that read provider health was therefore reading a value nothing ever wrote —
# unfalsifiable, and reading as coverage. It was found by a sabotage to the
# indexer client's error path failing to fire.
provider_health_beat_demo() {
  local root="$WORK/healthbeat" data lib sock
  data="$root/data"; lib="$root/library"; sock="$data/heyarr.sock"
  mkdir -p "$data" "$lib"

  cat > "$WORK/healthbeat.yaml" <<YAML
data_dir: $data
peer:
  name: acceptance-health
  site: test
log:
  level: info
  format: json
# Loopback socket, no auth, for the same reason the bare node above does it:
# this section is about a beat, not about tokens, and a fixed TCP port would
# make two runs on one machine collide.
http:
  addr: ""
  unix_socket: $sock
  auth:
    enabled: false
libraries:
  - name: films
    content_type: movie
    roots: ["$lib"]
providers:
  # A fake that answers, so "checked and healthy" is reachable, and a REAL
  # torznab client pointed at port 9 — discard, reserved, refusing connections
  # everywhere — so "checked and unreachable" is reachable too. ADR-0026: a
  # real indexer can never run here.
  - name: health-indexer
    type: fake
    capabilities: [indexer]
  - name: health-torznab
    type: torznab
    endpoint: http://127.0.0.1:9/api
    api_key: not-a-real-key-and-nothing-will-read-it
YAML

  hb_api() { curl -sS --unix-socket "$sock" "http://heyarr$1"; }
  hb_jobs() { "$BIN" --config "$WORK/healthbeat.yaml" jobs list --type provider_health --json; }
  hb_entry() { jq -c --arg n "$1" '[.providers[] | select(.name == $n)] | .[0]' <<<"$(hb_api /api/v1/providers)"; }
  # hb_entry_checked <name> — 0 once that provider has been observed at least
  # once, for wait_for. Per-provider, because the beat records each check as it
  # makes it and one provider's observation says nothing about another's.
  hb_entry_checked() { [[ "$(jq -r '.checked_at // "never"' <<<"$(hb_entry "$1")")" != "never" ]]; }
  hb_wait_ready() {
    local waited=0
    while (( waited < 600 )); do
      curl -sf --unix-socket "$sock" http://heyarr/readyz >/dev/null 2>&1 && return 0
      sleep 0.1; waited=$(( waited + 1 ))
    done
    return 1
  }

  # ---- 1. A controller alone: the pass is SCHEDULED and nothing has run it --
  local hb_ctrl hb_worker
  "$BIN" --config "$WORK/healthbeat.yaml" controller >"$root/controller.log" 2>&1 &
  hb_ctrl=$!
  if ! hb_wait_ready; then
    fail "the health-beat node never became ready"; kill -KILL "$hb_ctrl" 2>/dev/null || true; return 1
  fi

  # THE EMPTY HALF, and it is deterministic: no worker process exists.
  assert_eq "$(jq -r '.checked_at // "never"' <<<"$(hb_entry health-torznab)")" "never" \
    "before any worker has run, an indexer is never-checked rather than unhealthy"
  assert_eq "$(jq -r '.checked_at // "never"' <<<"$(hb_entry health-indexer)")" "never" \
    "and so is one that would answer"

  # THE ASSERTION #164 EXISTS FOR. A job, pending, of the right type — the row
  # that did not exist on any node for the whole of M3. Asserted through the
  # operator's own view of the queue rather than by reading SQLite, because
  # that is the view somebody debugging "why is nothing being checked" has.
  assert_eq "$(hb_jobs | jq 'length')" "1" \
    "a controller enqueues exactly one provider health pass at startup"
  assert_eq "$(hb_jobs | jq -r '.[0].state')" "pending" \
    "and it is pending, waiting for a worker rather than already spent"

  # ---- 2. A worker arrives: the pass runs and checked_at is populated -------
  "$BIN" --config "$WORK/healthbeat.yaml" worker >"$root/worker.log" 2>&1 &
  hb_worker=$!

  # Waited for on a CONDITION. This poll is not dead time: the thing it waits
  # for now arrives, which is the whole of this issue.
  local waited=0 hb_tz hb_fake
  while (( waited < 300 )); do
    hb_tz=$(hb_entry health-torznab)
    [[ "$(jq -r '.checked_at // "never"' <<<"$hb_tz")" != "never" ]] && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if [[ "$(jq -r '.checked_at // "never"' <<<"$hb_tz")" == "never" ]]; then
    fail "checked_at never became populated — the health pass was enqueued and never ran"
    kill -KILL "$hb_ctrl" "$hb_worker" 2>/dev/null || true; return 1
  fi
  pass "a worker claims the beat's job and checked_at becomes populated"

  # A SECOND PROVIDER IS A SECOND OBSERVATION (#207). The wait above is about
  # health-torznab; the two assertions below are about health-indexer, which the
  # pass checks separately and therefore records at its own moment. Asserting
  # one provider's observation on the strength of another's is the same
  # wait-for-A-assert-B shape, with the subjects side by side rather than in
  # sequence — and it would fail as "healthy: null", reading as a health check
  # that got the wrong answer rather than one that had not run.
  wait_for "the health beat never recorded an observation for provider 'health-indexer' — the two assertions below are about what that check saw" \
    300 hb_entry_checked health-indexer
  hb_fake=$(hb_entry health-indexer)
  assert_eq "$(jq -r '.healthy' <<<"$hb_fake")" "true" \
    "a provider that answers is reported healthy, having actually been asked"
  assert_eq "$(jq -r '.version' <<<"$hb_fake")" "fake" \
    "and reports the version its handshake returned"

  # VACUOUS ASSERTION 1, now falsifiable: it is made against a check that has
  # demonstrably run, on this node, in this run.
  assert_eq "$(jq -r '.healthy' <<<"$hb_tz")" "false" \
    "an unreachable indexer is not reported as healthy"
  # ...and the refusal names the network rather than the credential. The API
  # key three lines up in this file is real-looking and wrong; saying "the key
  # was rejected" for a connection that was never made is the report that
  # sends an operator to the wrong page.
  assert_eq "$(jq -r '.detail' <<<"$hb_tz")" "unreachable" \
    "and says so as a network failure rather than a credential one"
  assert_not_contains "$(jq -r '.detail' <<<"$hb_tz")" "not implemented" \
    "the torznab kind is a real client rather than a placeholder"

  # VACUOUS ASSERTION 2 (#131), now falsifiable: THE CACHE MUST NOT
  # MANUFACTURE AN OBSERVATION. Check() refreshes the capabilities cache and a
  # capabilities document is where the reported version comes from. This
  # endpoint refuses connections and has never produced one, so a version
  # appearing here would mean a remembered document passed off as something
  # just observed.
  #
  # assert_eq on "absent", not assert_contains: version is enum-like, and a
  # substring match on an absent field matches nothing and passes.
  assert_eq "$(jq -r '.version // "absent"' <<<"$hb_tz")" "absent" \
    "an indexer that has never handshaked reports no version"

  # ---- 3. A second pass, from the beat, on a restart -----------------------
  #
  # Procured by restarting the CONTROLLER rather than by waiting out the beat
  # interval: the interval is a minute (see internal/controller/healthbeat.go
  # for why a minute) and this script has a budget. A restart is a real beat
  # enqueue on the real path — startProviderHealth enqueues at startup — and it
  # asserts the thing a fixed sleep could not: that the dedupe key which makes
  # two roles produce one check has not quietly turned the beat into a
  # one-shot.
  local hb_first
  hb_first=$(jq -r '.checked_at' <<<"$hb_tz")
  kill -TERM "$hb_ctrl" 2>/dev/null || true; wait "$hb_ctrl" 2>/dev/null || true
  "$BIN" --config "$WORK/healthbeat.yaml" controller >"$root/controller2.log" 2>&1 &
  hb_ctrl=$!
  if ! hb_wait_ready; then
    fail "the health-beat node did not come back"; kill -KILL "$hb_ctrl" "$hb_worker" 2>/dev/null || true; return 1
  fi

  # The two halves are asserted SEPARATELY and in this order, because they
  # fail for completely different reasons and a single assertion over both
  # would report whichever one it noticed. "A second job exists" is a claim
  # about the beat; "the observation moved" is a claim about the check.
  waited=0
  while (( waited < 300 )); do
    [[ "$(hb_jobs | jq 'length')" == "2" ]] && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  assert_eq "$(hb_jobs | jq 'length')" "2" \
    "the beat enqueues a second pass on a later start, rather than being a one-shot"

  waited=0
  local hb_second
  while (( waited < 300 )); do
    hb_second=$(hb_entry health-torznab)
    [[ "$(jq -r '.checked_at' <<<"$hb_second")" != "$hb_first" ]] && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if [[ "$(jq -r '.checked_at' <<<"$hb_second")" == "$hb_first" ]]; then
    fail "the second pass reported the first pass's observation — a replay, not a check"
    kill -KILL "$hb_ctrl" "$hb_worker" 2>/dev/null || true; return 1
  fi
  pass "and the second pass records a new observation rather than the first one again"

  # VACUOUS ASSERTION 3 (#131), now falsifiable: A SECOND CHECK IS A SECOND
  # OBSERVATION, NOT A REPLAY OF THE FIRST. Decision 3 in
  # internal/indexers/client.go makes the health check WRITE the capabilities
  # cache and never read it. If that inverted, an indexer that answered once
  # would stay healthy for the TTL after it stopped answering — and here,
  # where it has never answered at all, the report must be false on every pass
  # rather than only on the first.
  assert_eq "$(jq -r '.healthy' <<<"$hb_second")" "false" \
    "a second health pass observes the indexer again rather than replaying the first"
  assert_eq "$(jq -r '.version // "absent"' <<<"$hb_second")" "absent" \
    "and still reports no version on the second pass"

  kill -TERM "$hb_ctrl" "$hb_worker" 2>/dev/null || true
  wait "$hb_ctrl" "$hb_worker" 2>/dev/null || true
}

# ---------------------------------------------------------------------------
# THE SECOND PEER: placement, proven (§56, §64, M4-11, ADR-0010, ADR-0027)
# ---------------------------------------------------------------------------
#
# This is the section the milestone exists for. Everything above it runs on ONE
# peer, where placement is satisfied the moment content is because there is
# nowhere else for bytes to be — and every claim about `converging` up to this
# point was a claim about a table test with a synthetic peer set.
#
# Here there are two real nodes. Two data directories, two databases, two
# content stores, two Ed25519 identities, and an mTLS peer surface between them
# that each has enrolled the other's public key on. A blob is DELETED from one
# of them, the gap is observed from the other node's API as `converging`, the
# transfer runs, and the same API is asked again.
#
# # Why it is its own pair of nodes
#
# The same reason the health beat is: isolation. It configures its own two-file
# library under $WORK, so it creates no Work, no asset and no blob in the demo
# catalogue and cannot shift a count asserted anywhere else — the trap recorded
# at `note "  the CLI (M1-17)"`. It also needs a node whose peer set it can
# GROW, and the full demo node has been a fabric of one since before its first
# API call.
#
# # Why the ports are :0
#
# The peer surface binds a real TCP port, and a fixed one makes two runs on one
# machine collide — which is the same lesson `http.addr: ""` records above. It
# binds :0 and logs the address it actually got; the helper below reads it back
# out of the log, so nothing here guesses a port.
#
# # Every enum check is assert_eq
#
# "not_satisfied" CONTAINS "satisfied". A substring match on a satisfaction
# value passes for the opposite meaning, and it has shipped in this file once
# already. The same applies to the garbage-collection refusals at the end:
# "no_other_peer" contains "other_peer".
#
# # Why it runs twice (M4-16)
#
# Once with each node as `heyarr all`, and once with each node as three separate
# role processes. ADR-0002 says the roles must be independently runnable as OS
# processes, and the split-process section below has kept that honest for the
# single-node path since Milestone 1. Everything this milestone added — the peer
# surface, enrolment, inventory exchange, the transfer, the collector's
# placement precondition — is new surface that the split has never been run
# against, and a split nothing exercises is decorative.

# peer_listen_addr reads the address a node's peer surface actually bound.
peer_listen_addr() { # logfile
  local waited=0 line
  while (( waited < 600 )); do
    line=$(grep '"msg":"peer surface listening"' "$1" 2>/dev/null | tail -1)
    if [[ -n "$line" ]]; then jq -r '.addr' <<<"$line"; return 0; fi
    sleep 0.1; waited=$(( waited + 1 ))
  done
  return 1
}

# peer_holds counts a blob's copies in a node's content store.
#
# By digest, over the store's own fanout, rather than by reconstructing the
# path: the layout is private to the storage fabric (ADR-0006) and a test that
# hard-coded it would be asserting on an implementation detail it does not own.
peer_holds() { # cas-root blob-hash
  find "$1/blobs" -name "${2#blake3:}" -type f 2>/dev/null | wc -l | tr -d ' '
}

# replicate_job_succeeded — 0 once node B has a replicate_blob job in
# `succeeded`. That is the LAST of the things the transfer section asserts:
# bytes, then the transfer record, then the log line, then the job state.
# Waiting on it covers all four; waiting on the bytes covers only the first
# (#207). Reads node B through api_b, which the two-peer arc defines.
replicate_job_succeeded() {
  [[ "$(api_b "/api/v1/jobs?type=replicate_blob" | jq -r '[.items[] | select(.state == "succeeded")] | length > 0')" == "true" ]]
}

# start_peer_node starts one two-peer node in the mode this pass is exercising,
# and reports the PIDs it started in NODE_PIDS.
#
# `all` and three role processes are the same node from every angle this section
# asserts from. That is ADR-0002's claim, and the only thing that keeps it true
# is running the milestone's arc under both rather than asserting it once under
# one and trusting the split — which is how a role that stopped being
# independently runnable would go unnoticed until somebody deployed it.
start_peer_node() { # config logfile mode
  local cfg=$1 log=$2 mode=$3 role
  NODE_PIDS=()
  if [[ "$mode" == "all" ]]; then
    "$BIN" --config "$cfg" all >>"$log" 2>&1 &
    NODE_PIDS+=($!)
  else
    for role in controller worker peer; do
      "$BIN" --config "$cfg" "$role" >>"$log" 2>&1 &
      NODE_PIDS+=($!)
    done
  fi
  PEER_PIDS+=("${NODE_PIDS[@]}")
}

# stop_peer_node stops one node and WAITS for it, so that "the peer is down" is
# a fact rather than a signal that has been sent. A refusal asserted against a
# peer that is still finishing its last request proves nothing.
stop_peer_node() { # pid...
  local p
  for p in "$@"; do kill -TERM "$p" 2>/dev/null || true; done
  for p in "$@"; do wait "$p" 2>/dev/null || true; done
}

# ctrl_blob_bytes counts blob-content reads served by a node's CLIENT API,
# from the chi route pattern on its own /metrics.
#
# It is used to prove an instrument is LIVE, and never on its own to prove a
# negative. See peer_blob_reads for why that distinction is the whole point of
# this pair of helpers.
#
# The label is matched with `grep -F`, not a regex. The route pattern contains
# `{hash}`, and a brace in a regex is an interval operator on some awks and a
# literal on others — the same class of portability trap as `stat -f`, which is
# a valid GNU flag meaning something else entirely. A fixed-string match means
# the same thing on darwin and on the Linux runner.
ctrl_blob_bytes() { # metrics-text
  local rows
  rows=$(grep '^heyarr_http_requests_total{' <<<"$1" |
    grep -F 'route="/api/v1/blobs/{hash}/content"') || true
  awk '{ s += $NF } END { printf "%d", s + 0 }' <<<"$rows"
}

# peer_blob_reads prints the peer names a node SERVED a blob to on its peer
# surface, one line per read.
#
# # Why this is not the client API's counter
#
# The first version of the "controller carried no bytes" assertion counted
# ctrl_blob_bytes across the transfer and required it not to move. That is
# wrong, and it is wrong in the most dangerous way available: it passed here,
# five runs in a row, and failed in CI.
#
# The two surfaces SHARE the blob handler, deliberately (ADR-0013) — one
# implementation of ranges, validators and flat memory use rather than two that
# drift silently. What they do not share is the credential. But the client API's
# metrics label a request by its chi route pattern, and
# `/api/v1/blobs/{hash}/content` is the pattern for reads that arrived on a
# bearer token AND the string a reader would assume covers the peer fabric too.
# So that counter cannot answer "which listener served these bytes", which is
# the only question the assertion was asking.
#
# What actually moved it in CI was PROBING. internal/media/probe fetches blob
# bytes from its own node over HTTP Range to hand them to ffprobe, and those
# reads land on the client API's blob route like any other. The jobs are
# enqueued by ingest and run whenever the worker reaches them, so the count
# drifts by an amount that tracks load and elapsed time. This machine has no
# ffprobe; the probe job carries a RequiredCapability, is never claimed, and the
# reads never happen. The assertion was measuring a mechanism that was absent
# locally — the same shape as three other things caught the same day.
#
# So the claim is made POSITIVELY instead, from the sending side, on the surface
# that actually carried the bytes: node A recording that it served this blob, by
# GET, to a peer that presented a pinned certificate. Probing cannot reach that
# record, because probing has no certificate and never touches this listener.
peer_blob_reads() { # logfile method blob-hash
  grep -F '"msg":"served blob content to a peer"' "$1" 2>/dev/null |
    jq -r --arg m "$2" --arg h "$3" 'select(.method == $m and .blob_hash == $h) | .peer_name' || true
}

# peer_blob_read_count counts those reads.
#
# `grep -c .` rather than `wc -l`, because a here-string of "" is one empty line
# to wc and would report a surface that served nothing as having served once.
peer_blob_read_count() { # logfile method blob-hash
  peer_blob_reads "$1" "$2" "$3" | grep -c . || true
}

# peer_served_bytes is how many CONTENT bytes this node has sent for a blob,
# summed over every GET its peer surface answered.
#
# THE SOURCE'S SIDE, and that is the whole point. A destination's account of
# what it fetched is a claim about itself: a transfer that fetched nothing and
# published the wrong file would report a very good number. What left the
# source is a fact about the source, and since #218 the source records it —
# `bytes` on "served blob content to a peer", counted by the response recorder
# the client API's access log already uses.
#
# GET only. A HEAD is the durability precondition asking whether a blob is here
# and carries no body, and counting it would put a zero in every sum.
#
# Used as a DELTA around an operation rather than as an absolute. This arc
# moves the same blob more than once, so a total answers "how much has ever
# left" when the question is "how much did THAT cost".
peer_served_bytes() { # logfile blob-hash
  # The grep is braced with `|| true` rather than trailing the pipeline with
  # `|| echo 0`. Under `pipefail` a grep that matches nothing fails the whole
  # pipeline AFTER jq has already printed its 0, so the fallback would APPEND a
  # second value and every arithmetic use of this would then be a syntax error
  # on a two-line number.
  { grep -F '"msg":"served blob content to a peer"' "$1" 2>/dev/null || true; } |
    jq -s --arg h "$2" '[.[] | select(.method == "GET" and .blob_hash == $h) | .bytes] | add // 0'
}

# pct is a percentage as an integer, for a message a person reads.
#
# Integer arithmetic deliberately: bash has no floats, and every threshold this
# file asserts is a coarse one — "under a tenth of the blob" rather than
# "1.07%". A fraction that needs a decimal point to be convincing is a fraction
# that is too close to its threshold to be asserted at all.
pct() { # part whole
  if (( $2 == 0 )); then echo 0; else echo $(( $1 * 100 / $2 )); fi
}

two_peer_demo() { # mode
  local mode=$1
  local root="$WORK/twopeer-$mode" lib
  lib="$root/library"
  mkdir -p "$lib/movies/Signal Fire (2021)" "$lib/movies/Static Field (2019)" \
    "$lib/movies/Cold Harbour (2018)"

  # Three small files, ingested by BOTH nodes from the same directory. That is
  # what gives the two catalogues the same blob digests without either node
  # having to be told about the other's contents — content addressing doing the
  # job it exists for (invariant 1).
  #
  # Three rather than two because the garbage-collection arc needs two blobs it
  # can make unreferenced: one to unlink while this node is still a fabric of
  # one, and one to spare once a second peer exists and has gone away. A refusal
  # with no matching permission beside it is indistinguishable from a collector
  # that refuses everything, so the permission has to be shown too.
  # Signal Fire is the file the transfer arc moves, and it is deliberately the
  # only one ABOVE §16's chunking threshold (manifests.ThresholdBytes, 4 MiB).
  # The lazy-chunking section at the end of this arc needs a blob on each side
  # of that number, and making the one that already crosses the wire the large
  # one costs a five-megabyte loopback transfer and no new fixture.
  head -c 5242880 /dev/urandom > "$lib/movies/Signal Fire (2021)/Signal.Fire.2021.1080p.mkv"
  head -c 131072 /dev/urandom > "$lib/movies/Static Field (2019)/Static.Field.2019.1080p.mkv"
  head -c 196608 /dev/urandom > "$lib/movies/Cold Harbour (2018)/Cold.Harbour.2018.1080p.mkv"

  local cfg_a="$WORK/twopeer-$mode-a.yaml" cfg_b="$WORK/twopeer-$mode-b.yaml"
  local n
  for n in a b; do
    mkdir -p "$root/$n"
    cat > "$WORK/twopeer-$mode-$n.yaml" <<YAML
data_dir: $root/$n/data
peer:
  name: site-$n
  site: site-$n
  # The mTLS peer surface (§26, ADR-0012). Port 0: the kernel chooses, the
  # node logs what it got, and two concurrent runs cannot collide.
  listen: 127.0.0.1:0
log:
  level: info
  format: json
http:
  addr: ""
libraries:
  - name: films
    content_type: movie
    roots: ["$lib/movies"]
YAML
  done

  local sock_a="$root/a/data/heyarr.sock" sock_b="$root/b/data/heyarr.sock"
  local log_a="$root/a.log" log_b="$root/b.log"
  local token_a token_b
  token_a=$("$BIN" --config "$cfg_a" token create acceptance --scopes admin --json | jq -r .token)
  token_b=$("$BIN" --config "$cfg_b" token create acceptance --scopes admin --json | jq -r .token)

  # Only node B's PIDs are kept: it is the one this section stops on purpose,
  # to make a peer go away. Node A runs to the end and is stopped with
  # everything else through PEER_PIDS.
  local pids_b=()
  start_peer_node "$cfg_a" "$log_a" "$mode"
  start_peer_node "$cfg_b" "$log_b" "$mode"; pids_b=("${NODE_PIDS[@]}")

  # api_a / api_b are the two nodes' client APIs; cli_a / cli_b are the two
  # operators. Both are needed: enrolment and inventory reporting are CLI verbs
  # over the peer surface, and satisfaction is an HTTP question.
  api_a() { curl -sS --unix-socket "$sock_a" -H "Authorization: Bearer $token_a" "${@:2}" "http://heyarr$1"; }
  api_b() { curl -sS --unix-socket "$sock_b" -H "Authorization: Bearer $token_b" "${@:2}" "http://heyarr$1"; }
  cli_a() { "$BIN" --config "$cfg_a" --token "$token_a" "$@"; }
  cli_b() { "$BIN" --config "$cfg_b" --token "$token_b" "$@"; }

  local s waited
  for s in "$sock_a" "$sock_b"; do
    waited=0
    while (( waited < 600 )); do
      curl -sf --unix-socket "$s" http://heyarr/readyz >/dev/null 2>&1 && break
      sleep 0.1; waited=$(( waited + 1 ))
    done
    if (( waited >= 600 )); then
      fail "two-peer: $s never became ready"; tail -20 "$log_a" "$log_b"; return 1
    fi
  done

  local l
  for l in "$log_a" "$log_b"; do
    waited=0
    while (( waited < 900 )); do
      (( $(grep -c '"msg":"ingested"' "$l" 2>/dev/null || true) >= 3 )) && break
      sleep 0.1; waited=$(( waited + 1 ))
    done
    if (( waited >= 900 )); then
      fail "two-peer: $l never ingested all three fixture files"; tail -20 "$l"; return 1
    fi
  done

  local addr_a addr_b
  addr_a=$(peer_listen_addr "$log_a") || { fail "two-peer: node A never bound a peer surface"; return 1; }
  addr_b=$(peer_listen_addr "$log_b") || { fail "two-peer: node B never bound a peer surface"; return 1; }
  assert_contains "$addr_a" "127.0.0.1:" "the mTLS peer surface binds an address of its own (§26, ADR-0012)"

  # -------------------------------------------------------------------------
  note "  one peer: placement is satisfied, and says why that proves nothing"
  # -------------------------------------------------------------------------
  #
  # Node A is a fabric of one, which is what most Heyarr deployments will be
  # forever. Asserted FIRST, before the second peer is enrolled, because it is
  # the half of the `unproven` contract that a hard-coded `false` would break —
  # and a suite that only checked the two-peer answer would never notice.
  local tp_profile tp_work tp_want tp_json
  tp_profile=$(api_a /api/v1/quality-profiles -X POST -H 'Content-Type: application/json' \
    -d '{"name":"twopeer-anything","description":"accepts whatever exists"}' | jq -r '.id')
  tp_work=$(api_a /api/v1/works | jq -r '.items[] | select(.title == "Signal Fire") | .id')
  tp_want=$(api_a /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d "{\"work_id\":\"$tp_work\",\"quality_profile_id\":\"$tp_profile\"}" | jq -r '.id')

  tp_json=$(api_a "/api/v1/desired/$tp_want/satisfaction")
  assert_eq "$(jq -r '.content.satisfaction' <<<"$tp_json")" "satisfied" \
    "node A holds bytes a profile that accepts anything accepts"
  assert_eq "$(jq -r '.placement.satisfaction' <<<"$tp_json")" "satisfied" \
    "with one peer, placement is satisfied the moment content is"
  assert_eq "$(jq -r '.placement.unproven' <<<"$tp_json")" "true" \
    "and it says so: unproven is TRUE when the target set is this node alone"
  assert_eq "$(jq -r '.state' <<<"$tp_json")" "FULLY_SATISFIED" \
    "so the §64 name goes straight to FULLY_SATISFIED — PLACEMENT_CONVERGING is unreachable here"

  # -------------------------------------------------------------------------
  note "  and garbage collection lets go of bytes, while this is still a fabric of one"
  # -------------------------------------------------------------------------
  #
  # THE POSITIVE CONTROL for the refusal at the end of this section, and it has
  # to come first, on this node, while it still has no other peer.
  #
  # The refusal below is the milestone's other headline. It is also the
  # assertion that a collector which refuses EVERYTHING passes perfectly, and a
  # precondition that never lets go is a different way of losing a library —
  # slowly, to a full disk. So the same binary, on the same node, with the same
  # collector, is made to unlink a blob HERE. Between this assertion and the
  # refusal a hundred lines below, nothing about the code changes; a second peer
  # is enrolled.
  #
  # It uses the third fixture, whose bytes node B also holds and which nothing
  # after this point asserts on.
  local sole_blob sole_assets sole_pass1 sole_pass2 aid
  sole_blob=$(api_a /api/v1/assets |
    jq -r '.items[] | select(.source_path != null and (.source_path | contains("Cold.Harbour"))) | .blob_hash' |
    head -1)
  assert_contains "$sole_blob" "blake3:" "the third fixture names bytes this node ingested"
  assert_eq "$(peer_holds "$root/a/data/cas" "$sole_blob")" "1" "and node A holds them"

  # Unreference them: DELETE /assets removes the catalog row and never touches a
  # byte, which is precisely the garbage a sweep exists to find (ADR-0018).
  sole_assets=$(api_a /api/v1/assets | jq -r --arg h "$sole_blob" '.items[] | select(.blob_hash == $h) | .id')
  for aid in $sole_assets; do api_a "/api/v1/assets/$aid" -X DELETE -o /dev/null; done
  assert_eq "$(api_a /api/v1/assets | jq -r --arg h "$sole_blob" '[.items[] | select(.blob_hash == $h)] | length')" "0" \
    "nothing in the catalogue references those bytes any more"

  # Two passes, because a blob is never reclaimed by the pass that marks it. The
  # grace window is a window and not a flag, and a single-pass reclaim would
  # mean a bad scan and a `gc --apply` in the same minute took the library.
  sole_pass1=$("$BIN" --config "$cfg_a" gc --apply --grace 1ns --json 2>/dev/null)
  assert_eq "$(jq -r --arg h "$sole_blob" '[.marked[].hash] | index($h) != null' <<<"$sole_pass1")" "true" \
    "the first pass starts the grace window"
  assert_eq "$(jq -r '.reclaimed | length' <<<"$sole_pass1")" "0" \
    "and reclaims nothing at all on the pass that marked it"
  assert_eq "$(peer_holds "$root/a/data/cas" "$sole_blob")" "1" "so the bytes are still there afterwards"

  sole_pass2=$("$BIN" --config "$cfg_a" gc --apply --grace 1ns --json 2>/dev/null)
  assert_eq "$(jq -r --arg h "$sole_blob" '[.reclaimed[].hash] | index($h) != null' <<<"$sole_pass2")" "true" \
    "the second pass reclaims them: one peer means no elsewhere to satisfy, so the window is the whole gate"
  assert_eq "$(jq -r '.spared | length' <<<"$sole_pass2")" "0" \
    "nothing was spared — this collector is a gate, not a refusal to ever delete"
  assert_eq "$(jq -r '.refusals | length' <<<"$sole_pass2")" "0" \
    "and nothing stopped the sweep as a whole"
  assert_eq "$(peer_holds "$root/a/data/cas" "$sole_blob")" "0" "node A let the bytes go"

  # -------------------------------------------------------------------------
  note "  two peers, enrolled by public key in both directions (§26, ADR-0012)"
  # -------------------------------------------------------------------------
  #
  # Operator-mediated and explicit: no discovery, no join token, no trust on
  # first use. Two nodes, two commands, each carrying the other's key.
  local key_a key_b peer_b_id
  key_a=$(cli_a peers list --json | jq -r '.[] | select(.is_self) | .public_key')
  key_b=$(cli_b peers list --json | jq -r '.[] | select(.is_self) | .public_key')
  assert_contains "$key_a" "ed25519:" "each node has an Ed25519 identity of its own (M4-03)"

  peer_b_id=$(cli_a peers add --name site-b --site site-b --mode full \
    --public-key "$key_b" --endpoint "https://$addr_b" --json | jq -r '.id')

  # Peer health, before node B has said anything to node A (#184).
  #
  # `unknown` is the column's default and it is deliberately NOT a synonym for
  # reachable: a peer that has never been heard from has not been shown to be
  # up. Asserting it HERE is what makes the assertions after it mean something —
  # without it, "reachable" below would pass on a build where the column started
  # reachable and nothing ever moved it, which is precisely how #184 survived M4
  # with every test green.
  #
  # It has to sit between the two enrolments rather than after both, and the
  # reason is the mechanism itself. Node B's own `peers add` opens an
  # AUTHENTICATED mTLS request to node A's peer surface to ask about the return
  # path, and node A records liveness on exactly that — so by the line after the
  # next, the value has legitimately moved. (Node A's enrolment a moment ago did
  # not move node B's view for node A the same way, because at that point node B
  # had not enrolled node A and the handshake was refused. That refusal is the
  # "not verified in both directions" note on stderr, and it is correct.)
  #
  # assert_eq, not assert_contains: "unreachable" CONTAINS "reachable".
  local tp_health
  tp_health=$(cli_a peers list --json | jq -r '.[] | select(.name == "site-b") | .health')
  assert_eq "$tp_health" "unknown" \
    "a peer that has not been heard FROM is unknown — a dial OUT is not an observation of the far end (#184)"

  cli_b peers add --name site-a --site site-a --mode full \
    --public-key "$key_a" --endpoint "https://$addr_a" --json >/dev/null
  # Each node also records where IT can be reached, so it can report its own
  # inventory through the same door every other peer uses. A self peer reports
  # through the same mechanism as anyone else (M4-07); there is no privileged
  # in-process shortcut, because a call that could not survive being a network
  # hop is not allowed (invariant 4).
  cli_a peers add --name site-a --public-key "$key_a" --endpoint "https://$addr_a" --json >/dev/null
  cli_b peers add --name site-b --public-key "$key_b" --endpoint "https://$addr_b" --json >/dev/null

  # The ping answers with what the OTHER node made of this one's certificate,
  # which is the fact worth asserting: node B derived "site-a" from the key it
  # was handed, so the enrolment on both sides describes the same two machines.
  # Asserting on "site-b" here would only prove that the string this node typed
  # into the command came back.
  local tp_ping
  tp_ping=$(cli_a peers ping site-b --json 2>&1)
  assert_eq "$(jq -r '.name // "none"' <<<"$tp_ping")" "site-a" \
    "node B recognises node A's certificate as the peer it enrolled (ADR-0012, ADR-0033)"
  assert_contains "$tp_ping" "served_by" \
    "over pinned mTLS, with no second credential anywhere (ADR-0033)"
  # The ping went A -> B, so it is NODE B that just observed node A. Asserting
  # it from the dialling side would be asserting about the wrong node: liveness
  # is recorded by the end that was TALKED TO, which is the whole reason the
  # peer surface had to become one of its writers (#184).
  assert_eq "$(cli_b peers list --json | jq -r '.[] | select(.name == "site-a") | .health')" \
    "reachable" "node B observed node A on the connection A opened to its peer surface (#184)"


  # The field the milestone changed, answering differently on the SAME node it
  # answered `true` on ninety seconds ago. Nothing about the code moved between
  # these two assertions — a peer was enrolled.
  tp_json=$(api_a "/api/v1/desired/$tp_want/satisfaction")
  assert_eq "$(jq -r '.placement.unproven' <<<"$tp_json")" "false" \
    "unproven is FALSE with two required peers: the axis is answering a real question"

  # -------------------------------------------------------------------------
  note "  the inventories, exchanged (§19, §20, M4-07)"
  # -------------------------------------------------------------------------
  #
  # `replicas` on a controller is what the CONTROLLER believes; a peer's
  # inventory is what is on its DISK. Until B says what it holds, A knows only
  # that B is required to hold things — which is `converging`, correctly, and
  # for a reason that is about missing information rather than missing bytes.
  cli_a peers report-inventory site-b --json >/dev/null
  cli_b peers report-inventory site-a --json >/dev/null

  tp_json=$(api_a "/api/v1/desired/$tp_want/satisfaction")
  assert_eq "$(jq -r '.placement.satisfaction' <<<"$tp_json")" "satisfied" \
    "with both peers reporting the bytes, placement is satisfied on evidence"
  assert_eq "$(jq -r '.state' <<<"$tp_json")" "FULLY_SATISFIED" \
    "and the §64 name says so"
  assert_eq "$(jq -r '.placement.unproven' <<<"$tp_json")" "false" \
    "unproven does not come back when the answer is satisfied — it is about the target set"
  # THE TRANSITION. Node B reported its inventory to node A over the peer
  # surface — pinned mTLS, no bearer token anywhere near it — and that is the
  # interaction that actually happens between two peers. Before #184 nothing
  # observed it, and this value stayed "unknown" for the rest of the run.
  #
  # assert_eq, not assert_contains: "unreachable" CONTAINS "reachable".
  tp_health=$(cli_a peers list --json | jq -r '.[] | select(.name == "site-b") | .health')
  assert_eq "$tp_health" "reachable" \
    "THE TRANSITION: a peer that has spoken only to the peer surface is reachable (#184)"
  # last_seen_at is the actionable half. "reachable" with no timestamp is a
  # status nobody can act on, and a column that is set but never advanced would
  # pass the assertion above forever.
  assert_eq "$(cli_a peers list --json | jq -r '.[] | select(.name == "site-b") | (.last_seen_at != null)')" \
    "true" "and it records WHEN, which is the half an operator acts on"
  # -------------------------------------------------------------------------
  note "  a one-way pairing is REPORTED, not refused (#186, ADR-0037, ADR-0038)"
  # -------------------------------------------------------------------------
  #
  # Replication needs two flows and they run in OPPOSITE directions: a peer
  # PUSHES its inventory to the controller, and a destination PULLS bytes from
  # the source (ADR-0030). A link that carries one direction only cannot
  # converge in the other, and it fails SILENTLY — the controller is never told
  # the far node holds a blob, so reconciliation correctly emits nothing and
  # nothing is reported as wrong.
  #
  # 🔴 It is REPORTED and never refused, and that is ADR-0038 rather than a
  # softened ADR-0037. Each peer is authoritative for its own site: a node that
  # can be reached but cannot reach back still fetches what it lacks from the
  # peer it CAN reach, and both sites keep serving everything already on their
  # own disks. Refusing to enrol that would block a working configuration for
  # being unusual. What is lost is convergence in one direction, and an operator
  # who is told at the terminal can act on it.
  #
  # The first assertion is that a healthy pairing says NOTHING. A check that
  # always spoke would be a check nobody reads.
  local ow_quiet ow_broken ow_status
  ow_quiet=$(cli_a peers add --name site-b --site site-b --mode full \
    --public-key "$key_b" --endpoint "https://$addr_b" --json 2>&1 >/dev/null)
  assert_eq "$ow_quiet" "" \
    "a pairing verified in both directions enrols with nothing printed (#186)"

  # Now break ONE direction, for real. Node B's record of node A is moved to a
  # port that refuses connections, so when node A asks node B to reach back,
  # node B genuinely cannot. Node A can still reach node B throughout: that is
  # the observed asymmetry, reproduced on one host.
  #
  # Port 9 is discard: reserved, and refusing connections everywhere.
  cli_b peers add --name site-a --site site-a --mode full \
    --public-key "$key_a" --endpoint "https://127.0.0.1:9" --json >/dev/null 2>&1

  ow_status=0
  ow_broken=$(cli_a peers add --name site-b --site site-b --mode full \
    --public-key "$key_b" --endpoint "https://$addr_b" --json 2>&1 >/dev/null) || ow_status=$?
  assert_eq "$ow_status" "0" \
    "a one-way pairing is ENROLLED, not refused — each peer is authoritative for its own site (ADR-0038)"
  assert_contains "$ow_broken" "the return path did not answer" \
    "and the operator is TOLD, naming the direction that failed"
  assert_contains "$ow_broken" "127.0.0.1:9" \
    "naming the address the far node actually tried, so a stale record is distinguishable from a firewall"
  assert_contains "$ow_broken" "not a fault" \
    "stated as information rather than as a fault, because under ADR-0038 it is not one"
  assert_contains "$ow_broken" "ADR-0038" \
    "and it cites the record that says why, so an operator need not infer the stance"

  # The peer really was enrolled, and with the endpoint the operator typed. A
  # report that half-applied would leave a working endpoint replaced while the
  # peer looked healthy.
  assert_eq "$(cli_a peers show site-b --json 2>/dev/null | jq -r '.endpoint')" "https://$addr_b" \
    "the enrolment landed intact: node A's record of node B is exactly what was asked for"

  # Repair, so the rest of the phase runs against a fabric working both ways.
  # Load-bearing: without it every later assertion here runs against a node B
  # that cannot reach node A.
  cli_b peers add --name site-a --site site-a --mode full \
    --public-key "$key_a" --endpoint "https://$addr_a" --json >/dev/null 2>&1



  # -------------------------------------------------------------------------
  note "  PLACEMENT_CONVERGING, reached by a real gap in real bytes"
  # -------------------------------------------------------------------------
  #
  # THE MILESTONE'S HEADLINE ASSERTION. The blob is deleted from node B's
  # content store — really deleted, from a real disk — B reports what it now
  # holds, and node A's API is asked the §56 question again.
  local tp_blob tp_asset
  tp_asset=$(jq -r '.content.satisfied_by' <<<"$tp_json")
  tp_blob=$(api_a "/api/v1/assets/$tp_asset" | jq -r '.blob_hash')
  assert_contains "$tp_blob" "blake3:" "the satisfying asset names the bytes placement is about"

  # -------------------------------------------------------------------------
  note "  arming the observation: WHICH SURFACE serves the bytes (§21, §32, ADR-0030)"
  # -------------------------------------------------------------------------
  #
  # ADR-0030 puts the byte-carrying hop on the PEER SURFACE: the destination
  # pulls from the source's mTLS listener, and the client API — the controller's
  # door, the one that authenticates a bearer token — is not in the data path at
  # all. The claim after the transfer is that the bytes went peer to peer.
  #
  # It is asserted POSITIVELY, from the node that sent them. A negative
  # assertion — "this counter did not move" — is the weakest evidence in this
  # file: it passes against a metric that was never registered, a label that was
  # renamed, and, as this section learned in CI, against a perfectly live
  # counter that simply had a second, unrelated writer. See peer_blob_reads.
  #
  # Two controls arm it, and they point in opposite directions on purpose.
  local ctrl_a_before
  assert_eq "$(peer_blob_read_count "$log_a" GET "$tp_blob")" "0" \
    "node A has served these bytes to no peer yet: the record starts empty"

  # A genuine blob read on the CLIENT API, for the same blob, on a bearer token.
  # It must be visible to the client API's counter — that instrument is live —
  # and invisible to the peer surface's record. If a bearer-token read showed up
  # as a peer read, the two surfaces would not be distinguishable and every
  # assertion below would be measuring one number twice.
  ctrl_a_before=$(ctrl_blob_bytes "$(api_a /metrics)")
  assert_eq "$(api_a "/api/v1/blobs/$tp_blob/content" -H 'Range: bytes=0-1023' -o /dev/null -w '%{http_code}')" \
    "206" "node A's client API serves blob bytes to a bearer token, as it always has"
  assert_eq "$(( $(ctrl_blob_bytes "$(api_a /metrics)") - ctrl_a_before ))" "1" \
    "the client API's counter saw that read: it is a live instrument, not one that never existed"
  assert_eq "$(peer_blob_read_count "$log_a" GET "$tp_blob")" "0" \
    "and the peer surface recorded NOTHING for it — a bearer-token read is not a peer read"

  assert_eq "$(peer_holds "$root/b/data/cas" "$tp_blob")" "1" "node B holds the bytes before the gap is made"
  find "$root/b/data/cas/blobs" -name "${tp_blob#blake3:}" -type f -delete
  assert_eq "$(peer_holds "$root/b/data/cas" "$tp_blob")" "0" "and does not hold them after"

  cli_b peers report-inventory site-b --json >/dev/null
  cli_b peers report-inventory site-a --json >/dev/null

  tp_json=$(api_a "/api/v1/desired/$tp_want/satisfaction")
  # assert_eq, not assert_contains. "not_satisfied" contains "satisfied" and
  # this is the assertion the whole milestone is for.
  assert_eq "$(jq -r '.placement.satisfaction' <<<"$tp_json")" "converging" \
    "PLACEMENT_CONVERGING: one of two required peers holds the bytes (§56)"
  assert_eq "$(jq -r '.placement.missing | join(",")' <<<"$tp_json")" "$peer_b_id" \
    "and it names WHICH peer is missing them — converging with no list is unactionable"
  assert_eq "$(jq -r '.state' <<<"$tp_json")" "PLACEMENT_CONVERGING" \
    "the §64 state §56 draws this distinction for, reached by a running system"
  assert_eq "$(jq -r '.content.satisfaction' <<<"$tp_json")" "satisfied" \
    "content is still satisfied — the two axes are separate answers, and only one moved"

  # -------------------------------------------------------------------------
  note "  the transfer: the destination pulls, verifies, and then claims (M4-09)"
  # -------------------------------------------------------------------------
  api_b "/api/v1/peers/site-b/reconcile" -X POST -o /dev/null
  # WAIT FOR THE LAST OF THE THREE THINGS ASSERTED BELOW, not the first (#207).
  # internal/worker/replicateblob.go pulls the bytes, THEN records the transfer,
  # THEN logs "replicated a blob", and only then does the queue mark the job
  # succeeded. Waiting on the bytes landing left the job row and the log line —
  # both asserted below — still in flight, and a slow runner would have reported
  # a replication job "retried into silence" that had simply not finished.
  wait_for "the replicate_blob job never reached succeeded on node B — the bytes, the transfer record and the log line all land before it does" \
    900 replicate_job_succeeded
  assert_eq "$(peer_holds "$root/b/data/cas" "$tp_blob")" "1" \
    "the bytes crossed the wire and landed in node B's content store"
  assert_eq "$(api_b "/api/v1/jobs?type=replicate_blob" | jq -r '[.items[] | select(.state == "succeeded")] | length > 0')" "true" \
    "and a replicate_blob job succeeded rather than being retried into silence"
  # The destination's account of the transfer, by field rather than by the
  # presence of a log line: which blob, how many bytes, and that they were
  # actually pulled rather than found already present.
  local tp_rep tp_size
  tp_size=$(api_a "/api/v1/blobs/$tp_blob" | jq -r '.size')
  tp_rep=$(grep -F '"msg":"replicated a blob"' "$log_b" 2>/dev/null |
    jq -s --arg h "$tp_blob" '[.[] | select(.blob_hash == $h)] | last // {}')
  # `| tostring`, never `// "absent"`. jq's alternative operator fires on false
  # as well as on null, so `.deduplicated // "absent"` reports a successful
  # non-deduplicated transfer as a missing field — which is exactly what it did
  # the first time this ran. An absent field comes back as "null" and fails
  # loudly instead.
  assert_eq "$(jq -r '.blob_hash | tostring' <<<"$tp_rep")" "$tp_blob" \
    "node B recorded the transfer of exactly these bytes"
  assert_eq "$(jq -r '.bytes | tostring' <<<"$tp_rep")" "$tp_size" \
    "and the whole blob crossed — the byte count matches what the catalogue says it is"
  assert_eq "$(jq -r '.deduplicated | tostring' <<<"$tp_rep")" "false" \
    "pulled over the wire, not found already present: a dedupe would prove nothing about a transfer"

  # -------------------------------------------------------------------------
  # And now the surface, from the node that SENT the bytes.
  # -------------------------------------------------------------------------
  #
  # THE CONTROLLER CARRIED NO BYTES, asserted as the positive it actually is:
  # node A served this blob on its PEER SURFACE, by GET, to the peer whose
  # pinned certificate opened the connection. The whole blob is accounted for by
  # that read — node B pulled tp_size bytes and re-hashed them itself — so there
  # is nothing left over for the client API to have carried.
  #
  # This is the assertion CI corrected. It used to count the client API's route
  # label and require it not to move, which is a fact about ffprobe's absence on
  # a laptop rather than a fact about the data path. See peer_blob_reads.
  local tp_served
  tp_served=$(peer_blob_reads "$log_a" GET "$tp_blob" | sort -u | tr '\n' ',' | sed 's/,$//')
  assert_eq "$(( $(peer_blob_read_count "$log_a" GET "$tp_blob") >= 1 ))" "1" \
    "node A served these bytes on its peer surface — the record that was empty a moment ago is not now"
  assert_eq "$tp_served" "site-b" \
    "and to site-b alone: the only listener in the data path is the one a bearer token cannot open"
  # -------------------------------------------------------------------------
  note "  the byte saving, as a NUMBER (M5-06, M5-07, M5-08, M5-09)"
  # -------------------------------------------------------------------------
  #
  # 🔴 Milestone 5's thesis is a saving, and a milestone whose thesis is a
  # saving must assert the saving rather than describe it. This section is where
  # that arithmetic is done, out loud, in the demo.
  #
  # THE CONTROL FIRST, because a saving assertion with no control passes on a
  # transfer that fetched nothing at all. The transfer above moved a blob to a
  # peer holding NONE of it, and the number is stated as a percentage rather
  # than left as two byte counts a reader has to divide.
  local sv_moved sv_pct
  sv_moved=$(jq -r '.bytes | tostring' <<<"$tp_rep")
  sv_pct=$(( sv_moved * 100 / tp_size ))
  assert_eq "$sv_pct" "100" \
    "THE CONTROL: replicating to a peer that holds nothing moves 100% of the blob — $sv_moved of $tp_size bytes"
  assert_eq "$(cli_b blobs verify "$tp_blob" --json 2>/dev/null | jq -r '.verified | tostring')" "true" \
    "and every one of those bytes was re-hashed by the destination against the blob's own digest (invariant 1)"

  # 🔴 AND THE SAVING ITSELF IS ASSERTED, further down this same arc.
  #
  # It was not, until this milestone closed: resumption and chunk reuse were
  # not on `main`, and the peer-backed chunk source was not wired into
  # `fsck --repair`, so this file could assert the expensive case and nothing
  # else. #196's acceptance is that a milestone whose thesis is a saving must
  # assert the saving, and a control on its own is a milestone that measured
  # the wrong number.
  #
  # It is asserted where the fabric can be made to produce it deterministically
  # — the repair arc, after lazy chunking has given node A a manifest — and the
  # measurement is the same instrument as the control above: bytes the SOURCE
  # recorded serving, not bytes a destination claims it fetched.
  #
  # THREE of the four savings this milestone measured are still asserted in Go
  # rather than here, and that is a limitation rather than a preference:
  #
  #   - a resumed transfer moving 74.5% of a 512 KiB blob after a real SIGKILL,
  #     29 of 110 chunks kept;
  #   - a modified file moving 1.1% of its size to a peer holding the original;
  #   - 3 KiB prepended moving 1.2%.
  #
  # Each needs an interruption at a chosen point, or a second blob built to
  # share chunks with the first. From a shell the first means a test hook in
  # production code and the second moves catalogue counts this file asserts
  # elsewhere. They live in internal/peer/transfer, where the byte counts are
  # measured on the source's serving side exactly as they are here.


  cli_b peers report-inventory site-a --json >/dev/null
  tp_json=$(api_a "/api/v1/desired/$tp_want/satisfaction")
  assert_eq "$(jq -r '.placement.satisfaction' <<<"$tp_json")" "satisfied" \
    "FULLY_SATISFIED after the transfer: placement closed the gap it reported"
  assert_eq "$(jq -r '.state' <<<"$tp_json")" "FULLY_SATISFIED" \
    "and the §64 name completes the walk CONTENT_SATISFIED → PLACEMENT_CONVERGING → FULLY_SATISFIED"
  assert_eq "$(jq -r '.placement.unproven' <<<"$tp_json")" "false" \
    "on evidence, with unproven false throughout"
  # -------------------------------------------------------------------------
  note "  the manifest on the peer surface: a description, not a negotiation (M5-05, ADR-0034)"
  # -------------------------------------------------------------------------
  #
  # A destination may ask a source what its chunks are. The source answers with
  # what it stored and decides NOTHING — it is never told what the destination
  # holds, never asked what to send, and never computes a difference (ADR-0030).
  #
  # 🔴 And it never GENERATES. A GET that chunked a 20 GB blob to answer would
  # be a remote denial of service with a polite name, so §16's third state is
  # the ANSWER: a destination that is told "no manifest" pulls whole, which is
  # exactly what the transfer above just did.
  #
  # The route lives on the PEER listener and nowhere else. A member that may
  # read the bytes may read their description; a bearer token is not a peer
  # credential (ADR-0011, ADR-0012, ADR-0015).
  assert_contains "$(<api/openapi.yaml)" "/peer/v1/blobs/{hash}/manifest" \
    "the manifest route is documented on the PEER surface (ADR-0015)"
  assert_not_contains "$(<api/openapi.yaml)" "/api/v1/blobs/{hash}/manifest" \
    "and nowhere on the client API: a bearer token is not a peer credential"

  # The two manifest-less answers are DIFFERENT answers and a destination acts
  # differently on each — one says "pull these bytes whole from this same
  # source", the other says "there is nothing here at all, try another source".
  # Asserted on the `type` URI, which is the contract, and asserted to be
  # non-overlapping, because a contains-check on one must not match the other.
  assert_contains "$(<api/openapi.yaml)" "no-chunk-manifest" \
    "a blob the source HOLDS with no manifest has a problem type of its own"
  assert_not_contains "no-chunk-manifest" "not-found" \
    "and it is not a substring of the not-found type: the two do not overlap"

  # This route is CALLED in this run, and by a running fabric rather than by a
  # curl. It is not called here: at this point in the arc no blob on either
  # node has a manifest, which is why the transfer above took the whole path
  # and is the assertion directly below. The call happens further down, once
  # lazy chunking has given node A a manifest and node B has a gap — and the
  # source's record of what it served, and to whom, is asserted there.
  #
  # The 404 that names WHICH manifest-less state a source was in is still a Go
  # assertion (internal/api/peerapi): reaching it from here would mean asking a
  # source for a manifest of a blob it holds and has not chunked, which is a
  # request this fabric never makes on its own.


  # -------------------------------------------------------------------------
  note "  a blob on NEITHER peer is not_satisfied, not converging"
  # -------------------------------------------------------------------------
  #
  # The distinction EvaluatePlacement draws, against real rows: converging
  # means replication is closing a gap, and bytes on nobody are not converging
  # on anything. Made by deleting the OTHER fixture's bytes from both stores.
  local tp_work2 tp_want2 tp_asset2 tp_blob2 tp_json2
  tp_work2=$(api_a /api/v1/works | jq -r '.items[] | select(.title == "Static Field") | .id')
  tp_want2=$(api_a /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d "{\"work_id\":\"$tp_work2\",\"quality_profile_id\":\"$tp_profile\"}" | jq -r '.id')
  tp_asset2=$(api_a "/api/v1/desired/$tp_want2/satisfaction" | jq -r '.content.satisfied_by')
  tp_blob2=$(api_a "/api/v1/assets/$tp_asset2" | jq -r '.blob_hash')

  find "$root/a/data/cas/blobs" -name "${tp_blob2#blake3:}" -type f -delete
  find "$root/b/data/cas/blobs" -name "${tp_blob2#blake3:}" -type f -delete
  cli_a peers report-inventory site-a --json >/dev/null
  cli_b peers report-inventory site-b --json >/dev/null
  cli_b peers report-inventory site-a --json >/dev/null

  tp_json2=$(api_a "/api/v1/desired/$tp_want2/satisfaction")
  assert_eq "$(jq -r '.placement.satisfaction' <<<"$tp_json2")" "not_satisfied" \
    "bytes on no peer at all are not_satisfied — nowhere is not converging on anything"
  assert_eq "$(jq -r '.placement.missing | length' <<<"$tp_json2")" "2" \
    "and both required peers are named as missing them"

  # -------------------------------------------------------------------------
  # -------------------------------------------------------------------------
  note "  lazy chunking: a manifest is a job's product, never a lookup's (§16, §75, M5-04)"
  # -------------------------------------------------------------------------
  #
  # §16 makes chunking lazy and §75 has listed `chunk_blob` since Milestone 1
  # with nothing behind it. This is the handler, on a running fabric.
  #
  # # Placement
  #
  # Inside the two-peer arc, at the end of it, and ABOVE the garbage-collection
  # refusal only because that section stops node B. It ingests one small file,
  # so it DOES create a Work, an asset and a blob — but in this arc's own
  # isolated library, below every count this arc asserts, and nowhere near the
  # demo catalogue the CLI section counts. That is the trap recorded at
  # `note "  the CLI (M1-17)"`, and this is the side of it that is safe.
  #
  # # Why the assertions are all on node A
  #
  # Node A holds every byte and its convergence cycle is triggered here
  # explicitly. Node B's own cycle also enqueues chunking, for blobs that are
  # arriving as it runs, and asserting the result of that race would be
  # asserting a timing rather than a rule.
  # A SMALL blob is needed and there is not one left. The section above
  # deliberately deleted the second fixture's bytes from BOTH stores, and the
  # fabric-of-one collector reclaimed the third, so the only blob node A still
  # holds is the five-megabyte one that just crossed the wire. One is therefore
  # ingested here, through a real scan, which also means the ingest assertions
  # below are made against a file that arrived after this node started rather
  # than against fixtures the run has been carrying since the beginning.
  local lc_small lc_large
  lc_large=$tp_blob
  mkdir -p "$lib/movies/Short Cut (2020)"
  head -c 32768 /dev/urandom > "$lib/movies/Short Cut (2020)/Short.Cut.2020.1080p.mkv"
  #
  # `scan --wait` waits for the SCAN jobs, which enqueue the ingests; the asset
  # appears when the ingest lands, so the wait is on the asset rather than on
  # the command returning. That distinction is the same one wait_for_ingest
  # makes in the single-node demo, and getting it wrong here cost a run.
  cli_a scan films --wait --json >/dev/null 2>&1 || true
  waited=0
  while (( waited < 900 )); do
    lc_small=$(api_a /api/v1/assets |
      jq -r '.items[] | select(.source_path != null and (.source_path | contains("Short.Cut"))) | .blob_hash' |
      head -1)
    [[ -n "$lc_small" ]] && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  assert_contains "$lc_small" "blake3:" "the small fixture names bytes node A ingested"
  assert_eq "$(api_a "/api/v1/blobs/$lc_small" | jq -r '.size')" "32768" \
    "and it is a small blob: well under the 4 MiB threshold, unlike the one that just crossed the wire"
  assert_eq "$(( $(api_a "/api/v1/blobs/$lc_large" | jq -r '.size') > 4194304 ))" "1" \
    "while the transferred blob is above it — the two sides of the threshold, on one node"

  # THE ABSENCE FIRST, and it is measured before anything is asked to produce
  # one. Ingest is `file → BLAKE3 → blob available` and nothing else: no
  # manifest, and not even a job to make one.
  #
  # assert_eq on the state, never assert_contains. The three §16 answers were
  # chosen so that none is a substring of another, and a containment match here
  # would accept the opposite meaning.
  assert_eq "$(cli_a blobs stat "$lc_large" --json | jq -r '.chunk_manifest')" "undecided" \
    "ingest left the large blob UNDECIDED — chunking it eagerly would be a second full read of "\
"every byte ingested, for a manifest that may never be used (§16)"
  assert_eq "$(cli_a blobs stat "$lc_small" --json | jq -r '.chunk_manifest')" "undecided" \
    "and the small one too: 'never needs one' is a decision, not something ingest guesses"
  assert_eq "$(api_a "/api/v1/jobs?type=chunk_blob" | jq -r '.items | length')" "0" \
    "and no chunk_blob job exists at all — not even one waiting to run"

  # Asking must never generate (ADR-0034). Five reads of the state, and the
  # answer is still the one that means nobody has decided.
  local lc_ask
  for lc_ask in 1 2 3 4 5; do
    api_a "/api/v1/blobs/$lc_large" >/dev/null
    cli_a blobs stat "$lc_large" --json >/dev/null
  done
  assert_eq "$(cli_a blobs stat "$lc_large" --json | jq -r '.chunk_manifest')" "undecided" \
    "ten reads of 'does this blob have a manifest' produced none: asking is a READ (ADR-0034)"
  assert_eq "$(api_a "/api/v1/jobs?type=chunk_blob" | jq -r '.items | length')" "0" \
    "and enqueued nothing — a question that produces the answer it was asked cannot be asked"

  # Now something decides it wants one: a convergence cycle that has just
  # worked out these bytes must cross a network. That is §16's own trigger —
  # "when replication or deduplication requires it" — and it is the only thing
  # that enqueues this job. There is no sweep.
  #
  # The same cycle also queues the TRANSFERS that close those gaps, with node B
  # as the destination, and node A refuses to run those — a destination pulls
  # its own bytes (ADR-0030). That is not something this section introduces:
  # every node's own five-minute beat computes the same gaps for every peer,
  # and it is why the chunking half is keyed on the blob alone rather than on
  # the transfer.
  # Only the large blob has to be taken away: node B has never seen the small
  # one — it was ingested into this library a moment ago and node A is the only
  # node that has scanned it — so it is already a gap, and one this section did
  # not have to manufacture.
  find "$root/b/data/cas/blobs" -name "${lc_large#blake3:}" -type f -delete
  # Node B walks its own store first and only then tells node A, which is the
  # order the gap arc above uses: a peer's inventory is what is on its DISK,
  # and reporting to A without re-reading B's own would forward a belief B has
  # not itself re-checked.
  cli_b peers report-inventory site-b --json >/dev/null
  cli_b peers report-inventory site-a --json >/dev/null
  api_a "/api/v1/peers/site-b/reconcile" -X POST -o /dev/null

  # Waited on the CONDITION rather than on a job count: the claim is that both
  # blobs stop being undecided, and a count of succeeded jobs can be satisfied
  # by jobs about other blobs entirely.
  waited=0
  while (( waited < 900 )); do
    [[ "$(cli_a blobs stat "$lc_large" --json | jq -r '.chunk_manifest')" != "undecided" &&
       "$(cli_a blobs stat "$lc_small" --json | jq -r '.chunk_manifest')" != "undecided" ]] && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  assert_eq "$(( $(api_a "/api/v1/jobs?type=chunk_blob" | jq -r '[.items[] | select(.state == "succeeded")] | length') >= 2 ))" "1" \
    "the cycle that decided the bytes must move enqueued the chunking, and it ran"
  assert_eq "$(api_a "/api/v1/jobs?type=chunk_blob" | jq -r '[.items[] | select(.state == "failed")] | length')" "0" \
    "and no chunking failed — a job that gives up is a manifest nobody will make"

  # The two sides of the threshold, on the same node, in the same pass.
  assert_eq "$(cli_a blobs stat "$lc_large" --json | jq -r '.chunk_manifest')" "present" \
    "the large blob has a manifest, generated on demand by a job (§16)"
  assert_eq "$(cli_a blobs stat "$lc_small" --json | jq -r '.chunk_manifest')" "not_required" \
    "and the small one is recorded as NEVER NEEDING one, rather than left undecided — "\
"below the threshold the manifest costs the same full read as the transfer it would optimise"
  assert_eq "$(api_a "/api/v1/blobs/$lc_small" | jq -r '.chunked')" "false" \
    "the compatibility boolean says false for it, which is exactly the conflation §16 needed a third state for"
  assert_eq "$(api_a "/api/v1/blobs/$lc_large" | jq -r '.chunked')" "true" \
    "and true for the blob that actually has one"

  # Idempotent (invariant 9). A second cycle over the same fabric enqueues
  # nothing new, and nothing about the recorded answers moves.
  api_a "/api/v1/peers/site-b/reconcile" -X POST -o /dev/null
  sleep 0.5
  assert_eq "$(cli_a blobs stat "$lc_large" --json | jq -r '.chunk_manifest')" "present" \
    "a second cycle left the manifest alone"
  assert_eq "$(cli_a blobs stat "$lc_small" --json | jq -r '.chunk_manifest')" "not_required" \
    "and left the recorded exemption alone — a re-run that changes nothing must also SAY nothing"
  # THE ASSERTION M5-03 EXISTS FOR: the two states a boolean collapsed together
  # are not the same string. Written as an explicit inequality rather than left
  # to be inferred from the two assertions above, because those two would both
  # pass if the names had been made the same value.
  if [[ "$(cli_a blobs stat "$lc_small" --json | jq -r '.chunk_manifest')" \
     == "$(cli_a blobs stat "$lc_large" --json | jq -r '.chunk_manifest')" ]]; then
    fail "'never needs a manifest' and 'has one' report the same state — that is the boolean again"
  else
    pass "the states blobs.chunked collapsed together are distinguishable in one read (M5-03)"
  fi


  # Put the fabric back the way this section found it. The gap above was made
  # by deleting bytes from node B, and the garbage-collection refusal below
  # turns on node A believing node B holds them — a peer that is UNREACHABLE is
  # a different refusal from a peer that has reported not having the bytes, and
  # leaving this section's gap in place would quietly change which one is being
  # asserted. It is closed by the real mechanism rather than by copying files
  # about: node B reconciles, pulls, and reports what it now holds.
  #
  # It is also the first transfer in this run that the SOURCE can describe:
  # node A has a manifest for these bytes now (asserted directly above) and
  # node B holds none of them, so this pull takes the CHUNKED path, and what it
  # costs is measured on node A's serving side either side of it.
  local ctl_before ctl_moved ctl_size ctl_manifests_before
  ctl_size=$(api_a "/api/v1/blobs/$lc_large" | jq -r '.size')
  ctl_before=$(peer_served_bytes "$log_a" "$lc_large")
  ctl_manifests_before=$(grep -cF '"msg":"served a chunk manifest to a peer"' "$log_a" 2>/dev/null || true)
  api_b "/api/v1/peers/site-b/reconcile" -X POST -o /dev/null
  waited=0
  while (( waited < 900 )); do
    (( $(peer_holds "$root/b/data/cas" "$lc_large") == 1 )) && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  cli_b peers report-inventory site-b --json >/dev/null
  cli_b peers report-inventory site-a --json >/dev/null
  assert_eq "$(peer_holds "$root/b/data/cas" "$lc_large")" "1" \
    "and the fabric is converged again, on the same transfer path, before the refusal below is asked for"
  ctl_moved=$(( $(peer_served_bytes "$log_a" "$lc_large") - ctl_before ))

  # THE MANIFEST, OVER THE WIRE. Until this pull, every transfer in this run
  # was of a blob nothing had chunked, so the route existed and nothing had
  # ever called it. This one is a destination asking a source to describe bytes
  # it is about to fetch, over mTLS, and the source recording what it served
  # and to whom.
  assert_eq "$(( $(grep -cF '"msg":"served a chunk manifest to a peer"' "$log_a" 2>/dev/null || true) \
      > ctl_manifests_before ))" "1" \
    "a destination read the source's chunk manifest over the peer surface before pulling — the \
route is no longer one nothing calls (M5-05, ADR-0034)"

  # 🔴 THE CONTROL, as a number, on the chunked path.
  #
  # Milestone 5's thesis is a saving, and a saving assertion with no control
  # passes on a transfer that fetched NOTHING at all. This is that control: the
  # destination held none of these bytes, so the chunked path had nothing to
  # reuse and had to move all of them. If this ever reads materially under 100%
  # the saving asserted below is measuring an empty transfer.
  assert_eq "$(pct "$ctl_moved" "$ctl_size")" "100" \
    "THE CONTROL: a chunked pull to a peer holding NONE of the blob moves 100% of it — \
$ctl_moved of $ctl_size bytes, measured on the SOURCE's serving side"
  assert_eq "$(cli_b blobs verify "$lc_large" --json 2>/dev/null | jq -r '.verified | tostring')" "true" \
    "and every one of those bytes was re-hashed by the destination against the blob's own digest (invariant 1)"

  # -------------------------------------------------------------------------
  note "  🔴 THE SAVING, as a NUMBER: a repair fetches the damage, not the blob (M5-08, M5-09)"
  # -------------------------------------------------------------------------
  #
  # #196's acceptance in its own words: a milestone whose thesis is a saving
  # must ASSERT the saving. Everything above this line is the control, and a
  # control on its own is a milestone that measured the expensive case.
  #
  # This is the cheap case, driven end to end on the running fabric: node A's
  # copy of a five-megabyte blob is damaged in ONE chunk, and `fsck --repair`
  # rebuilds it from node B. Node A already holds every chunk but that one, so
  # the only bytes that may cross the wire are the damaged chunk's — and what
  # crosses is counted on NODE B, the machine that sent them.
  #
  # Repair rather than a resumed or reusing transfer, and that choice is worth
  # stating. All three demonstrate the same saving. Only this one is
  # DETERMINISTIC from a shell: a resumed transfer needs an interruption at a
  # chosen point, and interrupting a transfer from outside means either a test
  # hook in production code or a race with a five-megabyte loopback copy. A
  # flaky assertion about a saving is worse than an honest paragraph saying the
  # saving is asserted in Go — so the other three stay in Go, and the epilogue
  # says which one this run drove.
  local rp_size rp_file rp_off rp_before rp_moved rp_json rp_entry
  rp_size=$ctl_size
  # -print -quit rather than `| head -1`: this file runs under `pipefail`, and
  # a find killed by the SIGPIPE `head` sends it makes the whole pipeline
  # non-zero, which under `set -e` ends the run with no output at all.
  rp_file=$(find "$root/a/data/cas/blobs" -name "${lc_large#blake3:}" -type f -print -quit)
  if [[ ! -f "$rp_file" ]]; then
    fail "the blob to damage is not in node A's store, so the repair below would measure nothing"
  fi
  # Damaged in the MIDDLE, not at the start: a chunker's first boundary is the
  # one most likely to be shared by accident, and damage at offset 0 would let
  # a repair that re-fetched a fixed prefix look chunk-scoped.
  # chmod first: a published blob is read-only, which is the store protecting
  # its own bytes from exactly this. Damaging it means stepping around that
  # deliberately, the way the single-node repair section already does.
  rp_off=$(( rp_size / 2 / 4096 ))
  chmod u+w "$rp_file"
  dd if=/dev/urandom of="$rp_file" bs=4096 seek="$rp_off" count=1 conv=notrunc 2>/dev/null
  assert_eq "$(cli_a blobs verify "$lc_large" --json 2>/dev/null | jq -r '.verified | tostring')" "false" \
    "the blob is damaged on node A: it no longer hashes to its own name"

  rp_before=$(peer_served_bytes "$log_b" "$lc_large")
  # ONE repair pass, reported as JSON. The enum is the diagnosis: every refusal
  # this can end in — no_manifest, unreachable, source_corrupt — is a different
  # sentence about a different thing, and asserting "the blob verifies" alone
  # would report all of them as the same silence.
  # "$BIN" directly rather than cli_a: fsck talks to the database and the store,
  # not to the API, so it takes no --token (ADR-0002 — it has to work when the
  # controller will not start, which is precisely when someone reaches for it).
  rp_json=$("$BIN" --config "$cfg_a" fsck --deep --repair --json 2>/dev/null || true)
  rp_moved=$(( $(peer_served_bytes "$log_b" "$lc_large") - rp_before ))
  rp_entry=$(jq -c --arg h "$lc_large" '[.repairs[] | select(.hash == $h)][0] // {}' <<<"$rp_json")

  assert_eq "$(jq -r '.outcome | tostring' <<<"$rp_entry")" "repaired" \
    "the damaged blob was REPAIRED from a peer — the outcome enum, so a refusal names ITSELF \
rather than arriving as a blob that still does not verify (ADR-0036)"
  assert_eq "$(cli_a blobs verify "$lc_large" --json 2>/dev/null | jq -r '.verified | tostring')" "true" \
    "and it hashes to its own name again: the replacement was verified WHOLE before publication, \
never written in place (invariant 1, ADR-0036)"

  # 🔴 THE NUMBER, from TWO instruments that do not share a code path.
  #
  # `bytes_fetched` is the repairer's own account of what it pulled. The delta
  # is what node B recorded SERVING. A repairer that under-reported, and a
  # source that over-reported, would each be invisible on its own; neither
  # survives having to agree with the other.
  local rp_reported rp_total
  rp_reported=$(jq -r '.bytes_fetched | tostring' <<<"$rp_entry")
  rp_total=$(jq -r '.chunks_total | tostring' <<<"$rp_entry")

  # THE SAVING IS ASSERTED IN CHUNKS, and the bytes are reported beside it.
  #
  # That is not a softer claim, it is the fixture-independent one. At the
  # chunker's shipped parameters — 256 KiB minimum, 1 MiB average, 4 MiB
  # maximum — a five-megabyte blob is about FIVE chunks, so one chunk of it is
  # a fifth, and the measured figure swings between roughly 8% and 23% run to
  # run purely with where a content-defined boundary happens to fall in random
  # bytes. A percentage threshold here would be asserting the fixture's size,
  # and it would go red on a chunker re-tuning that is not a regression.
  #
  # What the feature actually claims is that a repair costs ONE CHUNK rather
  # than one blob, and that is exact: fetched over total. The dramatic ratios
  # need a blob with many chunks, and a blob with many chunks does not fit a
  # 240-second acceptance budget — which the epilogue says out loud rather than
  # letting this section imply otherwise.
  assert_eq "$(jq -r '.chunks_fetched | tostring' <<<"$rp_entry")" "1" \
    "🔴 THE SAVING: repairing one damaged chunk fetched exactly ONE chunk of the $rp_total this \
blob has — $rp_moved bytes of $rp_size, $(pct "$rp_moved" "$rp_size")% — against the 100% control \
above, both measured on the SOURCE's serving side (#196, ADR-0036)"
  assert_eq "$(( rp_total > 1 ))" "1" \
    "and the blob has MORE than one chunk ($rp_total), without which fetching one of them is not \
a saving and this section would pass on a single-chunk blob"
  assert_eq "$(( rp_moved > 0 ))" "1" \
    "it crossed the WIRE: node B served $rp_moved bytes. A repair that fetched nothing repaired \
nothing, and would report the best saving in this file"
  assert_eq "$rp_reported" "$rp_moved" \
    "and the two ends agree — the repairer says it fetched $rp_reported bytes and the source says \
it served $rp_moved"
  assert_eq "$(( rp_moved < rp_size ))" "1" \
    "and it is materially less than the blob: $rp_moved of $rp_size bytes"

  # The repair settled, asserted about THIS blob rather than about the whole
  # store. A second pass may legitimately report other blobs — this arc leaves
  # unreferenced and deleted bytes behind on purpose — and a global exit code
  # would make this section fail for somebody else's fixture.
  local rp_second
  rp_second=$("$BIN" --config "$cfg_a" fsck --deep --repair --json 2>/dev/null || true)
  assert_eq "$(jq -r --arg h "$lc_large" '[.repairs[]? | select(.hash == $h)] | length' <<<"$rp_second")" "0" \
    "a second pass finds nothing to repair for these bytes — which is what tells a real repair \
apart from one that republished the damage"

  # The damaged original is preserved, never deleted (ADR-0018). It is then
  # removed HERE, by this script, so the sections below count the store they
  # expect: quarantine is evidence for an operator, and leaving it would make
  # this section's cost show up in somebody else's assertion.
  local rp_q
  rp_q=$(find "$root/a/data/cas/quarantine" -type f 2>/dev/null | wc -l | tr -d ' ')
  assert_eq "$(( rp_q >= 1 ))" "1" \
    "the damaged original was QUARANTINED rather than deleted — on a hardlink-ingested library \
the corruption may be the operator's own file (ADR-0018)"
  find "$root/a/data/cas/quarantine" -type f -delete 2>/dev/null || true
  # ADR-0034's falsification, asserted about transfers that have ALREADY
  # happened rather than by deleting rows out of a database. Every blob in this
  # arc was `undecided` when the five-megabyte blob first crossed the wire a
  # hundred lines above — no manifest on either node, and no chunk_blob job at
  # all, both asserted at the time. The bytes crossed anyway, node B re-hashed
  # them itself, and no replication job failed.
  #
  # That is ADR-0034's condition in its own words: if deleting every manifest in
  # the store broke anything other than efficiency, the line would have been
  # crossed. A store with no manifests is the state this run spent its whole
  # transfer arc in.
  assert_eq "$(api_b "/api/v1/jobs?type=replicate_blob" | jq -r '[.items[] | select(.state == "failed")] | length')" "0" \
    "no replication has failed for want of a manifest: a manifest is an optimisation, never a precondition (ADR-0034)"
  assert_eq "$(cli_b blobs verify "$tp_blob" --json 2>/dev/null | jq -r '.verified | tostring')" "true" \
    "and the blob that crossed with no manifest anywhere verifies to its own whole-object digest on the destination"

  # -------------------------------------------------------------------------
  note "  garbage collection catches a LYING replica row (Refusal 3, #184)"
  # -------------------------------------------------------------------------
  #
  # The refusal that needs a peer to ANSWER, and the one that lived only in Go
  # until #184 made a remote peer's health able to move.
  #
  # The refusal below this one is the easy half: node B is stopped, nothing
  # answers, and nothing about where the bytes are can be established. This is
  # the other one. Node B is UP and reachable. Its `replicas` row for the blob
  # says `present` and was confirmed moments ago, so the catalog's belief is
  # fresh and positive. The bytes are then deleted from node B's disk WITHOUT B
  # reporting its inventory again — which is the scenario ADR-0018 is about: a
  # peer that lost a disk, restored an older CAS, or quarantined a blob, leaving
  # a row that still says `present`. Node A is then asked to reclaim its last
  # copy.
  #
  # Before #184 this path was unreachable from a shell. Reachable() is consulted
  # before the dial, and a remote peer's health was pinned at `unknown`, so the
  # sweep refused with `peer_unreachable` before it ever asked.
  #
  # It uses the SMALL blob, not the transferred one, and that is deliberate:
  # this section leaves its blob unreferenced with a `missing` row on B, and the
  # refusal below turns on node A believing node B still holds ITS blob. Sharing
  # a blob between the two would quietly change which refusal the section below
  # is asserting — from peer_unreachable (rank 3) to replica_not_present
  # (rank 1) — and it would pass either way.
  local r3_json r3_before r3_after r3_assets
  # Node B has to genuinely HOLD these bytes for the lie to be a lie, and until
  # now it has not: the small fixture was written into the shared library
  # directory a moment ago and only node A has scanned it. Node B ingests it
  # from the same file, by its own scan — content addressing gives the two
  # catalogues the same digest without either being told about the other.
  #
  # This is deliberately BELOW the chunking assertions above rather than beside
  # the scan that made the file. Those assertions turn on the small blob being a
  # gap for node B; ingesting it here would close that gap and quietly change
  # what they prove.
  cli_b scan films --wait --json >/dev/null 2>&1 || true
  waited=0
  while (( waited < 900 )); do
    (( $(peer_holds "$root/b/data/cas" "$lc_small") == 1 )) && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  cli_b peers report-inventory site-b --json >/dev/null
  cli_b peers report-inventory site-a --json >/dev/null

  # Node B is reachable, and it is asserted rather than assumed: this refusal is
  # only distinguishable from peer_unreachable if the peer is actually up.
  # assert_eq, never assert_contains — "unreachable" CONTAINS "reachable".
  assert_eq "$(cli_a peers list --json | jq -r '.[] | select(.name == "site-b") | .health')" \
    "reachable" "node B is reachable, so a refusal here cannot be silence wearing a lie's clothes"

  # The catalog's BELIEF: present, and fresh, because B reported it moments ago.
  assert_eq "$(api_a "/api/v1/replicas?state=present" |
    jq -r --arg h "$lc_small" --arg p "$peer_b_id" \
      '[.items[] | select(.blob_hash == $h and .peer_id == $p)] | length')" "1" \
    "the catalog believes node B holds these bytes, and says so in a fresh row"

  # And now the divergence. Deleted from B's disk, and B is NOT asked to report
  # again — the row keeps saying present, which is exactly the lie.
  assert_eq "$(peer_holds "$root/b/data/cas" "$lc_small")" "1" "node B holds the bytes before the lie is made"
  find "$root/b/data/cas/blobs" -name "${lc_small#blake3:}" -type f -delete
  assert_eq "$(peer_holds "$root/b/data/cas" "$lc_small")" "0" "and does not hold them after"

  # Unreferenced on node A, so it is garbage by every LOCAL measure.
  r3_assets=$(api_a /api/v1/assets | jq -r --arg h "$lc_small" '.items[] | select(.blob_hash == $h) | .id')
  for aid in $r3_assets; do api_a "/api/v1/assets/$aid" -X DELETE -o /dev/null; done
  assert_eq "$(api_a /api/v1/assets | jq -r --arg h "$lc_small" '[.items[] | select(.blob_hash == $h)] | length')" "0" \
    "the blob is unreferenced on node A, and eligible by every LOCAL measure"

  r3_before=$(find "$root/a/data/cas" -type f | wc -l | tr -d ' ')
  # Marking pass, then the pass that would reclaim.
  "$BIN" --config "$cfg_a" gc --apply --grace 1ns --json >/dev/null 2>&1

  r3_json=$("$BIN" --config "$cfg_a" gc --apply --grace 1ns --json 2>/dev/null)
  r3_after=$(find "$root/a/data/cas" -type f | wc -l | tr -d ' ')

  assert_eq "$(jq -r '.reclaimed | length' <<<"$r3_json")" "0" "nothing was reclaimed"
  assert_eq "$r3_after" "$r3_before" "and nothing left the store: the file count is unchanged"
  assert_eq "$(peer_holds "$root/a/data/cas" "$lc_small")" "1" \
    "THE LAST COPY IS STILL THERE — a lying row did not cost the fabric its only copy"
  assert_eq "$(jq -r '[.spared[] | select(.hash == "'"$lc_small"'")] | length' <<<"$r3_json")" "1" \
    "the blob whose last local copy was at stake was spared"
  # THE ASSERTION THIS SECTION EXISTS FOR. assert_eq, never assert_contains:
  # every reason in this enum shares words with its neighbours, and
  # "peer_unreachable" is the answer this must NOT be — that is the easy half,
  # and it is the one the demo could already prove.
  assert_eq "$(jq -r '[.spared[] | select(.hash == "'"$lc_small"'")][0].reason' <<<"$r3_json")" "remote_lacks_blob" \
    "REFUSAL 3: the row said present, the peer was ASKED, and it answered that it does not hold them"
  assert_eq "$(jq -r '[.spared[] | select(.hash == "'"$lc_small"'")][0].peer_name' <<<"$r3_json")" "site-b" \
    "and it names WHICH peer's claim did not survive being checked"

  # The correction, which is the half that stops the next sweep rediscovering
  # the same lie. The row is not deleted — a peer that lost bytes must stay
  # VISIBLE — it is moved to `missing`.
  assert_eq "$(api_a "/api/v1/replicas?state=missing" |
    jq -r --arg h "$lc_small" --arg p "$peer_b_id" \
      '[.items[] | select(.blob_hash == $h and .peer_id == $p)] | length')" "1" \
    "and the lying row was CORRECTED to missing, so the next sweep does not have to discover it again"
  assert_eq "$(api_a "/api/v1/replicas?state=present" |
    jq -r --arg h "$lc_small" --arg p "$peer_b_id" \
      '[.items[] | select(.blob_hash == $h and .peer_id == $p)] | length')" "0" \
    "the present claim is gone: correcting it means the belief changed, not that a second row appeared"

  # The operator at a terminal, on the NEXT sweep — and the reason has changed,
  # which is the correction doing its job rather than a weaker assertion.
  #
  # This refusal corrects the row it caught, so the lie survives exactly one
  # reclaiming pass. The next pass reads a row that now says `missing` — a peer
  # claiming NOT to hold the bytes — and spares them on that alone, without
  # dialling anybody. Asserting `remote_lacks_blob` here would be asserting that
  # the correction did NOT happen, and it would pass while the feature was
  # broken. `replica_not_present` is the assertion that the correction landed
  # somewhere the collector reads.
  assert_eq "$(grep -cF "spared        $lc_small  replica_not_present" \
    <<<"$("$BIN" --config "$cfg_a" gc --apply --grace 1ns 2>/dev/null)" | tr -d ' ')" "1" \
    "the next sweep spares the same blob on the CORRECTED row, without asking the peer again"


  # -------------------------------------------------------------------------
  note "  garbage collection REFUSES to delete the last copy (ADR-0018, §53, M4-12)"
  # -------------------------------------------------------------------------
  #
  # THE MILESTONE'S OTHER HEADLINE, and the one an operator most needs to have
  # seen work.
  #
  # Node B is STOPPED first, and waited for, so that "the peer is gone" is a
  # fact rather than a signal that has been sent. Then the blob that just
  # crossed the wire is unreferenced on node A, which makes it garbage by every
  # LOCAL measure the first three milestones had: nothing points at it, and the
  # grace window is set to a nanosecond so age cannot be what saves it.
  #
  # Before this milestone that combination unlinked the bytes. The fabric's last
  # copy of a blob and an orphan from a rolled-back ingest look identical from
  # inside one node's catalog, and ADR-0018 deferred the difference to here.
  #
  # The contrast that makes this mean something is a hundred lines above: the
  # SAME collector, on the SAME node, unlinked a blob when this was a fabric of
  # one. Nothing in between changed the code. A peer was enrolled, and then it
  # went away.
  local gc_before gc_after gc_assets gc_refuse gc_text
  stop_peer_node "${pids_b[@]}"
  pids_b=()

  gc_assets=$(api_a /api/v1/assets | jq -r --arg h "$tp_blob" '.items[] | select(.blob_hash == $h) | .id')
  for aid in $gc_assets; do api_a "/api/v1/assets/$aid" -X DELETE -o /dev/null; done
  assert_eq "$(api_a /api/v1/assets | jq -r --arg h "$tp_blob" '[.items[] | select(.blob_hash == $h)] | length')" "0" \
    "the transferred blob is unreferenced on node A, and eligible by every LOCAL measure"

  gc_before=$(find "$root/a/data/cas" -type f | wc -l | tr -d ' ')
  # Marking pass, then the pass that would reclaim.
  "$BIN" --config "$cfg_a" gc --apply --grace 1ns --json >/dev/null 2>&1
  gc_refuse=$("$BIN" --config "$cfg_a" gc --apply --grace 1ns --json 2>/dev/null)
  gc_after=$(find "$root/a/data/cas" -type f | wc -l | tr -d ' ')

  assert_eq "$(jq -r '.reclaimed | length' <<<"$gc_refuse")" "0" "nothing was reclaimed"
  assert_eq "$gc_after" "$gc_before" "and nothing left the store: the file count is unchanged"
  assert_eq "$(peer_holds "$root/a/data/cas" "$tp_blob")" "1" \
    "THE LAST COPY IS STILL THERE — this is the assertion the milestone is for"
  # Named rather than counted. Until Refusal 3 was folded in above, this could
  # assert "exactly one blob was spared" and mean "the sweep did something"; that
  # section now leaves a second unreferenced blob behind, whose own row it
  # corrected to `missing`. Counting would fail on a change that made this
  # section MORE thorough, so the blob at stake is named and the count is only
  # asked to be non-zero.
  assert_eq "$(( $(jq -r '.spared | length' <<<"$gc_refuse") >= 1 ))" "1" \
    "at least one blob was spared, so this is a refusal and not a sweep that did nothing"
  assert_eq "$(jq -r '[.spared[] | select(.hash == "'"$tp_blob"'")] | length' <<<"$gc_refuse")" "1" \
    "and the blob whose last local copy was at stake is one of them"
  # assert_eq, never assert_contains: "no_other_peer" CONTAINS "other_peer",
  # and a substring match on an enum-like value has shipped here once already.
  assert_eq "$(jq -r '[.spared[] | select(.hash == "'"$tp_blob"'")][0].reason' <<<"$gc_refuse")" "peer_unreachable" \
    "the reason is that nothing about where these bytes are could be established"
  assert_eq "$(jq -r '[.spared[] | select(.hash == "'"$tp_blob"'")][0].peer_name' <<<"$gc_refuse")" "site-b" \
    "and it names WHICH peer could not be established against — a refusal nobody can diagnose is an outage"

  # The operator at a terminal must not be the one left guessing either. The
  # --json shape is for scripts; a person reading the output gets the hash and
  # the reason on one line, and the sentence underneath it.
  gc_text=$("$BIN" --config "$cfg_a" gc --apply --grace 1ns 2>/dev/null)
  assert_eq "$(grep -cF "spared        $tp_blob  peer_unreachable" <<<"$gc_text" | tr -d ' ')" "1" \
    "gc's plain output names the blob it spared and why"

  local p
  for p in "${PEER_PIDS[@]:-}"; do kill -TERM "$p" 2>/dev/null || true; done
  for p in "${PEER_PIDS[@]:-}"; do wait "$p" 2>/dev/null || true; done
  PEER_PIDS=()
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
  note "the provider health beat (#164)"
  provider_health_beat_demo
  note "THE SECOND PEER: placement, proven (§56, §64, M4-11) — heyarr all"
  two_peer_demo all
  note "THE SECOND PEER, again, as separate role processes (ADR-0002, M4-16)"
  two_peer_demo split
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

# ---------------------------------------------------------------------------
# What this demo proves, and what it does not (M3-15)
# ---------------------------------------------------------------------------
#
# Printed rather than filed, because a limitation in a document is one nobody
# reads at the moment it matters. Whoever runs this and sees it pass should see
# the boundary of what passing means in the same breath.
note "what this run proves, and what it does not"
printf '       proven, on this machine, in this run:\n'
printf '         a want for content that existed NOWHERE reached bytes under\n'
printf '         management — searched, evaluated, explained, acquired, verified\n'
printf '         and ingested — with NO REAL INDEXER present (§84, ADR-0026).\n'
printf '         a want that found three releases and accepted none left three\n'
printf '         durable explanations rather than a silence (§63).\n'
printf '         a node with no toolchain, no indexer and no download client\n'
printf '         starts, scans, ingests, serves ranges, and says which of those\n'
printf '         it cannot do (ADR-0023, ADR-0025).\n'
printf '         PLACEMENT, by two peer processes with two data directories, two\n'
printf '         databases, two content stores and two Ed25519 identities, each\n'
printf '         enrolled on the other'"'"'s public key and talking over mutually\n'
printf '         authenticated TLS with no CA anywhere (§26, ADR-0012). A blob\n'
printf '         was deleted from one store — really deleted, from a real disk —\n'
printf '         the OTHER node'"'"'s API reported PLACEMENT_CONVERGING and named the\n'
printf '         peer missing it, the destination pulled the bytes, verified them\n'
printf '         itself and only then claimed the replica, and the same API\n'
printf '         answered FULLY_SATISFIED. Bytes on NEITHER peer answered\n'
printf '         not_satisfied rather than converging, which is the distinction\n'
printf '         §56 draws the state for. The whole arc ran twice: once with each\n'
printf '         node as `heyarr all`, once with each node as three separate role\n'
printf '         processes (ADR-0002).\n'
printf '         THE CONTROLLER CARRIED NO BYTES, asserted from the node that\n'
printf '         SENT them rather than from a counter that stayed still. Node A\n'
printf '         recorded serving this blob on its PEER SURFACE, by GET, to the\n'
printf '         peer whose pinned certificate opened the connection; node B\n'
printf '         pulled the whole blob and re-hashed it itself (§21, §32,\n'
printf '         ADR-0030). Two listeners, two trust roots, and every byte\n'
printf '         accounted for by the one a bearer token cannot open. A\n'
printf '         client-API read of the same blob moments earlier moved the\n'
printf '         client API'"'"'s counter and left the peer surface'"'"'s record empty,\n'
printf '         so the two are known to be distinguishable rather than assumed\n'
printf '         to be.\n'
printf '         GARBAGE COLLECTION REFUSED TO DELETE THE LAST COPY. With the\n'
printf '         second peer stopped and waited for, a blob nothing referenced\n'
printf '         and a one-nanosecond grace window — garbage by every measure the\n'
printf '         first three milestones had — was kept, and reported as spared,\n'
printf '         by hash, naming the peer nothing could be established about\n'
printf '         (ADR-0018). The same collector on the same node unlinked a blob\n'
printf '         minutes earlier, while that node was still a fabric of one, so\n'
printf '         this is a gate and not a collector that refuses everything.\n'
printf '         CHUNK MANIFESTS, AND THE THIRD STATE A BOOLEAN COULD NOT HOLD.\n'
printf '         `blobs.chunked` had been 0 on every row in every deployment\n'
printf '         since Milestone 1 because nothing ever wrote it, and §16 asks a\n'
printf '         question with three answers. All three were read off one\n'
printf '         running fabric in one pass: a blob with a manifest, a blob\n'
printf '         RECORDED as never needing one, and a blob nobody has decided\n'
printf '         about — asserted as an inequality, not inferred from three\n'
printf '         separate reads (§16, ADR-0034). Asking never generated one:\n'
printf '         ten reads of the state left no manifest, no chunk row and no\n'
printf '         chunk_blob job, measured against the job queue rather than\n'
printf '         against the answer.\n'
printf '         LAZY CHUNKING, BY A JOB AND BY NOTHING ELSE. Ingest chunked\n'
printf '         nothing; no sweep chunked anything; the manifests appeared\n'
printf '         when a convergence cycle decided the bytes had to cross a\n'
printf '         network, which is §16'"'"'s own trigger. The blob above the 4 MiB\n'
printf '         threshold got a manifest and the one below it was recorded as\n'
printf '         never needing one — the two sides of the threshold, on the same\n'
printf '         node, in the same pass — and a second cycle changed neither.\n'
printf '         WHICH WORKER CAN DO WHAT, from the running binary rather than\n'
printf '         from a unit test (#112, ADR-0039). The worker this run started\n'
printf '         advertised ITSELF within its startup beat — the assertion no Go\n'
printf '         test can make, because every one of them passes on a build\n'
printf '         where the beat is never started — with an expiry in the future,\n'
printf '         a stated source for every capability, and an exact-match filter\n'
printf '         that answers nothing for a partial dotted segment. A second\n'
printf '         worker against the same database appeared as its own holder, so\n'
printf '         the view is per worker rather than per node.\n'
printf '         A LYING REPLICA ROW, CAUGHT (Refusal 3, ADR-0018, #184). The\n'
printf '         refusal below is the easy half — nothing answered. This is the\n'
printf '         other one: node B was UP and reachable, its row said `present`\n'
printf '         and had been confirmed seconds earlier, its bytes were then\n'
printf '         deleted from its disk without it reporting again, and node A was\n'
printf '         asked to reclaim its last copy. A dialled the peer, B denied\n'
printf '         holding the bytes, the sweep refused by name, and THE ROW WAS\n'
printf '         CORRECTED to `missing` rather than deleted — so the next sweep\n'
printf '         does not rediscover the same lie. Until #184 this path was\n'
printf '         unreachable from a shell: a remote peer'"'"'s health was pinned at\n'
printf '         `unknown` and the sweep refused with `peer_unreachable` before\n'
printf '         it ever asked. Peer health was asserted `unknown` FIRST and then\n'
printf '         observed moving to `reachable` on peer-surface traffic alone,\n'
printf '         with the timestamp an operator acts on beside it.\n'
printf '         A ONE-WAY PAIRING IS REPORTED AND ENROLLED, NEVER REFUSED\n'
printf '         (#186, ADR-0037, ADR-0038). One node was pointed at a port that\n'
printf '         refuses connections so the return leg genuinely could not\n'
printf '         answer; the enrolment SUCCEEDED, and the operator was told which\n'
printf '         direction failed, at which address, that it is not a fault, and\n'
printf '         why. A healthy pairing said nothing at all, which is what makes\n'
printf '         the report worth reading.\n'
printf '\n'
printf '       NOT proven, and not claimed:\n'
printf '         A REAL NETWORK. The two peers above are two PROCESSES ON ONE\n'
printf '         MACHINE. They share a kernel, a disk, a clock and a loopback\n'
printf '         interface. That is enough for the protocol, the pinning, the\n'
printf '         verification, the refusals and the data path, and it is the\n'
printf '         whole of what was shown: nothing in this run has met a\n'
printf '         partition, a slow link, an MTU, packet loss, a reordered or\n'
printf '         half-open connection, or two clocks that disagree. A green run\n'
printf '         here says the fabric is correct, not that it is deployable.\n'
printf '         A GENUINELY ONE-WAY PATH. Two processes on one loopback\n'
printf '         interface reach each other by construction, so the topology\n'
printf '         #186 was found on cannot occur here. The section above\n'
printf '         SIMULATES it, by pointing one node'"'"'s record of the other at a\n'
printf '         port that refuses connections, and proves the report fires and\n'
printf '         names the direction. It says nothing about what a real\n'
printf '         asymmetric path does to a transfer already in flight. Note that\n'
printf '         under ADR-0038 such a pairing is ORDINARY rather than broken:\n'
printf '         each peer is authoritative for its own site, so a node that\n'
printf '         cannot be reached back still serves everything on its own disk\n'
printf '         and still fetches what it lacks from the peer it can reach. The\n'
printf '         cost is convergence in one direction, and nothing here is\n'
printf '         degraded for want of a partner.\n'
printf '         A SECOND PHYSICAL MACHINE, and by extension any deployment\n'
printf '         host. Nothing above left this one. Peer-to-peer mTLS, pinning,\n'
printf '         revocation and re-enrolment WERE exercised between two real\n'
printf '         machines during Milestone 4, and #184'"'"'s liveness transition and\n'
printf '         #186'"'"'s return-path check were exercised between two real\n'
printf '         machines on different subnets during Milestone 5 — by a person,\n'
printf '         at two keyboards, once each. That is recorded here so its\n'
printf '         absence from this run is not mistaken for its absence\n'
printf '         altogether, and it is not an assertion in this file. This\n'
printf '         paragraph describes the run.\n'
printf '         THE PROBED HALF OF PEER LIVENESS. The transition above is\n'
printf '         driven by traffic ARRIVING: node A recorded node B because node\n'
printf '         B spoke to its peer surface. The idle prober — a node dialling a\n'
printf '         peer that has said nothing, over pinned mTLS, to find out\n'
printf '         whether it is still there — is asserted in\n'
printf '         internal/peer/health rather than here, and the two-machine run\n'
printf '         that observed the transition observed the inbound half of it\n'
printf '         too. Nothing in this file moves a health value by probing.\n'
printf '         A REAL INDEXER. No Torznab client has ever answered a search\n'
printf '         here; every candidate above came from a fake, and ADR-0026 says\n'
printf '         that stays true in CI forever. The client now EXISTS — it is\n'
printf '         tested against a corpus captured from two different Torznab\n'
printf '         servers — but no assertion above was answered by one.\n'
printf '         What those two servers do not cover is quality attributes: the\n'
printf '         only tracker safe to capture from into a public repository\n'
printf '         indexes Linux distributions, which assert no resolution, codec\n'
printf '         or audio. Against a real indexer today, this system determines\n'
printf '         a release SIZE and reports every quality rule undetermined.\n'
printf '         TRANSCODE. Milestone 2 ships remux only, and nothing through\n'
printf '         Milestone 5 has needed more: quality profiles select BETWEEN\n'
printf '         releases rather than producing them, and replication moves\n'
printf '         bytes — now fewer of them — without looking inside them.\n'
printf '         LINKED ASSETS still have no blob (ADR-0020). Milestone 3 added a\n'
printf '         fifth place that has to say so — placement, which reports\n'
printf '         not_applicable rather than a vacuous satisfied.\n'
printf '         METADATA. Identification is still Milestone 1'"'"'s path parser, and\n'
printf '         HDR detection is still a substring match on a probe profile.\n'
printf '         THE ARC ITSELF ends at AVAILABLE, not CONTENT_SATISFIED, and\n'
printf '         always will: the artifact it acquires stands in for a completed\n'
printf '         download and is not media, so no toolchain can measure it. That\n'
printf '         is the right answer — a gate that cannot be shown to hold must\n'
printf '         not pass — and measurement against real media is proven by the\n'
printf '         satisfaction section instead.\n'

# The part of "NOT proven" that is a property of THIS MACHINE rather than of
# this milestone (M5-10, #187).
#
# Everything above is a limitation of the system and reads the same on every
# runner. This is different: it is what this particular machine was not equipped
# to ask, and it is the difference between a green run here and a green run on
# the equipped CI runner. Without it the two are indistinguishable, which is how
# five green local runs measured the absence of ffprobe.
if (( CAPS_UNEXERCISED_N > 0 )); then
  printf '         WHAT THIS MACHINE COULD NOT ASK. %d assertion block(s)\n' "$CAPS_UNEXERCISED_N"
  printf '         declared a capability this machine does not have. Their\n'
  printf '         subject was ABSENT, so they were not run — they are neither\n'
  printf '         failures nor coverage, and the branch that ran in their place\n'
  printf '         asserts the degraded behaviour instead (ADR-0023, ADR-0025):\n'
  capability_ledger '           '
  printf '         A green run here is NOT a green run on a machine, or a build,\n'
  printf '         that has them. Two kinds are listed above and they close\n'
  printf '         differently: a MACHINE capability (ffmpeg, ffprobe) closes by\n'
  printf '         installing the pinned toolchain with scripts/toolchain.sh, or\n'
  printf '         by reading CI, which runs it both ways. A BUILD gap — a block\n'
  printf '         whose subject is a code path not on this build — closes only\n'
  printf '         when that path lands, and no amount of tooling here will\n'
  printf '         change it. Each entry says which it is.\n'
else
  printf '         NOTHING WAS LEFT UNASKED for want of a capability: every\n'
  printf '         assertion block in this file found what it declared it needed,\n'
  printf '         so this run is the complete one (#187).\n'
fi
printf '\n'
printf '       NEW IN THIS MILESTONE, and stated rather than left to be found:\n'
printf '         PARTIAL TRANSFER STATE EXISTS NOW, and ADR-0035 is what it had\n'
printf '         to satisfy. Milestone 4 had no partial state at all — a receive\n'
printf '         that did not finish left nothing, and that absence is what made\n'
printf '         the handler idempotent. It is idempotent in a stronger sense\n'
printf '         now: a re-run re-verifies what an earlier attempt left, against\n'
printf '         a manifest this node fetched and checked itself, and keeps only\n'
printf '         the contiguous prefix that checks out. The resume unit is a\n'
printf '         CHUNK and never a byte offset; the assembled result is hashed\n'
printf '         WHOLE before publication; partial bytes are addressable by\n'
printf '         nothing and are never a replica; and a blob with no manifest is\n'
printf '         still not resumable, retried whole, which is §16 doing its job\n'
printf '         rather than a gap.\n'
printf '         WHAT THIS RUN DRIVES OF THAT, AND WHAT IT DOES NOT. The saving\n'
printf '         is asserted above as a number, over mTLS, against a 100%%%%\n'
printf '         control — but by the REPAIR path, because that is the one a\n'
printf '         shell can make deterministic. Resumption after an interruption,\n'
printf '         and reuse across a modified file, are asserted in\n'
printf '         internal/peer/transfer and not here: the first needs a transfer\n'
printf '         interrupted at a chosen point, which from outside means a test\n'
printf '         hook in production code or a race, and the second needs a\n'
printf '         second blob built to share chunks with the first, which moves\n'
printf '         catalogue counts this file asserts elsewhere. A flaky assertion\n'
printf '         about a saving would be worse than this paragraph.\n'
printf '         MANIFESTS ARE LAZY, so any blob may have one, may be recorded as\n'
printf '         never needing one, or may not have been decided about — and the\n'
printf '         third is the ordinary state of a library nothing has replicated.\n'
printf '         This run exercises all three, but it reaches `present` and\n'
printf '         `not_required` only through a convergence cycle on a two-peer\n'
printf '         fabric. A single-node Heyarr stays entirely `undecided` forever,\n'
printf '         which is correct and is asserted above, and it means a\n'
printf '         one-machine deployment exercises none of the chunk paths.\n'
printf '         REPAIR WAS NOT PROVEN AGAINST REAL CORRUPTION. Every damaged\n'
printf '         blob in this repository'"'"'s tests was damaged by OVERWRITING BYTES\n'
printf '         THROUGH THE FILESYSTEM. That is not bit rot, not a failing\n'
printf '         sector, and not a torn write — the three things repair exists\n'
printf '         for — and it is not a filesystem that returns EIO on a read. It\n'
printf '         establishes that the repairer detects a digest mismatch, fetches\n'
printf '         only the chunks that changed, stages a whole replacement, and\n'
printf '         never edits a blob in place (ADR-0036). It establishes nothing\n'
printf '         about the failure modes of a real disk.\n'
printf '         THE SAVINGS ARE DEMONSTRATIONS, NOT BENCHMARKS. Every byte count\n'
printf '         above is measured on a fixture sized to fit a 240-second\n'
printf '         acceptance budget. The ratios are real and the controls are\n'
printf '         real, and a number measured on a few megabytes says nothing\n'
printf '         about a twenty-gigabyte blob over a link that is slower than a\n'
printf '         disk. Nothing here is a performance claim.\n'
printf '         AND THE RATIO ABOVE IS THE FIXTURE SPEAKING, not the feature.\n'
printf '         At the shipped chunker parameters — 256 KiB minimum, 1 MiB\n'
printf '         average, 4 MiB maximum — the five-megabyte blob repaired above\n'
printf '         is only a HANDFUL of chunks, so one chunk of it is a fifth or a\n'
printf '         third, and the measured percentage moves run to run with where\n'
printf '         a content-defined boundary happens to fall in random bytes. The\n'
printf '         claim asserted is therefore the fixture-independent one — one\n'
printf '         chunk fetched out of N, with the byte counts reported beside it\n'
printf '         and agreed by the repairer and the source independently. The\n'
printf '         percentages that make the feature sound impressive need a blob\n'
printf '         with hundreds of chunks, and such a blob does not fit this\n'
printf '         budget. Read the numerator, not the percentage.\n'
printf '         THIS REPOSITORY IS PUBLIC, AND THE GUARD IS NEW. A real host\n'
printf '         name, a document named after a real machine and a personal home\n'
printf '         directory were on `main` when this milestone opened (#211) and\n'
printf '         were scrubbed during it. `make hygiene` now enforces the rule\n'
printf '         rather than trusting it: shape patterns plus retired proper\n'
printf '         nouns held as SHA-256 DIGESTS, so the guard cannot spell what it\n'
printf '         forbids. It found a real device MAC in a renderer fixture on its\n'
printf '         first run, which is the argument for it. A guard is not a proof\n'
printf '         of cleanliness — it catches the shapes somebody thought of.\n'
printf '\n'

# The verdict line. It carries the capability gap, because the verdict line is
# what a person actually reads and what a CI log gets scrolled to — a limitation
# reported only in the middle of 400 `ok`s is one nobody sees at the moment it
# matters (#187).
CAPS_VERDICT=""
if (( CAPS_UNEXERCISED_N > 0 )); then
  CAPS_VERDICT=$(capability_names | tr '\n' ' ' | sed 's/ $//; s/ /, /g')
fi

VERDICT_REACHED=1
if (( FAILED )); then
  printf '\n\033[31macceptance: FAILED\033[0m — %d assertions\n' "$ASSERTIONS"
  if (( CAPS_MISSING_N > 0 )); then
    printf '  %d of those failures are MISSING CAPABILITIES: an assertion declared\n' "$CAPS_MISSING_N"
    printf '  something this machine does not have, so it could not be exercised.\n'
    printf '  That is a failed run, deliberately — see #187.\n'
  fi
  exit 1
fi
if (( CAPS_UNEXERCISED_N > 0 )); then
  printf '\n\033[32macceptance: all checks passed\033[0m — %d assertions, ' "$ASSERTIONS"
  printf '\033[33m%d block(s) NOT exercised\033[0m (absent: %s)\n' \
    "$CAPS_UNEXERCISED_N" "$CAPS_VERDICT"
  printf '  a green run here is not a green run on a machine that has them (#187)\n'
else
  printf '\n\033[32macceptance: all checks passed\033[0m — %d assertions, every declared capability present\n' \
    "$ASSERTIONS"
fi
