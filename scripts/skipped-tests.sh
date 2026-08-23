#!/usr/bin/env bash
# Which tests SKIPPED, and why (#187).
#
# `go test ./...` prints one line per package. A package whose only meaningful
# test called t.Skip prints `ok` — the same word, in the same column, as a
# package that ran everything. That is not a display quirk; it is how
# "TestProbingCostIsBoundedAndDoesNotScaleWithBlobSize passes locally" came to be
# reported about a test that had never executed (#157). ffprobe is absent on the
# development machines, the test skips, the package prints `ok`, and the summary
# line is indistinguishable from coverage.
#
# Skipping is a supported state here — ADR-0023 makes the media toolchain
# optional, and several tests correctly decline to run without a filesystem
# feature, a captured corpus or a platform. What is not supported is skipping
# INVISIBLY. So this target makes the skip the loud thing:
#
#   * every skipped test, with the reason it gave, grouped by package;
#   * and, called out separately, any package where EVERY test skipped while the
#     package still reported `ok`. That is the #157 shape exactly, and it is the
#     only one of the two that is usually a bug.
#
# It exits 0 whether or not anything skipped. This reports; it does not gate.
# The gate for "a capability was absent and the assertion was therefore vacuous"
# is scripts/acceptance.sh's require_capability, on the other side of the fence.
set -euo pipefail
cd "$(dirname "$0")/.."

if ! command -v jq >/dev/null 2>&1; then
  echo "skipped-tests: jq is not installed; this target reads 'go test -json'" >&2
  exit 1
fi

JSON=$(mktemp)
trap 'rm -f "$JSON"' EXIT

# -count=1 so a cached PASS does not hide a skip that a fresh run would report:
# the cache replays the old output, and a package cached from a machine that had
# the toolchain would report no skips on one that does not.
#
# `|| true` because a genuinely failing test is not this script's business to
# swallow OR to report — `make test` is the gate. This one still has something
# useful to say about the packages that did run.
go test -json -count=1 "${@:-./...}" >"$JSON" 2>/dev/null || true

# One row per skipped test: package<TAB>test<TAB>reason.
#
# The reason is the test's own output. `go test -json` emits it as output events
# on the test, and the skip message is the line ending in the file:line prefix
# that testing.go writes — so take the last non-empty output line before the
# skip action, with the file:line prefix stripped.
SKIPS=$(jq -rs '
  [ .[] | select(.Test != null) ] as $ev
  | [ $ev[] | select(.Action == "skip") | {pkg: .Package, test: .Test} ] as $skipped
  | [ $skipped[]
      | . as $s
      | ( [ $ev[]
            | select(.Package == $s.pkg and .Test == $s.test and .Action == "output")
            | .Output
            | select(test("^\\s*[^\\s]+\\.go:[0-9]+:"))
            | sub("^\\s*[^\\s]+\\.go:[0-9]+:\\s*"; "")
            | rtrimstr("\n")
          ] | map(select(length > 0)) | last // "no reason given"
        ) as $why
      | "\($s.pkg)\t\($s.test)\t\($why)"
    ]
  | .[]' "$JSON" | sort)

# How much of each package skipped. The ratio is the actionable number: five of
# seven is a toolchain that is missing, seven of seven is a package that
# reported `ok` having run nothing.
STATS=$(jq -rs '
  [ .[] | select(.Test != null and (.Action == "pass" or .Action == "fail" or .Action == "skip")) ]
  | group_by(.Package)
  | map("\(.[0].Package)\t\(map(select(.Action == "skip")) | length)\t\(length)")
  | .[]' "$JSON" | sort)

# Packages where EVERY test that reported a result skipped, and which still said
# `ok`. The dangerous shape, and the one worth a colour.
VACUOUS=$(printf '%s\n' "$STATS" | awk -F'\t' '$2 == $3 && $3 > 0 { print $1 }')

TOTAL=$(printf '%s' "$SKIPS" | grep -c . || true)
PKGS=$(printf '%s' "$SKIPS" | cut -f1 | sort -u | grep -c . || true)

printf '\n\033[1mskipped tests, and why\033[0m\n\n'
if [[ -z "$SKIPS" ]]; then
  printf '  nothing skipped: every test in every package ran.\n\n'
  exit 0
fi

LAST=""
ratio=""
while IFS=$'\t' read -r pkg test why; do
  [[ -z "$pkg" ]] && continue
  if [[ "$pkg" != "$LAST" ]]; then
    ratio=$(printf '%s\n' "$STATS" | awk -F'\t' -v p="$pkg" '$1 == p { printf "%s of %s", $2, $3 }')
    printf '  %s  \033[2m(%s skipped)\033[0m\n' "$pkg" "${ratio:-?}"
    LAST=$pkg
  fi
  printf '    \033[33mSKIP\033[0m %-58s %s\n' "$test" "$why"
done <<<"$SKIPS"

printf '\n  %s test(s) skipped across %s package(s).\n' "$TOTAL" "$PKGS"

if [[ -n "$VACUOUS" ]]; then
  printf '\n  \033[31mEVERY test skipped in these packages, and each still reported `ok`:\033[0m\n'
  printf '    %s\n' $VACUOUS
  printf '  A package-level `ok` here means nothing ran. This is the #157 shape:\n'
  printf '  install what they need (scripts/toolchain.sh for the media toolchain)\n'
  printf '  before reporting that anything in them passes.\n'
fi
printf '\n'
