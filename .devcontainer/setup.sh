#!/usr/bin/env bash
# Container setup: dependencies, plus the pinned media toolchain.
#
# The container exists for one reason (ADR-0023): FFmpeg is the first
# dependency Heyarr cannot ship inside its own binary, and a Linux container is
# where a developer on any host gets the SAME FFmpeg that CI uses. The pin
# differs between Linux and macOS — see scripts/toolchain.lock — so working in
# here is how a macOS developer reproduces a probe result that CI disagrees
# with.
#
# It installs nothing that scripts/toolchain.sh does not, deliberately: two
# places that install FFmpeg is two versions waiting to disagree.
set -euo pipefail
cd "$(dirname "$0")/.."

go mod download
./scripts/toolchain.sh >/dev/null
echo "toolchain: $(./.toolchain/bin/ffprobe -version | head -1)"
