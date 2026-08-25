#!/usr/bin/env bash
# Run the acceptance demo and check what it printed against scripts/claims.list.
#
# The entry point stays a shell script so `make claims` and the CI job match the
# shape of `make hygiene`. The reasoning is in scripts/claims.list; the
# mechanism is in scripts/claims.py.
#
# It RUNS the demo rather than taking a transcript, so that the thing checked is
# the run that just happened. A transcript passed in from somewhere else is a
# transcript that can be stale, and a guard against "this was never exercised"
# must not be satisfiable by a file from last week.
#
# scripts/acceptance.sh is deliberately untouched by all of this. That file is
# single-owner while a branch is working on it — its own header says so — and a
# guard that made every feature branch edit it would cause the conflicts it was
# meant to prevent.
#
# python3 is required rather than optional, for the same reason hygiene.sh
# requires it: this job passing because an interpreter was missing is the
# failure mode a guard can least afford.
set -euo pipefail

cd "$(dirname "$0")/.."

if ! command -v python3 >/dev/null 2>&1; then
	echo "claims: python3 is required and was not found — refusing to pass" >&2
	exit 2
fi

TRANSCRIPT=${CLAIMS_TRANSCRIPT:-$(mktemp -t heyarr-demo)}
trap 'rm -f "$TRANSCRIPT"' EXIT

# The demo's exit status is what gates the build, and `tee` would replace it
# with tee's, so it is captured explicitly from PIPESTATUS.
set +e
./scripts/acceptance.sh 2>&1 | tee "$TRANSCRIPT"
DEMO_STATUS=${PIPESTATUS[0]}
set -e

# A failed demo short-circuits, and the first version of this file did not —
# which its own first run demonstrated. The demo refused to start because the
# binary was not built, and the ledger then reported EVERY proven claim as
# missing: seven paragraphs of alarming and entirely correct output, none of
# which was the problem. A ledger check against a run that did not happen says
# nothing about the code, and burying one real line under seven false ones is
# how a guard teaches people to skim it.
if [ "$DEMO_STATUS" -ne 0 ]; then
	echo
	echo "claims: the demo failed (status $DEMO_STATUS), so there is nothing to check the" >&2
	echo "        ledger against. Read the demo's verdict above — #277 makes it list every" >&2
	echo "        failure in order, so read DOWN from the first one rather than up from the" >&2
	echo "        last. The claims ledger is not the finding here." >&2
	exit "$DEMO_STATUS"
fi

echo
echo "---------------------------------------------------------------------------"
# `set -e` would abandon the script the moment python reports a finding, before
# anything below could run or explain it. The status is taken deliberately.
set +e
python3 scripts/claims.py "$TRANSCRIPT"
CLAIMS_STATUS=$?
set -e
exit "$CLAIMS_STATUS"
