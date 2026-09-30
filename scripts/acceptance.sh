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

# The rest of this script lives in scripts/acceptance/, one file per section,
# sourced below in the order the sections have always run in. They are sourced,
# not executed: every one shares this shell — its `set -euo pipefail`, the EXIT
# trap, the globals, and the working directory set just above (the repository
# root), which is what the relative paths below rely on. The driver that calls
# the demo sections stays in this file, so it still reads as the order the demo
# runs in.
#
# Two rules the split adds, both because of how `.` meets `set -e`:
#   - A section file must END on a command that exits 0. `.` returns the status
#     of the last command it ran, so a section ending on, say, `[[ ... ]] && x`
#     that is false inline carries on, but sourced it ends the run right here.
#     Every section today ends on a function definition, a pass/fail/assert, or
#     an if/fi.
#   - Never `. file || ...` or `if . file`: that suspends `set -e` for everything
#     the file runs, and a failing command inside it would stop nothing.

. scripts/acceptance/00-common.sh
. scripts/acceptance/01-guard-self-tests.sh
. scripts/acceptance/02-m1-node-lifecycle.sh
. scripts/acceptance/03-m4-peer-identity.sh
. scripts/acceptance/04-m1-ingest-and-scan.sh
. scripts/acceptance/05-m1-full-library.sh
. scripts/acceptance/06-provider-health-beat.sh
. scripts/acceptance/07-polled-acquisition.sh
. scripts/acceptance/08-m4-peer-helpers.sh
. scripts/acceptance/09-vault-placement.sh
. scripts/acceptance/10-m4-two-peer.sh
. scripts/acceptance/11-m6-swarm.sh
. scripts/acceptance/12-split-process.sh
. scripts/acceptance/13-m8-device-identity.sh
. scripts/acceptance/14-m8-personal-state.sh
. scripts/acceptance/15-subtitle-extraction.sh

# The gate is only a gate if people run it, and people stop running a gate
# they have to wait for. Milestone 1 finished at about fifteen seconds and 100
# assertions; Milestone 2 adds probing, remuxing and a second worker process,
# all of which cost real time.
#
# The budget is generous rather than tight — a loaded CI runner is slower than
# a laptop and this must not be flaky — but it exists so that a change which
# doubles the runtime is noticed by CI rather than by whoever stops running
# `make demo` six weeks later.
# Raised from 240 to 300 on 2026-08-25, deliberately, which is what the failure
# message below asks for.
#
# #247's polled acquisition arc costs about thirty-five seconds and cannot cost
# much less. It brings up a node of its own — it must, because the full demo's
# only download client refuses connections ON PURPOSE and giving that node a
# working one would delete the ADR-0025 section that proves a client being down
# is a degraded state rather than a failure. The search is forced rather than
# waited for, which already took thirty seconds off it. What remains is the
# download poll interval: fifteen seconds is the beat's own cadence and the
# section deliberately does not reach into it, because a demo that special-cased
# the interval would stop proving the interval anybody actually runs.
#
# Measured on this machine, equipped, verdict line each time: 196s before the
# section, 263s with it and the search beat waited out, and 233s with the search
# forced. macOS is the tight runner class — M6 measured it at 205-213s against
# 240 — so 300 restores roughly the margin that existed before, rather than
# buying new room.
#
# By 2026-09 the margin was gone again, on BOTH runner classes rather than only
# macOS. Twenty-four CI verdict lines: ubuntu-latest 288-294s, macos-latest
# 285-306s, with macOS the one that crossed 300 (301, 301, 306). The runners
# were not the difference — the demo had grown. The answer this time was to
# take time out rather than add budget: two waits that proved nothing on the
# happy path (a 34-42s wait for a health pass that #164's section already
# procures and asserts, and four event-log replays that each sat out a
# five-second --max-time; see events_replay) came out, and the budget stayed.
#
# Later in 2026-09 a search stopped asking indexers one after another. The
# demo's two real-but-refusing indexers (acceptance-torznab, -newznab) each
# spend about six seconds in retry backoff per search, and they used to be
# paid serially; asked concurrently, they cost one backoff rather than two.
# CI verdict lines went from ubuntu 237-241s / macOS 239-255s to 208s / 227s.
# The budget stayed at 300 deliberately: that is margin regained, not room to
# spend.
DEMO_BUDGET_SECONDS=${DEMO_BUDGET_SECONDS:-300}
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
  note "THE POLLED ACQUISITION ARC: a grab that succeeds (§58, §65, #247)"
  polled_acquisition_demo
  note "THE DEVICE AUTHENTICATES AS ITS USER (§40, ADR-0048, ADR-0032, #303)"
  device_auth_demo
  note "DEVICE PAIRING: an old device authorises a new one over a dumb relay (§40, ADR-0022, ADR-0038, #305)"
  pairing_demo
  note "IDENTITY RECOVERY: the secret reconstructs the identity offline (§79, ADR-0022, #306)"
  recovery_demo
  note "ENCRYPTED PERSONAL STATE: the peer stores ciphertext it cannot read (§38, §42, ADR-0049, #320)"
  personalstate_demo
  note "THE DEVICE GATEWAY: a stock Subsonic client reads playlists off the DEVICE, the controller holds ciphertext (§73, ADR-0051, #387)"
  gateway_demo
  note "CONVERGE AFTER A PARTITION: two devices, an offline concurrent edit each, merged client-side (§42, §43, ADR-0049, #324)"
  converge_after_partition_demo
  note "SNAPSHOTS BOUND THE LOG: snapshot + compaction, and the state survives (§44, ADR-0049, #325)"
  snapshot_demo
  note "REVOCATION CUTS ACCESS: a device is revoked by rotating the space key (§41, ADR-0022, ADR-0049, #361)"
  revocation_demo
  note "THE VAULT PLACEMENT PIN: a vault blob replicates and is retained by a pin, not an asset (ADR-0096, #540)"
  vault_placement_demo
  note "THE SECOND PEER: placement, proven (§56, §64, M4-11) — heyarr all"
  two_peer_demo all
  note "THE SECOND PEER, again, as separate role processes (ADR-0002, M4-16)"
  two_peer_demo split
  note "THE SWARM: two peers converge (§23, §24, §33, M6-06)"
  swarm_demo
  note "split-process mode, end to end (ADR-0002)"
  split_process_demo
  note "THE EMBEDDED SUBTITLE: an in-container track lifted into its own asset (§66, ADR-0084, #490)"
  subtitle_extraction_demo
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

. scripts/acceptance/16-epilogue-and-verdict.sh
