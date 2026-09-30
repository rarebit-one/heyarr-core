#!/usr/bin/env bash
# Regenerate all committed generated code. CI runs this and then asserts
# `git diff --exit-code`, so generated output can never drift from its source.
set -euo pipefail
cd "$(dirname "$0")/.."

# CLI reference docs, generated from the cobra command tree (M1-17).
#
# The generator removes pages no command produces any more, which the diff
# check below cannot do on its own: a deleted command's page is still committed
# and still unchanged, so `git diff` would never mention it.
go run ./internal/tools/gendocs docs/cli

echo "generate: ok"
