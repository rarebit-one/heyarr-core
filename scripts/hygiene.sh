#!/usr/bin/env bash
# Fail if any tracked file contains a string from scripts/hygiene.denylist.
#
# The shape is deliberately the same as the org's no-secrets lint: a grep over
# tracked files against a small, commented deny-list. See the deny-list itself
# for what belongs in it and why (#147, #211).
#
# Usage: scripts/hygiene.sh [denylist-path]
set -euo pipefail

cd "$(dirname "$0")/.."
denylist="${1:-scripts/hygiene.denylist}"

if [ ! -f "$denylist" ]; then
	echo "hygiene: no deny-list at $denylist" >&2
	exit 2
fi

# The deny-list and this script quote the forbidden strings by definition, so
# scanning them would fail every run.
exclusions=(':(exclude)scripts/hygiene.sh' ':(exclude)scripts/hygiene.denylist')

status=0
patterns=0
while IFS= read -r pattern; do
	case "$pattern" in
	'' | '#'*) continue ;;
	esac
	patterns=$((patterns + 1))
	# -I skips binary files; -n reports the line; -i because the deny-list is
	# written in the plain form.
	if hits=$(git grep -I -n -i -E -- "$pattern" -- . "${exclusions[@]}"); then
		status=1
		echo "hygiene: FORBIDDEN pattern /$pattern/ appears in tracked files:" >&2
		echo "$hits" | sed 's/^/  /' >&2
	fi
done <"$denylist"

if [ "$patterns" -eq 0 ]; then
	echo "hygiene: deny-list is empty — that is almost certainly a mistake" >&2
	exit 2
fi

if [ "$status" -ne 0 ]; then
	cat >&2 <<'MSG'

This repo is public. Replace the name with a placeholder that describes the
SHAPE of the thing rather than its identity — `Site A` / `peer-a` for premises,
`the reference host` / `<host>` for machines, `/srv/media/...` for library
paths. If a match is a genuine false positive, narrow the pattern in
scripts/hygiene.denylist and say why in the commit message.
MSG
	exit 1
fi

echo "hygiene: $patterns deny-list patterns, no matches in tracked files"
