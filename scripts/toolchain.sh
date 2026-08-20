#!/usr/bin/env bash
# Install the pinned FFmpeg toolchain (ADR-0023).
#
# Reads scripts/toolchain.lock, downloads the entries for this platform,
# verifies each against its committed SHA-256, and leaves ffmpeg and ffprobe in
# a directory it prints on stdout. Idempotent: an already-installed, correct
# copy is left alone, so this is cheap to call from every CI job.
#
# The verification is the reason this exists rather than a `curl | tar`. A
# GitHub release asset can be replaced by its publisher after the fact, and the
# only difference between "we noticed" and "we did not" is a digest that was
# written down beforehand.
#
# Usage:
#   scripts/toolchain.sh              # install, print the bin directory
#   scripts/toolchain.sh --check      # verify an existing install, install nothing
#   TOOLCHAIN_DIR=/opt/x scripts/toolchain.sh
set -euo pipefail
cd "$(dirname "$0")/.."

LOCK=scripts/toolchain.lock
DIR=${TOOLCHAIN_DIR:-$PWD/.toolchain}
BIN=$DIR/bin
CHECK_ONLY=0
[ "${1:-}" = "--check" ] && CHECK_ONLY=1

die() { echo "toolchain: $*" >&2; exit 1; }

# Platform names match Go's, because everything else in this repository does and
# a second naming scheme is a second place to be wrong.
case "$(uname -s)" in
  Linux)  GOOS=linux ;;
  Darwin) GOOS=darwin ;;
  *) die "unsupported operating system: $(uname -s)" ;;
esac
case "$(uname -m)" in
  x86_64|amd64) GOARCH=amd64 ;;
  arm64|aarch64) GOARCH=arm64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac

[ -f "$LOCK" ] || die "$LOCK is missing"

# sha256 of a file, on either platform. Two tools, one answer.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

# verify_installed reports whether the binary at $1 exists and reports version
# $2. It checks the version rather than only presence: a leftover install from
# an older pin is exactly the drift this file exists to prevent, and it looks
# identical to a correct one until something behaves differently.
# The comparison is EXACT, on the parsed field, and not a substring match on
# the banner. `grep -F " version 7.0.2"` also matches " version 7.0.2-static",
# so a substring check accepts a binary the lock does not describe — which is
# the whole thing this file exists to prevent, and which is exactly what the
# first version of this function did.
verify_installed() {
  local path=$1 version=$2 reported
  [ -x "$path" ] || return 1
  reported=$("$path" -version 2>/dev/null | head -1 | awk '{print $3}')
  [ "$reported" = "$version" ] || return 1
  return 0
}

install_one() {
  local tool=$1 key="$1-$GOOS-$GOARCH" line version want url tmp got
  line=$(awk -v k="$key" '$1 == k { print; exit }' "$LOCK")
  [ -n "$line" ] || die "$LOCK has no entry for $key"
  version=$(echo "$line" | awk '{print $2}')
  want=$(echo "$line" | awk '{print $3}')
  url=$(echo "$line" | awk '{print $4}')

  if verify_installed "$BIN/$tool" "$version"; then
    return 0
  fi
  [ "$CHECK_ONLY" -eq 1 ] && die "$tool is not installed at version $version"

  mkdir -p "$BIN"
  tmp=$(mktemp)
  trap 'rm -f "$tmp"' RETURN
  echo "toolchain: fetching $tool $version for $GOOS/$GOARCH" >&2
  curl -fsSL --retry 3 -o "$tmp" "$url" || die "downloading $url failed"

  got=$(sha256_of "$tmp")
  if [ "$got" != "$want" ]; then
    # Loud and specific. A checksum mismatch is either a corrupted download or
    # a replaced asset, and the two need different responses from a human.
    die "$tool: sha256 mismatch
  expected $want
  got      $got
  from     $url
This is either a corrupt download or a changed release asset. Do not update the
lock file to match without establishing which."
  fi

  gunzip -c "$tmp" > "$BIN/$tool.part"
  chmod +x "$BIN/$tool.part"
  mv "$BIN/$tool.part" "$BIN/$tool"

  verify_installed "$BIN/$tool" "$version" ||
    die "$tool installed but does not report version $version — the pinned
digest and the pinned version disagree, which means the lock file is wrong
about one of them"
}

install_one ffmpeg
install_one ffprobe

echo "$BIN"
