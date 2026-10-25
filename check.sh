#!/usr/bin/env bash
# Run from any directory after npm --prefix client ci.
set -euo pipefail
project_root="$(cd "$(dirname "$0")" && pwd)"
check_dir="$(mktemp -d "${TMPDIR:-/tmp}/minicoco-check.XXXXXX")"
trap 'rm -rf "$check_dir"' EXIT
bash "$project_root/check_default_deps.sh"
(
    cd "$project_root/backend"
    MINICOCO_MODEL_IS_FUNCTION_CALL=true MINICOCO_DEBUG_PROMPT=0 go test -race -cover ./...
    go vet ./...
    go build -o "$check_dir/backend" .
)
npm --prefix "$project_root/client" run typecheck
npm --prefix "$project_root/client" run selftest
npm --prefix "$project_root/client" test
