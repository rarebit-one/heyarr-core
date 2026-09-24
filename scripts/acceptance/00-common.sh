# shellcheck shell=bash
# A section of scripts/acceptance.sh: sourced by it, in order, never run alone.

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
# FAILURES records every failure in the order it happened, so the verdict can
# print them back.
#
# Without it the verdict says only "FAILED — N assertions", and the failures
# themselves are wherever they happened — often thousands of lines up. Anyone
# reading the TAIL of a CI log, which is what a person checking a red check and
# every tool that greps one actually reads, sees the LAST failure and infers it
# is the fault.
#
# That is not hypothetical: it cost a misdiagnosis. A run failed with six
# assertion failures followed by "the demo took 288s, past its 240s budget",
# and the overrun — a CONSEQUENCE of the six, since a wait that never lands
# runs to its full timeout — was read as the whole story (#274).
#
# The first failure is nearly always the cause and the rest the consequences,
# so they are printed in order and the first is called out.
FAILURES=()
fail() {
  ASSERTIONS=$(( ASSERTIONS + 1 ))
  printf '  \033[31mFAIL\033[0m %s\n' "$1"
  FAILURES+=("$1")
  FAILED=1
}
note() { printf '\n\033[1m%s\033[0m\n' "$1"; }

# ps_enrol_device pins a device as an enrolled recipient so a space key may be
# wrapped for it (enrol-before-wrap, ADR-0049): it generates a user identity in
# the device's own dir, self-signs the device's cert (binding its X25519
# encryption key), and enrols both on the node at sock. Args: sock token dir name.
ps_enrol_device() {
  local sock="$1" token="$2" dir="$3" nm="$4" cl uk cert
  cl=( env "VOIDBIND_IDENTITY_DIR=$dir" "VOIDBIND_DEVICE_DIR=$dir" "$BIN" )
  "${cl[@]}" identity generate --name "$nm" >/dev/null 2>&1
  "${cl[@]}" identity enrol >/dev/null 2>&1
  uk=$("${cl[@]}" identity show --json | jq -r .public_key)
  cert=$("${cl[@]}" identity credential | cut -d'~' -f1)
  curl -sS --unix-socket "$sock" -H "Authorization: Bearer $token" -X POST \
    -H 'Content-Type: application/json' -d "{\"public_key\":\"$uk\",\"name\":\"$nm\"}" \
    -o /dev/null "http://heyarr/api/v1/identities/users"
  curl -sS --unix-socket "$sock" -H "Authorization: Bearer $token" -X POST \
    -H 'Content-Type: application/json' -d "{\"cert\":\"$cert\",\"name\":\"$nm\"}" \
    -o /dev/null "http://heyarr/api/v1/identities/devices"
}

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

