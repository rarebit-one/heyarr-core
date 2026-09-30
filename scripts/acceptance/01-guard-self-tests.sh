# shellcheck shell=bash
# A section of scripts/acceptance.sh: sourced by it, in order, never run alone.
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

