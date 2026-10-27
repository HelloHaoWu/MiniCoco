#!/usr/bin/env bash
set -euo pipefail
project_root="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$project_root/backend"
# Isolated local provider fixtures only; no .env load or real API calls.
# -json is available via GO_TEST_JSON=1 for a machine-readable test event stream.
if [[ "${GO_TEST_JSON:-0}" == 1 ]]; then
  go test -json -count=1 -run '^TestProactive' ./...
else
  go test -v -count=1 -run '^TestProactive' ./...
fi
