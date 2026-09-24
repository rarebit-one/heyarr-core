#!/usr/bin/env bash
# Decide whether this CI run needs the full operating-system matrix.
#
# ci.yml's `plan` job runs this and hands the answer to the jobs whose matrix
# has macOS or Windows legs (test, chunking-determinism, acceptance). Push to
# main, the nightly schedule and a manual dispatch always get the full matrix.
# A pull request gets Linux only, unless something in it is platform-sensitive.
#
# Why this is safe enough to be the default: over 30 days (778 ci.yml runs), a
# macOS or Windows leg failed while its Linux counterpart passed 25 times. Two
# of those were real platform bugs, and BOTH would have tripped a rule below —
# a bash 3.2 construct in scripts/acceptance.sh (scripts/), and a
# syscall.Stat_t reference in a package that already has *_unix.go files (a
# platform-sensitive directory, and a syscall. line in the patch). The other 23
# were flakes: the macOS demo overrunning its 300s budget, the macOS acceptance
# job exiting non-zero after a green verdict, and two timing-sensitive unit
# tests. What a PR gives up is paid back on the push to main, which still runs
# everything.
#
# The rules, in order. The first that matches decides, and says why:
#   1. the event is not a pull request                          -> full
#   2. the PR carries the `ci:full-matrix` label                -> full
#      (read live from the API, so "Re-run all jobs" after adding it works)
#   3. the PR is too large to list its files (> 3000)           -> full
#   4. a changed path is build- or CI-level: go.mod, go.sum, Makefile,
#      .github/workflows/ci.yml, .github/actions/, scripts/, or the chunking
#      package (the one whose output must match across platforms)  -> full
#   5. a changed file is platform-suffixed (*_darwin.go, *_unix.go, ...) -> full
#   6. a changed Go file's diff adds or removes a build constraint, a
#      runtime.GOOS check, a syscall. reference or golang.org/x/sys  -> full
#   7. a changed Go file has no diff to inspect (too large, binary)  -> full
#   8. a changed Go file lives in a package that already has platform-specific
#      files or build constraints                                     -> full
#   otherwise                                                         -> Linux only
#
# Anything this script cannot determine fails TOWARDS the full matrix, never
# away from it: a missed macOS leg is the costly mistake, an extra one is not.
#
# Inputs (env): EVENT_NAME, REPO, PR_NUMBER, GH_TOKEN, GITHUB_OUTPUT.
# Output: full=true|false and reason=<text> on $GITHUB_OUTPUT.
set -euo pipefail

cd "$(dirname "$0")/.."

out="${GITHUB_OUTPUT:-/dev/stdout}"

decide() {
	echo "full=$1" >>"$out"
	echo "reason=$2" >>"$out"
	if [ "$1" = true ]; then
		echo "ci-matrix: FULL matrix (linux, macos, windows) — $2"
	else
		echo "ci-matrix: linux-only matrix — $2"
	fi
	if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
		echo "**OS matrix:** full=$1 — $2" >>"$GITHUB_STEP_SUMMARY"
	fi
	exit 0
}

if [ "${EVENT_NAME:-}" != pull_request ]; then
	decide true "event is ${EVENT_NAME:-unknown}, which always runs the full matrix"
fi

if [ -z "${PR_NUMBER:-}" ] || [ -z "${REPO:-}" ]; then
	decide true "no pull request number or repository to inspect"
fi

if ! pr=$(gh api "repos/$REPO/pulls/$PR_NUMBER" --jq '[([.labels[].name] | index("ci:full-matrix") != null), .changed_files] | @tsv'); then
	decide true "could not read pull request #$PR_NUMBER from the API"
fi
labelled=$(cut -f1 <<<"$pr")
changed=$(cut -f2 <<<"$pr")

if [ "$labelled" = true ]; then
	decide true "the pull request is labelled ci:full-matrix"
fi
if ! [ "$changed" -le 3000 ] 2>/dev/null; then
	decide true "the pull request changes $changed files, more than the API will list"
fi

# One line per file: <filename> TAB <platform-line-in-patch|clean|nopatch>.
# The patch field has no ---/+++ headers, so ^[+-] is exactly the changed lines.
platform_re='//go:build|// \\+build|runtime\\.GOOS|syscall\\.|golang\\.org/x/sys'
if ! files=$(gh api --paginate "repos/$REPO/pulls/$PR_NUMBER/files?per_page=100" --jq "
	.[] | [
		.filename,
		(if .patch == null then \"nopatch\"
		 elif ([.patch | split(\"\n\")[] | select(test(\"^[+-]\")) | select(test(\"$platform_re\"))] | length) > 0 then \"platform\"
		 else \"clean\" end)
	] | @tsv"); then
	decide true "could not list the files changed by pull request #$PR_NUMBER"
fi
if [ -z "$files" ]; then
	decide true "the pull request lists no changed files"
fi

platform_suffix_re='_(darwin|windows|unix|linux|bsd|freebsd|netbsd|openbsd|other|posix)(_test)?\.go$'

# A package is platform-sensitive if it already splits code by platform.
platform_dir() {
	local dir=$1 f
	[ -d "$dir" ] || return 1
	for f in "$dir"/*.go; do
		[ -e "$f" ] || continue
		if [[ $(basename "$f") =~ $platform_suffix_re ]]; then
			return 0
		fi
		if grep -qE '^//go:build|^// \+build' "$f"; then
			return 0
		fi
	done
	return 1
}

while IFS=$'\t' read -r path patch; do
	[ -n "$path" ] || continue
	case "$path" in
	go.mod | go.sum | Makefile | .github/workflows/ci.yml | .github/actions/* | scripts/* | internal/storagefabric/chunking/*)
		decide true "$path is build-, CI- or platform-contract-level"
		;;
	esac
	case "$path" in
	*.go) ;;
	*) continue ;;
	esac
	if [[ $path =~ $platform_suffix_re ]]; then
		decide true "$path is platform-specific by its name"
	fi
	if [ "$patch" = platform ]; then
		decide true "$path changes a build constraint, runtime.GOOS, syscall or x/sys line"
	fi
	if [ "$patch" = nopatch ]; then
		decide true "$path has no diff the API will show, so it cannot be ruled out"
	fi
	if platform_dir "$(dirname "$path")"; then
		decide true "$path is in $(dirname "$path"), which has platform-specific files"
	fi
done <<<"$files"

decide false "no changed file is platform-sensitive (label the PR ci:full-matrix to force the full matrix)"
