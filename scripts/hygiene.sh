#!/usr/bin/env bash
# Fail if a tracked file names a real machine, network, premises or person.
#
# The entry point stays a shell script so `make hygiene` and the CI job match
# the org's other lints. The work is in scripts/hygiene.py, which holds the
# reasoning; scripts/hygiene.denylist and scripts/hygiene.digests hold the two
# lists and explain why they are two.
#
# python3 is the one dependency, and it is required rather than optional: this
# job passing because an interpreter was missing is the failure mode a hygiene
# guard can least afford.
set -euo pipefail

cd "$(dirname "$0")/.."

if ! command -v python3 >/dev/null 2>&1; then
	echo "hygiene: python3 is required and was not found — refusing to pass" >&2
	exit 2
fi

exec python3 scripts/hygiene.py "$@"
