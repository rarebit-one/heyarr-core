#!/usr/bin/env bash
# Regenerate all committed generated code. CI runs this and then asserts
# `git diff --exit-code`, so generated output can never drift from its source.
set -euo pipefail
cd "$(dirname "$0")/.."

# sqlc — generates internal/persistence/sqlite/gen from queries/ + migrations/.
# Milestone 1 (M1-03) introduces sqlc.yaml; until then there is nothing to do.
if [[ -f sqlc.yaml ]]; then
  sqlc generate
fi

# CLI reference docs, generated from the cobra command tree (M1-02).
if [[ -d docs/cli ]] && command -v go >/dev/null; then
  :
fi

echo "generate: ok"
