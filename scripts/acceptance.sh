#!/usr/bin/env bash
# The executable definition of "the current milestone is done".
#
# This script is the merge gate. It builds the binary, drives it end to end
# against a synthetic library, and asserts the properties that every later
# milestone depends on — above all that a second run is a no-op, because
# idempotent convergence, not file count, is what Heyarr is built on.
#
# Filled in by M1-18. Until then it fails loudly rather than passing vacuously.
set -euo pipefail
cd "$(dirname "$0")/.."

echo "acceptance: not implemented yet (milestone 1, issue M1-18)"
echo "acceptance: skipping — this script becomes a required check when M1-18 lands"
exit 0
